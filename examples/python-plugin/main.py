"""A monitor-hub plugin in Python: every Check records a "ping" event in SQLite.

It follows docs/protocol.md and uses only grpcio, protobuf and the standard library.
"""

import contextlib
import json
import os
import secrets
import signal
import sqlite3
import sys
import threading
import time
from collections.abc import Generator
from concurrent import futures
from pathlib import Path
from types import FrameType
from typing import override

import grpc
from google.protobuf import empty_pb2, wrappers_pb2
from google.protobuf.timestamp_pb2 import Timestamp

sys.path.insert(0, str(Path(__file__).parent / "gen"))
import plugin_pb2 as pb  # noqa: E402
import plugin_pb2_grpc as pb_grpc  # noqa: E402

COOKIE = "7c0e5a52-monitor-hub"
NAME = "example-python"
DEFAULT_LIMIT = 100
SERVING = 1

SCHEMA = """
CREATE TABLE IF NOT EXISTS events (
    seq INTEGER PRIMARY KEY AUTOINCREMENT,
    id TEXT NOT NULL UNIQUE,
    occurred_ms INTEGER NOT NULL,
    title TEXT NOT NULL
)
"""
ADD_PING = """
INSERT INTO events (id, occurred_ms, title)
SELECT 'ping-' || n, ?, 'ping ' || n
FROM (SELECT COALESCE((SELECT seq FROM sqlite_sequence WHERE name = 'events'), 0) + 1 AS n)
"""
ADD_FIRST_PING = ADD_PING + " WHERE NOT EXISTS (SELECT 1 FROM sqlite_sequence WHERE name = 'events')"


def log(level: str, message: str, **fields: object) -> None:
    line = {"@level": level, "@message": message, "@module": NAME, **fields}
    print(json.dumps(line), file=sys.stderr, flush=True)


class StorageError(Exception):
    pass


class Store:
    def __init__(self, path: Path) -> None:
        self._path = path

    @contextlib.contextmanager
    def _open(self) -> Generator[sqlite3.Connection]:
        try:
            with contextlib.closing(sqlite3.connect(self._path, timeout=5)) as conn:
                conn.execute(SCHEMA)
                with conn:
                    conn.execute(ADD_FIRST_PING, (now_ms(),))
                yield conn
        except sqlite3.Error as err:
            raise StorageError(str(err)) from err

    def add_ping(self) -> None:
        with self._open() as conn, conn:
            conn.execute(ADD_PING, (now_ms(),))

    def events_after(self, after: int, limit: int) -> list[pb.Event]:
        query = "SELECT seq, id, occurred_ms, title FROM events WHERE seq > ? ORDER BY seq LIMIT ?"
        with self._open() as conn:
            rows = conn.execute(query, (after, limit)).fetchall()
        return [to_event(*row) for row in rows]


def now_ms() -> int:
    return time.time_ns() // 1_000_000


def timestamp(ms: int) -> Timestamp:
    ts = Timestamp()
    ts.FromMilliseconds(ms)
    return ts


def to_event(seq: int, event_id: str, occurred_ms: int, title: str) -> pb.Event:
    event = pb.Event(
        seq=seq,
        id=event_id,
        kind="ping",
        severity=pb.SEVERITY_INFO,
        occurred_at=timestamp(occurred_ms),
        title=title,
    )
    event.payload.update({"n": seq})
    return event


