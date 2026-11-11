# Plugin protocol

monitor-hub runs each monitor as a plugin: a separate process that the hub starts and talks to over gRPC. This document is enough to write a plugin in any language. You do not need to read the Go code.

The contract lives in [`proto/monitorhub/plugin/v1/plugin.proto`](../proto/monitorhub/plugin/v1/plugin.proto). Generate stubs for your language from that file. The Go stubs are in `sdk/pluginpb`.

The process model comes from [hashicorp/go-plugin](https://github.com/hashicorp/go-plugin) v1.8.0. The hub is the go-plugin client and your plugin is the go-plugin server. The sections below say exactly what that requires.

## How a plugin runs

1. The hub starts your command. Its working directory is the plugin directory.
2. Your process checks the magic cookie, opens a gRPC server on a Unix socket, and prints one handshake line on stdout.
3. The hub reads the line, connects to the socket, and checks health.
4. From then on the hub calls `Describe`, `Status`, `Check` and `Events` whenever it wants.
5. On shutdown the hub asks your process to exit and kills it if it does not.

The plugin runs inside the hub's container. It must serve on a local socket only, never on a network interface.

## Startup

### 1. Check the magic cookie

The hub sets this environment variable on every plugin it starts:

```
MONITOR_HUB_PLUGIN=7c0e5a52-monitor-hub
```

If the variable is missing or has a different value, someone ran the plugin by hand. Print a short message to stderr and exit with a non-zero status. Do not open a socket.

### 2. Listen on a Unix socket

Create a socket path that does not exist yet and listen on it. The socket must be reachable by the hub, which runs as the same user.

- If `PLUGIN_UNIX_SOCKET_DIR` is set, create the socket inside that directory. go-plugin only sets it when the host uses a custom runner, which the hub does not, so expect it to be unset.
- Otherwise create it under `TMPDIR`, which the hub sets to `/tmp`. A fresh name such as `/tmp/plugin123456` is fine, as long as nothing exists at that path.
- Delete the socket file when you exit.

The hub also passes `PLUGIN_MIN_PORT`, `PLUGIN_MAX_PORT` and `PLUGIN_PROTOCOL_VERSIONS`. The port variables only matter for TCP, which monitor-hub does not use. `PLUGIN_PROTOCOL_VERSIONS` lists the application protocol versions the hub accepts. Today that is `1`.

Do not use TLS. monitor-hub runs without AutoMTLS, so the hub does not send `PLUGIN_CLIENT_CERT` and expects a plain gRPC connection.

### 3. Serve the gRPC services

Register two services on the server:

- `monitorhub.plugin.v1.Plugin`, which you implement.
- The standard [gRPC health service](https://github.com/grpc/grpc/blob/master/doc/health-checking.md) (`grpc.health.v1.Health`), with the service name `plugin` set to `SERVING`. The hub calls `Check` with `service = "plugin"` and requires the call to succeed, which means the name must be registered. The hub does not read the status in the reply, but report `SERVING` anyway: that is the contract. On the wire, `HealthCheckRequest` has one field, `string service = 1`, and `HealthCheckResponse` has one field, `ServingStatus status = 1`, where `SERVING = 1`.

Optionally register `plugin.GRPCController` (see [Shutdown](#shutdown)).

### 4. Print the handshake line

When the server is ready to accept connections, write exactly one line to stdout and flush it:

```
CORE-PROTOCOL-VERSION|APP-PROTOCOL-VERSION|NETWORK-TYPE|NETWORK-ADDR|PROTOCOL
```

For monitor-hub the line looks like this:

```
1|1|unix|/tmp/plugin123456|grpc
```

| Field | Value |
|---|---|
| `CORE-PROTOCOL-VERSION` | `1`. The hub rejects any other value. |
| `APP-PROTOCOL-VERSION` | `1`. This is the monitor-hub protocol version and must match `protocol_version` in `plugin.json`. |
| `NETWORK-TYPE` | `unix`. |
| `NETWORK-ADDR` | The absolute path of the socket, with no `unix:` prefix. |
| `PROTOCOL` | `grpc`. |

Print the line only after the server is listening. The hub trims whitespace and splits on `|`, so the path cannot contain `|`. If the hub does not get a valid line within 60 seconds, it kills the plugin (see [Start-up time](#start-up-time)).

Some gRPC libraries expect the address as `unix:/tmp/plugin123456`. Use that form when you bind, and print the bare path in the handshake.

### Start-up time

The hub waits 60 seconds for the handshake line. When the line has not arrived by then, the hub kills the plugin's whole process group and starts the plugin again later. The delay before each restart grows: 1 second, 5 seconds, 30 seconds, 2 minutes, then 5 minutes for every further attempt. It goes back to 1 second once the plugin has run for 10 minutes. A restart repeats everything the plugin does before the handshake, so a step that is too slow once stays too slow on every attempt.

A plugin that needs slow preparation, such as connecting to a database or applying migrations, can do it in one of two ways:

1. Prepare, then serve. Do each step before the handshake line, each under its own time limit, with a total well under 60 seconds. When a step fails or runs out of time, write one error line to stderr and exit with status 1.
2. Serve, then prepare. Print the handshake line first and prepare in the background. Until preparation is done, fail `Events` with `UNAVAILABLE` and keep answering `Describe`, `Status` and `Check` (see [How the hub calls a plugin](#how-the-hub-calls-a-plugin)). Pick it when preparation can take longer than the budget allows.

### stdout and stderr

stdout is for the handshake line and nothing else. The hub reads the first line from stdout as the handshake and discards the rest. A stray `print` before the handshake breaks startup.

Send all logs to stderr, one line per entry. The hub writes each line to its own log, tagged with the plugin name, and the hub's `LOG_LEVEL` decides which lines show.

- A line that is a single JSON object with a string `@level` and a string `@message` is logged at that level. `trace` and `debug` map to debug, `info`, `warn` and `error` map to themselves, and any other value maps to debug. The object's other fields become attributes of the entry, with `@module` kept as `module`. `@timestamp` is optional, and the hub ignores it whatever its format.
- Any other line is logged at debug as plain text. That includes JSON whose `@level` or `@message` is missing or is not a string.
- A line longer than 64 KiB is cut at 64 KiB, logged at debug and marked `truncated`. The hub drops the rest of that line.

The recommended format is the JSON that hclog writes, one object per line:

```json
{"@level":"info","@message":"fetched 3 items","@module":"diario","@timestamp":"2026-01-01T00:00:00.000000Z","items":3}
```

`@level` and `@message` are required. `@module` becomes the `module` attribute, and `@timestamp` is ignored. Every other key becomes an attribute of the entry, except that the hub renames the keys `plugin`, `module`, `time`, `level`, `msg` and `source` to `field_plugin`, `field_module` and so on. Avoid those names. Keep each line under 64 KiB, or the hub cuts it and the level is lost.

The Rust SDK writes exactly this format to stderr: `monitor_hub_plugin::logging::init` installs a `tracing` subscriber, and a panic becomes one `error` line. It prefixes a field that would collide with the keys above with `field_` itself.

## Shutdown

To stop a plugin, the hub:

1. Calls `plugin.GRPCController/Shutdown` with an empty message and ignores the result.
2. Closes the gRPC connection.
3. Waits up to 2 seconds for the process to exit.
4. Kills the process if it is still running.

`plugin.GRPCController` is a one-method service from go-plugin: `rpc Shutdown(Empty) returns (Empty)`, where `Empty` is a message with no fields. The full method path is `/plugin.GRPCController/Shutdown`. You do not have to implement it. If you do not, the call fails and the hub ignores the error. The hub then closes the connection, waits the 2 seconds, and kills the process.

The clean way is to implement `Shutdown`: stop the gRPC server, delete the socket file, and exit.

The hub starts each plugin in its own process group and kills that whole group once the plugin exits or is killed, so child processes the plugin started do not outlive it.

## The `Plugin` service

### How the hub calls a plugin

The hub polls every plugin every 15 seconds. Each poll calls `Describe` first. When `Describe` fails, or returns a `name` other than the plugin's name in `plugin.json`, the hub logs a warning and skips the rest of that poll. Otherwise it calls `Status` and reads `Events` at the same time. Each of these calls has a 30-second time limit.

The hub calls `Check` only when someone asks it, through the hub's API, to run a cycle now. That call has a 10-second time limit.

`Describe`, `Status` and `Check` must succeed at all times, including while `Events` still answers `UNAVAILABLE`. A plugin that is not ready reports it through `Status` (`STATE_FAILING` with a reason in `message`) and through `Check` (`accepted = false`), not by failing those calls.

### `Describe`

The hub calls it on every poll. Return:

- `name`: the plugin name, identical to `name` in `plugin.json`.
- `display_name`: a human-readable name.
- `version`: the plugin version.
- `kinds`: every kind of event the plugin can produce. Each has an `id` and a `description`. The `kind` field of every event must be one of these ids.

### `Status`

Report what the plugin is doing. The hub polls it.

- `state`: one of `STATE_IDLE`, `STATE_CHECKING`, `STATE_PAUSED` or `STATE_FAILING`. Never send `STATE_UNSPECIFIED`.
- `message`: a short explanation, for example why the plugin is failing.
- `last_check_started_at`, `last_success_at`, `next_check_at`: leave a timestamp unset when it is unknown.

### `Check`

Ask the plugin to run a cycle now. It must return immediately and not wait for the cycle to finish.

- `accepted = true` means a cycle was started or queued.
- `accepted = false` means the plugin refused. It may be running one already, or the request came too early. Put the reason in `message`.

### `Events`

Return events the hub has not stored yet. See the next section for the rules.

## Scheduling

The plugin schedules its own cycles. The hub never tells it when to run, except through `Check`. The hub only pulls events and status.

State that belongs to the monitor, such as the processes already seen or the editions already read, stays inside the plugin. The hub stores events, deliveries and status.

## Events: `seq`, `after` and `id`

`Events(after, limit)` returns the events whose `seq` is greater than `after`, in increasing `seq` order. It returns exactly `min(limit, available)` of them, where `available` is the number of events after `after` that the plugin holds.

- `limit <= 0` means 100. The plugin must apply that default. Never return more than the effective limit.
- Never return fewer than the effective limit while more events exist. The hub asks for pages of 500 and keeps reading while a page is full. It treats a shorter page as the end of the read, and during the first read that short page also completes the baseline (see [The first read is the baseline](#the-first-read-is-the-baseline)). A plugin that caps its pages below `limit`, for example at 100, completes the baseline after its first page, and every older event it held back is notified on the next poll.
- `seq` is an integer that strictly increases per plugin. Every new event gets a `seq` greater than every earlier one. Never reuse a value and never go backwards, including after a restart. Persist the counter, or derive it from something that only grows.
- The hub stores the highest `seq` it has saved and sends it back as `after`. Receiving `after = X` is the hub's confirmation that everything up to `X` is saved. The plugin may delete those events then, and only then.
- Until the hub confirms, keep the events and keep returning them. If the hub asks again with the same `after`, return the same events again.
- `id` is stable and unique per plugin. It identifies the thing that happened, not the delivery. Sending the same event twice is allowed: the hub deduplicates on `(plugin, id)`, so a second copy is dropped. An event that is re-sent with a new `seq` must keep its old `id`.

The other `Event` fields. Two are required, and different parts enforce them: the hub rejects the whole page when an event has no valid `occurred_at`, and the [conformance suite](#conformance) fails an event with an empty `title`. The hub does not check `title`.

| Field | Meaning |
|---|---|
| `kind` | One of the kind ids from `Describe`. |
| `severity` | `SEVERITY_INFO` or `SEVERITY_URGENT`. Never send `SEVERITY_UNSPECIFIED`. |
| `occurred_at` | Required. When the event happened, not when it was reported. The hub rejects the page that holds an event without it. |
| `title` | The notification title. It must not be empty. The conformance suite checks this, the hub does not. |
| `body` | Text for the notification. |
| `url` | A link to the source, or empty. |
| `payload` | Free-form structured data (`google.protobuf.Struct`). The hub stores it and passes it on without reading it. |

### The first read is the baseline

The hub treats the first `Events` call that completes without an error as the baseline. Those events are stored for history and are never notified. Everything after that is a candidate for a notification.

This has one consequence for plugins. If your plugin cannot read its events yet, because it is not logged in or its database is not ready, **fail the `Events` call with gRPC status `UNAVAILABLE`**. Do not answer with an empty list. An empty answer is a success, so it completes the baseline, and every event that shows up later gets notified as if it were new.

When the plugin is working and there is simply nothing new, return an empty list with an OK status.

## `plugin.json`

Every plugin ships a manifest named `plugin.json`:

```json
{ "protocol_version": 1, "name": "diario", "command": ["diario-alerr", "plugin"], "apk": ["poppler-utils"] }
```

| Field | Type | Meaning |
|---|---|---|
| `protocol_version` | number | Must be `1`. |
| `name` | string | Matches `^[a-z][a-z0-9-]{1,30}$`: lowercase letters, digits and single hyphens, starting with a letter, 2 to 31 characters. It must not end with a hyphen or contain two hyphens in a row, because one plugin's environment prefix must never be the beginning of another's. It must equal the name of the plugin's directory. |
| `command` | string array | The command to run. It must not be empty. |
| `apk` | string array | Alpine packages the plugin needs. The image build installs them. |

Unknown fields are rejected.

The hub resolves `command[0]` first as a file inside the plugin directory, then through `PATH`. Use the second form for interpreters such as `python3`. The plugin runs with the plugin directory as its working directory.

## Environment

A plugin receives only these variables:

- Every hub variable that starts with the plugin's prefix, with the prefix removed. The prefix is the plugin name in upper case, with `-` replaced by `_`, followed by `__`. For a plugin named `diario-alerr` the prefix is `DIARIO_ALERR__`, so `DIARIO_ALERR__WATCH_TERMS=a,b` arrives as `WATCH_TERMS=a,b`.
- `PATH` and `TZ`, copied from the hub.
- `TMPDIR=/tmp`.
- `MONITOR_HUB_DATA_DIR=<DATA_DIR>/plugins/<name>`. This is the only place the plugin should write persistent data. The hub does not create other directories for it.

On top of that, go-plugin adds `MONITOR_HUB_PLUGIN` and the `PLUGIN_*` variables described above.

Nothing else from the hub's environment is passed. A plugin cannot read the hub's database URL, notification tokens or another plugin's settings.

## Image layout

A plugin image puts the manifest and every file the plugin needs under `/plugin/`:

```
/plugin/plugin.json
/plugin/<executable or script>
/plugin/<other files>
```

The hub image build copies that directory to `/opt/monitor-hub/plugins/<name>/`. The plugin runs from there, as user `65532`, on a read-only root filesystem. It can write to `TMPDIR` and `MONITOR_HUB_DATA_DIR` only.

## Conformance

The `plugin-conformance` command starts your plugin through go-plugin, as the hub does, calls it over gRPC, and reports every rule it breaks. The start is not identical to the hub's: [What the plugin receives](#what-the-plugin-receives) lists the differences. Build the command from this repository, or run it at a released version:

```
go run github.com/ItzTas/monitor-hub/sdk/cmd/plugin-conformance@v0.2.0 \
  -min-events 2 -ready-timeout 10s -env EVENTS=2 -- ./my-plugin
```

Everything after `--` is the plugin's command line. The flags go before it:

| Flag | Default | Meaning |
|---|---|---|
| `-min-events N` | `0` | The first page of events must hold at least `N` events. |
| `-ready-timeout D` | `0` | How long to wait while the first `Events` call answers `UNAVAILABLE`, for example `10s`. The call is retried every 200 ms, and only for that status. With `0` it is not retried. |
| `-env NAME=value` | none | An environment variable for the plugin. Repeat the flag for more. |

`-env` puts the value in the command's argument list, where other users of the same machine can read it (with `ps`, or in `/proc/<pid>/cmdline`). Pass only throwaway test credentials this way, such as the URL of a test database.

### What the plugin receives

- The `-env` variables.
- The variables go-plugin adds for the handshake: `MONITOR_HUB_PLUGIN`, `PLUGIN_MIN_PORT`, `PLUGIN_MAX_PORT` and `PLUGIN_PROTOCOL_VERSIONS`.
- Nothing else. The hub passes `PATH`, `TZ`, `TMPDIR` and `MONITOR_HUB_DATA_DIR`, but the command does not: pass the ones your plugin needs with `-env`. Without `TMPDIR`, the plugin creates its socket in its own default directory, which is `/tmp` for the SDKs.
- The working directory of the command, not the plugin directory.
- The command's process group. The hub gives each plugin a process group of its own; the command does not.

Pass whatever configuration the plugin requires to produce events.

### Output

Exit codes:

| Code | Meaning |
|---|---|
| `0` | The plugin passed. A line on stdout says so. |
| `1` | The plugin broke a rule, or could not be started and reached. Each violation is one line on stderr. |
| `2` | The command was called wrongly. A usage line goes to stderr. |

The command copies everything the plugin writes to stderr onto its own stderr, with `plugin: ` in front of each line. The plugin's lines come out as the plugin writes them. The command writes its own lines (violations, or a line starting with `plugin-conformance: ` when the plugin could not be started or reached) only after it has stopped the plugin and read the plugin's stderr to the end. So every plugin line comes before them, and when a plugin dies during start-up, its last error is just above the command's line.

### Checks

The suite checks that:

- `Describe` returns a valid `name` (see [`plugin.json`](#pluginjson)), a non-empty `display_name`, and kind ids that are non-empty and unique.
- `Status` returns a state other than `STATE_UNSPECIFIED`.
- The first `Events(after = 0, limit = 1000)` succeeds and returns at least `-min-events` events. Each event has a positive `seq` that increases strictly, a unique non-empty `id`, a non-empty `title`, a `kind` declared by `Describe`, a `severity` other than `SEVERITY_UNSPECIFIED`, and an `occurred_at`.
- `Events` after the last `seq` is empty, and `Events` after the first `seq` equals the first list without its first event.
- When the first list holds at least two events, `Events(after = 0, limit = 1)` returns exactly the first event, and `Events(after = 0, limit = 0)` returns at most 100 events.
- `Check` succeeds.

The checks are limited to 30 seconds plus `-ready-timeout`. Starting the plugin and its handshake come before that limit, and go-plugin's default start timeout of one minute applies to them. Because the suite reads the events more than once and compares the answers, the plugin must keep its events stable while the suite runs: no new events and no changes between the calls.

The Rust SDK's testkit runs this command from `cargo test`. See [`sdk-rust/README.md`](../sdk-rust/README.md).

## Writing a plugin without an SDK

Generate the stubs from `plugin.proto` with your language's protoc plugin. Pin the generator to a version no newer than the protobuf and gRPC runtimes the plugin ships with, because protobuf refuses to import generated code that is newer than its runtime. [`examples/python-plugin`](../examples/python-plugin) shows this for Python.

## Checklist

- Exit with an error if `MONITOR_HUB_PLUGIN` is not `7c0e5a52-monitor-hub`.
- Listen on a Unix socket, without TLS.
- Register `monitorhub.plugin.v1.Plugin` and the health service with `plugin` set to `SERVING`.
- Print `1|1|unix|<socket path>|grpc` as the only line on stdout, within 60 seconds of starting.
- Log to stderr, preferably as hclog JSON.
- Keep `seq` strictly increasing, and delete events only after the hub confirms them through `after`.
- Return exactly `min(limit, available)` events from `Events`, with 100 as the default limit.
- Fail `Events` with `UNAVAILABLE` while the plugin cannot read yet, and keep answering `Describe`, `Status` and `Check`.
- Implement `plugin.GRPCController/Shutdown`, or be ready to be killed after 2 seconds.
- Give every event a non-empty `title` and an `occurred_at`, and pass `plugin-conformance`.
