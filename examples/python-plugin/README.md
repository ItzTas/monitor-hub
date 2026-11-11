# Python plugin example

A monitor-hub plugin written in Python. It is for authors who have no SDK for their language: `main.py` implements [`docs/protocol.md`](../../docs/protocol.md) directly and passes the conformance suite. It is documentation that runs, so it is not part of the production image and not listed in any `plugins.json`.

Every `Check` records one `ping` event (`title "ping <n>"`) in SQLite. The first run records one on its own.

## How the handshake works

1. The hub starts `python3 main.py` in the plugin directory with `MONITOR_HUB_PLUGIN`, `MONITOR_HUB_DATA_DIR` and `TMPDIR` set.
2. `main.py` exits with code 1 and a sentence on stderr if the cookie is not `7c0e5a52-monitor-hub`. Nothing is opened.
3. It listens on a Unix socket named `plugin<random>` under `PLUGIN_UNIX_SOCKET_DIR` or `TMPDIR`, without TLS.
4. It registers `monitorhub.plugin.v1.Plugin`, the health service (`plugin` is `SERVING`) and `plugin.GRPCController`.
5. Once the server is running it prints `1|1|unix|<socket>|grpc` on stdout and flushes. That is the only thing the process ever writes to stdout.
6. The hub connects, checks health, and calls `Describe`, `Status`, `Check` and `Events` from then on.

## Running it under the conformance suite

```
go test -race ./internal/examples/ -run Python
```

The test starts `main.py` with only the environment it needs, runs `plugintest.Check` with `MinEvents: 1`, and then checks that no Python process and no socket file is left behind. It skips when the interpreter cannot import `grpc` and `google.protobuf`, and fails instead when `CI` is set.

To use another interpreter, for example one from a virtual environment, set `MONITOR_HUB_EXAMPLE_PYTHON` to its path. The default is `python3`.

## Versions, and why they match Alpine

The image installs `python3`, `py3-grpcio` and `py3-protobuf` from Alpine 3.24 (see `plugin.json`), so the committed generated code has to import under those runtimes:

| Package | Alpine 3.24 |
|---|---|
| `py3-grpcio` | 1.78.1-r2 |
| `py3-protobuf` | 6.33.5-r1 |

(`python3` is 3.14.10-r1.)

The generated files in `gen/` come from `grpcio-tools==1.78.0`, run by `make generate-python`. Its gencode requires protobuf 6.31.1 or newer at runtime and grpcio 1.78.0 or newer, both satisfied by the table above. Protobuf refuses gencode newer than its runtime, so the tools version must not go past what Alpine ships. Release 1.78.1 of `grpcio-tools` would be the exact match but PyPI has yanked it, and 1.78.2 was published too recently, so 1.78.0 is the pin. Do not edit the files in `gen/` by hand.

When Alpine moves to a newer `py3-grpcio` or `py3-protobuf`, update the versions in this table and regenerate.

## What it does

- **Check.** The gRPC `Check` call inserts a `ping` event and returns `accepted = true`. If the database fails it returns `accepted = false` with the reason, and `Status` reports `STATE_FAILING` until a later call works.
- **Status.** `STATE_IDLE`, or `STATE_FAILING` with a message. It sets `last_check_started_at`, and `last_success_at` when the last check worked.
- **Events.** Returns events with `seq > after`, in order, up to `limit` (100 when `limit <= 0`). If SQLite cannot be opened or read, it fails with gRPC `UNAVAILABLE` and never answers an empty list, so the hub does not take a broken start for the baseline.

## How events are stored

`$MONITOR_HUB_DATA_DIR/events.sqlite3` holds one table, `events(seq, id, occurred_ms, title)`. `seq` is the next integer after the highest stored one and `id` is `ping-<seq>`, so both grow across restarts. Each call opens its own connection. The table is created and seeded on first use.

Acknowledged events are kept: receiving `after = X` changes nothing in the database. That is the simpler choice, and it means the table grows by one row per `Check`. A real plugin would delete rows up to `X` once it receives it.

## Logging

Logs go to stderr, one JSON object per line with `@level` and `@message` (plus `@module` and extra fields), which the hub reads as a log entry with that level. It logs `info` when it starts serving and `error` when SQLite fails. It never logs the environment.

## Shutdown

It implements `plugin.GRPCController/Shutdown`. The call, SIGTERM and SIGINT all lead to the same path: the gRPC server stops with a one second grace period, the socket file is removed and the process exits with code 0. The controller and health services are registered with generic handlers over wire-compatible well-known messages (`Empty`, `StringValue`, `Int32Value`), so the example needs neither a second `.proto` file nor the `grpcio-health-checking` package, which Alpine does not offer in its three packages.

## Type checking

Run it from this directory, so `pyrightconfig.json` applies and `gen/` is left out:

```
cd examples/python-plugin && uvx basedpyright main.py
```