class PluginService(pb_grpc.PluginServicer):
    def __init__(self, store: Store) -> None:
        self._store = store
        self._lock = threading.Lock()
        self._last_check_ms: int | None = None
        self._failure = ""

    @override
    def Describe(self, request: pb.DescribeRequest, context: grpc.ServicerContext) -> pb.DescribeResponse:
        return pb.DescribeResponse(
            name=NAME,
            display_name="Python example",
            version="0.1.0",
            kinds=[pb.EventKind(id="ping", description="Recorded by every Check")],
        )

    @override
    def Status(self, request: pb.StatusRequest, context: grpc.ServicerContext) -> pb.StatusResponse:
        with self._lock:
            last_check_ms, failure = self._last_check_ms, self._failure
        response = pb.StatusResponse(state=pb.STATE_IDLE)
        if failure:
            response.state = pb.STATE_FAILING
            response.message = failure
        if last_check_ms is not None:
            response.last_check_started_at.CopyFrom(timestamp(last_check_ms))
            if not failure:
                response.last_success_at.CopyFrom(timestamp(last_check_ms))
        return response

    @override
    def Check(self, request: pb.CheckRequest, context: grpc.ServicerContext) -> pb.CheckResponse:
        started_ms = now_ms()
        try:
            self._store.add_ping()
        except StorageError as err:
            failure = f"storage unavailable: {err}"
            self._record_check(started_ms, failure)
            log("error", "check failed", error=str(err))
            return pb.CheckResponse(accepted=False, message=failure)
        self._record_check(started_ms, "")
        return pb.CheckResponse(accepted=True)

    def _record_check(self, started_ms: int, failure: str) -> None:
        with self._lock:
            self._last_check_ms = started_ms
            self._failure = failure

    @override
    def Events(self, request: pb.EventsRequest, context: grpc.ServicerContext) -> pb.EventsResponse:
        limit = request.limit if request.limit > 0 else DEFAULT_LIMIT
        try:
            events = self._store.events_after(request.after, limit)
        except StorageError as err:
            log("error", "events unavailable", error=str(err))
            context.abort(grpc.StatusCode.UNAVAILABLE, f"storage unavailable: {err}")
        return pb.EventsResponse(events=events)


def health_handler() -> grpc.GenericRpcHandler:
    # grpc.health.v1 is wire-compatible with these wrappers: the request is a string at
    # field 1 and the response an enum (an int32) at field 1. The grpcio-health-checking
    # package is not among the Alpine packages the plugin may install.
    def check(request: wrappers_pb2.StringValue, context: grpc.ServicerContext) -> wrappers_pb2.Int32Value:
        if request.value != "plugin":
            context.abort(grpc.StatusCode.NOT_FOUND, "unknown service")
        return wrappers_pb2.Int32Value(value=SERVING)

    method = grpc.unary_unary_rpc_method_handler(
        check,
        request_deserializer=wrappers_pb2.StringValue.FromString,
        response_serializer=wrappers_pb2.Int32Value.SerializeToString,
    )
    return grpc.method_handlers_generic_handler("grpc.health.v1.Health", {"Check": method})


def controller_handler(stop: threading.Event) -> grpc.GenericRpcHandler:
    # plugin.GRPCController has one method over an empty message, so Empty stands in for
    # go-plugin's own message type.
    def shutdown(request: empty_pb2.Empty, context: grpc.ServicerContext) -> empty_pb2.Empty:
        stop.set()
        return empty_pb2.Empty()

    method = grpc.unary_unary_rpc_method_handler(
        shutdown,
        request_deserializer=empty_pb2.Empty.FromString,
        response_serializer=empty_pb2.Empty.SerializeToString,
    )
    return grpc.method_handlers_generic_handler("plugin.GRPCController", {"Shutdown": method})


def socket_path() -> Path:
    base = os.environ.get("PLUGIN_UNIX_SOCKET_DIR") or os.environ.get("TMPDIR") or "/tmp"
    return Path(base) / f"plugin{secrets.token_hex(6)}"


def serve(store: Store) -> int:
    stop = threading.Event()

    def request_stop(signum: int, frame: FrameType | None) -> None:
        stop.set()

    for signum in (signal.SIGTERM, signal.SIGINT):
        signal.signal(signum, request_stop)

    path = socket_path()
    server = grpc.server(futures.ThreadPoolExecutor(max_workers=4))
    pb_grpc.add_PluginServicer_to_server(PluginService(store), server)
    server.add_generic_rpc_handlers((health_handler(), controller_handler(stop)))
    try:
        if not server.add_insecure_port(f"unix:{path}"):
            raise RuntimeError(f"cannot listen on {path}")
        server.start()
        log("info", "serving", socket=str(path))
        print(f"1|1|unix|{path}|grpc", flush=True)
        stop.wait()
    except RuntimeError as err:
        log("error", "cannot start the server", error=str(err))
        return 1
    finally:
        server.stop(grace=1).wait()
        path.unlink(missing_ok=True)
    return 0


def main() -> int:
    if os.environ.get("MONITOR_HUB_PLUGIN") != COOKIE:
        print("This is a monitor-hub plugin. It is started by the hub, not by hand.", file=sys.stderr)
        return 1
    data_dir = os.environ.get("MONITOR_HUB_DATA_DIR")
    if not data_dir:
        print("MONITOR_HUB_DATA_DIR is not set.", file=sys.stderr)
        return 1
    return serve(Store(Path(data_dir) / "events.sqlite3"))


if __name__ == "__main__":
    sys.exit(main())
