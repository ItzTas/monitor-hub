# Python monitor example

A monitor for monitor-hub, written in Python. It is for authors who have no SDK for their language: `main.py` implements [`docs/protocol.md`](../../docs/protocol.md) directly and passes the conformance suite. It is documentation that runs, so it is not part of the production image and not listed in any `monitors.json`.

Every `Check` records one `ping` event (`title "ping <n>"`) in SQLite. The first run records one on its own.

## How the handshake works

1. The hub starts `python3 main.py` in the monitor directory with `MONITOR_HUB_MONITOR`, `MONITOR_HUB_DATA_DIR` and `TMPDIR` set.
2. `main.py` exits with code 1 and a sentence on stderr if the cookie is not `7c0e5a52-monitor-hub`. Nothing is opened.
3. It listens on a Unix socket named `monitor<random>` under `PLUGIN_UNIX_SOCKET_DIR` or `TMPDIR`, without TLS.
4. It registers `monitorhub.monitor.v1.Monitor`, the health service (`plugin` is `SERVING`, the name go-plugin uses) and `plugin.GRPCController`.
5. Once the server is running it prints `1|2|unix|<socket>|grpc` on stdout and flushes. That is the only thing the process ever writes to stdout.
6. The hub connects, checks health, opens `Watch`, and calls `Describe`, `Status`, `Check` and `Events` from then on.

## Running it under the conformance suite

```
go test -race ./src/internal/examples/ -run Python
```

The test starts `main.py` with only the environment it needs, runs `monitortest.Check` with `MinEvents: 1`, and then checks that no Python process and no socket file is left behind. It skips when the interpreter cannot import `grpc` and `google.protobuf`, and fails instead when `CI` is set.

To use another interpreter, for example one from a virtual environment, set `MONITOR_HUB_EXAMPLE_PYTHON` to its path. The default is `python3`.

## Versions, and why they match Alpine

The image installs `python3`, `py3-grpcio` and `py3-protobuf` from Alpine 3.24 (see `monitor.json`), so the committed generated code has to import under those runtimes:

| Package | Alpine 3.24 |
|---|---|
| `py3-grpcio` | 1.78.1-r2 |
| `py3-protobuf` | 6.33.5-r1 |

(`python3` is 3.14.10-r1.)

The generated files in `gen/` come from `grpcio-tools==1.78.0`, run by `make generate-python`. Its gencode needs protobuf 6.31.1 or newer and grpcio 1.78.0 or newer at runtime, and the table above satisfies both. `grpcio-tools` 1.78.1 would be the exact match, but PyPI has yanked it, so 1.78.0 is the pin. Do not edit the files in `gen/` by hand.

The code needs Python 3.12 or newer (`typing.override`). The data directory must already exist; the hub creates it.

The manifest name is `example-python`, and the protocol requires it to equal the directory name. To load the example in a hub, copy it into a monitor directory named `example-python`.

When Alpine moves to a newer `py3-grpcio` or `py3-protobuf`, update the versions in this table and regenerate.

## What it does

- **Check.** The gRPC `Check` call inserts a `ping` event and returns `accepted = true`. If the database fails it returns `accepted = false` with the reason, and `Status` reports `STATE_FAILING` until a later call works.
- **Status.** `STATE_IDLE`, or `STATE_FAILING` with a message. It sets `last_check_started_at`, and `last_success_at` when the last check worked.
- **Events.** Returns events with `seq > after`, in order, up to `limit` (100 when `limit <= 0`). If SQLite cannot be opened or read, it fails with gRPC `UNAVAILABLE` and never answers an empty list, so the hub does not take a broken start for the baseline.

## Watch

Each `Watch` call gets a signal at once, with the highest `seq` the monitor has issued (0 when SQLite could not be read at start-up). After that it gets one after every `Check`: with the new event's `seq` when the check worked, and with the same `latest_seq` when it failed, because `Status` changed. Checks can run in parallel, so the monitor keeps the largest `seq` it has seen. When nothing changes for 30 seconds, the call gets the same `latest_seq` again. The call ends when the hub cancels it or the process shuts down.

## How events are stored

`$MONITOR_HUB_DATA_DIR/events.sqlite3` holds one table, `events(seq, id, occurred_ms, title)`. `seq` is `INTEGER PRIMARY KEY AUTOINCREMENT`: SQLite never reuses the number of a deleted row, so `seq` keeps growing across restarts and after deletions, as the protocol requires. `id` is `ping-<seq>`. Each call opens its own connection. `MonitorService` creates the table at start-up, when it reads the highest `seq` for `Watch`, together with the first `ping` event. A single statement inserts that event, so it exists exactly once. If SQLite fails at that moment, the first later call that opens the database creates both.

Acknowledged events are kept: receiving `after = X` changes nothing in the database, so the table grows by one row per `Check`. Deleting the rows up to `X` is safe with this schema, and a real monitor would do it.

## Logging

Logs go to stderr, one JSON object per line with `@level` and `@message` (plus `@module` and extra fields), which the hub reads as a log entry with that level. It logs `info` when it starts serving and `error` when SQLite fails. It never logs the environment.

## Shutdown

It implements `plugin.GRPCController/Shutdown`. The call, SIGTERM and SIGINT all lead to the same path: open `Watch` calls end, the gRPC server stops with a one second grace period, the socket file is removed and the process exits with code 0. The controller and health services are registered with generic handlers over wire-compatible well-known messages (`Empty`, `StringValue`, `Int32Value`). That way the example needs neither a second `.proto` file nor the `grpcio-health-checking` package, which is not among the three Alpine packages.

## Type checking

Run it from this directory, so `pyrightconfig.json` applies and `gen/` is left out:

```
cd examples/python-monitor && uvx basedpyright main.py
```
