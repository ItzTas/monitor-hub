# Monitor protocol

monitor-hub runs each monitor as a separate process that the hub starts and talks to over gRPC. This document is enough to write a monitor in any language. You do not need to read the Go code. It describes two contracts: the one between the hub and its monitors, and, from [Consumers](#consumers), the one between the hub and the programs that receive its notifications.

The contract lives in [`proto/monitorhub/monitor/v1/monitor.proto`](../proto/monitorhub/monitor/v1/monitor.proto). Generate stubs for your language from that file. The Go stubs are in `sdk-go/monitorpb`.

The process model comes from [hashicorp/go-plugin](https://github.com/hashicorp/go-plugin) v1.8.0. The hub is the go-plugin client and your monitor is the go-plugin server. The sections below say exactly what that requires.

## How a monitor runs

1. The hub starts your command. Its working directory is the monitor directory.
2. Your process checks the magic cookie, opens a gRPC server on a Unix socket, and prints one handshake line on stdout.
3. The hub reads the line, connects to the socket, and checks health.
4. From then on the hub calls `Describe`, `Status`, `Check` and `Events` whenever it wants, and calls `Watch` to learn when to poll (see [`Watch`](#watch)).
5. On shutdown the hub asks your process to exit and kills it if it does not.

The monitor runs inside the hub's container. It must serve on a local socket only, never on a network interface.

## Startup

### 1. Check the magic cookie

The hub sets this environment variable on every monitor it starts:

```
MONITOR_HUB_MONITOR=7c0e5a52-monitor-hub
```

If the variable is missing or has a different value, someone ran the monitor by hand. Print a short message to stderr and exit with a non-zero status. Do not open a socket.

### 2. Listen on a Unix socket

Create a socket path that does not exist yet and listen on it. The socket must be reachable by the hub, which runs as the same user.

- If `PLUGIN_UNIX_SOCKET_DIR` is set, create the socket inside that directory. go-plugin only sets it when the host uses a custom runner, which the hub does not, so expect it to be unset.
- Otherwise create it under `TMPDIR`, which the hub sets to `/tmp`. A fresh name such as `/tmp/monitor123456` is fine, as long as nothing exists at that path.
- Delete the socket file when you exit.

The hub also passes `PLUGIN_MIN_PORT`, `PLUGIN_MAX_PORT` and `PLUGIN_PROTOCOL_VERSIONS`. The port variables only matter for TCP, which monitor-hub does not use. `PLUGIN_PROTOCOL_VERSIONS` lists the application protocol versions the hub accepts. Today that is `2`.

Do not use TLS. monitor-hub runs without AutoMTLS, so the hub does not send `PLUGIN_CLIENT_CERT` and expects a plain gRPC connection.

### 3. Serve the gRPC services

Register two services on the server:

- `monitorhub.monitor.v1.Monitor`, which you implement.
- The standard [gRPC health service](https://github.com/grpc/grpc/blob/master/doc/health-checking.md) (`grpc.health.v1.Health`), with the service name `plugin` set to `SERVING`. The name comes from go-plugin, not from monitor-hub. The hub calls `Check` with `service = "plugin"` and requires the call to succeed, which means the name must be registered. The hub does not read the status in the reply, but report `SERVING` anyway: that is the contract. On the wire, `HealthCheckRequest` has one field, `string service = 1`, and `HealthCheckResponse` has one field, `ServingStatus status = 1`, where `SERVING = 1`.

Optionally register `plugin.GRPCController` (see [Shutdown](#shutdown)).

### 4. Print the handshake line

When the server is ready to accept connections, write exactly one line to stdout and flush it:

```
CORE-PROTOCOL-VERSION|APP-PROTOCOL-VERSION|NETWORK-TYPE|NETWORK-ADDR|PROTOCOL
```

For monitor-hub the line looks like this:

```
1|2|unix|/tmp/monitor123456|grpc
```

| Field | Value |
|---|---|
| `CORE-PROTOCOL-VERSION` | `1`. The hub rejects any other value. |
| `APP-PROTOCOL-VERSION` | `2`. This is the monitor-hub protocol version and must match `protocol_version` in `monitor.json`. |
| `NETWORK-TYPE` | `unix`. |
| `NETWORK-ADDR` | The absolute path of the socket, with no `unix:` prefix. |
| `PROTOCOL` | `grpc`. |

Print the line only after the server is listening. The hub trims whitespace and splits on `|`, so the path cannot contain `|`. If the hub does not get a valid line within 60 seconds, it kills the monitor (see [Start-up time](#start-up-time)).

Some gRPC libraries expect the address as `unix:/tmp/monitor123456`. Use that form when you bind, and print the bare path in the handshake.

### Start-up time

The hub waits 60 seconds for the handshake line. When the line has not arrived by then, the hub kills the monitor's whole process group and starts the monitor again later. The delay before each restart grows: 1 second, 5 seconds, 30 seconds, 2 minutes, then 5 minutes for every further attempt. It goes back to 1 second once the monitor has run for 10 minutes. A restart repeats everything the monitor does before the handshake, so a step that is too slow once stays too slow on every attempt.

A monitor that needs slow preparation, such as connecting to a database or applying migrations, can do it in one of two ways:

1. Prepare, then serve. Do each step before the handshake line, each under its own time limit, with a total well under 60 seconds. When a step fails or runs out of time, write one error line to stderr and exit with status 1.
2. Serve, then prepare. Print the handshake line first and prepare in the background. Until preparation is done, fail `Events` with `UNAVAILABLE` and keep answering `Describe`, `Status` and `Check` (see [How the hub calls a monitor](#how-the-hub-calls-a-monitor)). Pick it when preparation can take longer than the budget allows.

### stdout and stderr

stdout is for the handshake line and nothing else. The hub reads the first line from stdout as the handshake and discards the rest. A stray `print` before the handshake breaks startup.

Send all logs to stderr, one line per entry. The hub writes each line to its own log, tagged with the monitor name, and the hub's `LOG_LEVEL` decides which lines show.

- A line that is a single JSON object with a string `@level` and a string `@message` is logged at that level. `trace` and `debug` map to debug, `info`, `warn` and `error` map to themselves, and any other value maps to debug. The object's other fields become attributes of the entry, with `@module` kept as `module`. `@timestamp` is optional, and the hub ignores it whatever its format.
- Any other line is logged at debug as plain text. That includes JSON whose `@level` or `@message` is missing or is not a string.
- A line longer than 64 KiB is cut at 64 KiB, logged at debug and marked `truncated`. The hub drops the rest of that line.

The recommended format is the JSON that hclog writes, one object per line:

```json
{"@level":"info","@message":"fetched 3 items","@module":"diario","@timestamp":"2026-01-01T00:00:00.000000Z","items":3}
```

`@level` and `@message` are required. `@module` becomes the `module` attribute, and `@timestamp` is ignored. Every other key becomes an attribute of the entry, except that the hub renames the keys `monitor`, `module`, `time`, `level`, `msg` and `source` to `field_monitor`, `field_module` and so on. Avoid those names. Keep each line under 64 KiB, or the hub cuts it and the level is lost.

The Rust SDK writes exactly this format to stderr: `monitor_hub_sdk::logging::init` installs a `tracing` subscriber, and a panic becomes one `error` line. It prefixes a field that would collide with the keys above with `field_` itself.

### Start-up errors

When a monitor exits before its handshake line, its last `error` line on stderr should say why. The hub's settings page, which comes in a later release, will show that reason after a save restarts the monitor. The hub will keep the last JSON line with `@level` `error` that the monitor wrote before it exited and show the line's `error` attribute, cut to 500 characters. When the line has no `error` attribute, it will show `@message` instead.

So write the reason in the `error` attribute of an `error` line, and exit. Never put a secret value in it, such as a password, a token or a database URL that holds a password.

```json
{"@level":"error","@message":"fatal","@timestamp":"2026-01-01T00:00:00.000000Z","error":"connecting to the database: connection refused"}
```

With the Rust SDK, log the error as a field cast to `&dyn Error`. The field then holds the error's whole `source()` chain:

```rust
tracing::error!(error = &err as &dyn std::error::Error, "fatal");
```

## Shutdown

To stop a monitor, the hub:

1. Calls `plugin.GRPCController/Shutdown` with an empty message and ignores the result.
2. Closes the gRPC connection.
3. Waits up to 2 seconds for the process to exit.
4. Kills the process if it is still running.

`plugin.GRPCController` is a one-method service from go-plugin: `rpc Shutdown(Empty) returns (Empty)`, where `Empty` is a message with no fields. The full method path is `/plugin.GRPCController/Shutdown`. You do not have to implement it. If you do not, the call fails and the hub ignores the error. The hub then closes the connection, waits the 2 seconds, and kills the process.

The clean way is to implement `Shutdown`: stop the gRPC server, delete the socket file, and exit.

The hub starts each monitor in its own process group and kills that whole group once the monitor exits or is killed, so child processes the monitor started do not outlive it.

## The `Monitor` service

### How the hub calls a monitor

The hub polls each monitor on its own, one poll at a time, so a slow monitor does not delay the others. Each poll calls `Describe` first. When `Describe` fails, or returns a `name` other than the monitor's name in `monitor.json`, the hub logs a warning and skips the rest of that poll. Otherwise it calls `Status` and reads `Events` at the same time, or calls only `Status` when the poll does not need events (see the list below). Each of these calls has a 30-second time limit.

The hub polls a monitor:

- Right after the monitor's process starts: when the first [`Watch`](#watch) signal arrives, or when `Watch` fails. When the `Watch` call is open but has not sent a signal yet, the first poll comes 15 seconds after the hub starts, or 15 seconds after its last full poll of the monitor, and the hub keeps polling every 15 seconds until the first signal.
- On every `Watch` signal. It calls `Status` every time. It reads `Events` only on the first signal of a `Watch` call, while the monitor's baseline is not complete (see [The first read is the baseline](#the-first-read-is-the-baseline)), or when the signal's `latest_seq` is greater than the highest `seq` the hub has stored for the monitor.
- 15 seconds after the previous poll ends, while the monitor's `Watch` call is down.
- Every 5 minutes while `Watch` is up, counted from the end of the last full poll. A full poll reads `Events` whatever `latest_seq` says: the poll after the first signal of a call is one, and so is this 5-minute poll. The polls that follow later signals, heartbeats included, follow the rule of the previous item and do not postpone the 5-minute poll.

Requests that arrive while a poll runs, for example a burst of signals, become one more poll. It starts 250 ms after the running poll ends, and it reads `Events` when any of the merged requests needed them.

The hub calls `Monitor.Check` (the `Check` of `monitorhub.monitor.v1.Monitor`, not the health service's `Check` from [Startup](#3-serve-the-grpc-services)) only when someone asks it, through the hub's API, to run a cycle now. That call has a 10-second time limit.

`Describe`, `Status` and `Check` must succeed at all times, including while `Events` still answers `UNAVAILABLE`. A monitor that is not ready reports it through `Status` (`STATE_FAILING` with a reason in `message`) and through `Check` (`accepted = false`), not by failing those calls.

### `Describe`

The hub calls it on every poll. Return:

- `name`: the monitor name, identical to `name` in `monitor.json`.
- `display_name`: a human-readable name.
- `version`: the monitor version.
- `kinds`: every kind of event the monitor can produce. Each has an `id` and a `description`. The `kind` field of every event must be one of these ids.

### `Status`

Report what the monitor is doing. The hub polls it.

- `state`: one of `STATE_IDLE`, `STATE_CHECKING`, `STATE_PAUSED` or `STATE_FAILING`. Never send `STATE_UNSPECIFIED`.
- `message`: a short explanation, for example why the monitor is failing.
- `last_check_started_at`, `last_success_at`, `next_check_at`: leave a timestamp unset when it is unknown.

### `Check`

Ask the monitor to run a cycle now. It must return immediately and not wait for the cycle to finish.

- `accepted = true` means a cycle was started or queued.
- `accepted = false` means the monitor refused. It may be running one already, or the request came too early. Put the reason in `message`.

### `Events`

Return events the hub has not stored yet. See the next section for the rules.

### `Watch`

Every monitor implements `Watch`. Through it the monitor asks the hub to poll now, so a notification goes out as soon as the monitor records an event instead of up to 15 seconds later. The hub sends an empty `WatchRequest` and reads a stream of `WatchResponse` messages. Each message is a signal. It carries `latest_seq` and nothing else: no state and no event. The hub reads those through its usual poll (see [How the hub calls a monitor](#how-the-hub-calls-a-monitor)).

A monitor's `Watch` follows these rules:

- Send a signal as soon as the call starts, with the highest `seq` the monitor has issued, or 0 when it has issued none.
- Send a signal after every write of events, with the highest `seq` written. Send it once the write is committed, never before the commit.
- Send a signal when the answer `Status` would give changes, for example when the monitor pauses, resumes or starts failing.
- Send a signal at least every 30 seconds, with the same `latest_seq` when nothing changed. This heartbeat tells the hub that the call is alive.
- Signals may be merged. When several are due before the previous one has gone out, send one, with the newest `latest_seq`.
- `latest_seq` is a hint. The hub uses it only to decide whether to read `Events`, and it may be greater than what `Events` returns at that moment, for example while the monitor holds events back during the first read (see [Events recorded before the first read ends](#events-recorded-before-the-first-read-ends)).
- Keep the call open for as long as the process runs. It ends when the hub cancels it or the monitor shuts down.

The first signal of a call always makes the hub read `Events`, and so does every signal until the hub's first read of the monitor is complete. After that, the hub reads `Events` only when a signal's `latest_seq` is greater than the position the hub has stored. So after recording events, signal with their `seq`. A signal that repeats the old `latest_seq` only makes the hub read `Status` again.

A monitor whose `Watch` answers `UNIMPLEMENTED` violates the protocol, and the [conformance suite](#checks) fails it. The hub logs it as an error, `the monitor violates the protocol: its Watch answered UNIMPLEMENTED; polling it every 15s`, and otherwise treats it like any other failed call (below).

Polling every 15 seconds is the hub's fallback for the times a signal cannot be relied on: while the call is open and has not sent its first signal, after 90 seconds without a signal, and while the call is down.

What the hub does with the call:

- On the first signal of a call, the hub logs `watching a monitor` and polls.
- When 90 seconds pass without a signal, the hub cancels the call, polls at once, goes back to polling every 15 seconds, and opens `Watch` again after a delay.
- An error and the end of the call are handled the same way, `UNIMPLEMENTED` included. For these and for 90 seconds of silence, the hub logs `the monitor's Watch stopped; polling it every 15s` as a warning, with the error and the delay; for `UNIMPLEMENTED` it logs the error line above instead.
- The delay before the hub opens `Watch` again grows: 1 second, 5 seconds, 30 seconds, then 2 minutes for every further attempt. It goes back to 1 second after a call that stayed open at least 30 seconds and sent at least two signals, or when the monitor's process changes.
- When the monitor's process restarts, the hub cancels the old call and opens `Watch` on the new process.
- A signal that comes early, twice or never changes only when the hub polls, never what it stores. When a signal announces a `seq` that `Events` does not return yet, the hub reads `Events`, stores nothing and keeps its position. The next signal, or the 5-minute poll, reads again.

With the Rust SDK, `serve(monitor, notifier)` answers `Watch` from a `Notifier` (see [Signalling the hub](../sdk-rust/README.md#signalling-the-hub)). In Go, implement `Watch(*monitorpb.WatchRequest, grpc.ServerStreamingServer[monitorpb.WatchResponse]) error` on the server type that embeds `monitorpb.UnimplementedMonitorServer`, because the embedded method answers `UNIMPLEMENTED`. Return when `stream.Context()` is done.

## Scheduling

The monitor schedules its own cycles. The hub never tells it when to run, except through `Check`. The hub only pulls events and status, and the monitor asks it to pull now through [`Watch`](#watch).

State that belongs to the monitor, such as the processes already seen or the editions already read, stays inside the monitor. The hub stores events, deliveries and status.

## Events: `seq`, `after` and `id`

`Events(after, limit)` returns the events whose `seq` is greater than `after`, in increasing `seq` order. It returns exactly `min(limit, available)` of them, where `available` is the number of events after `after` that the monitor holds.

- `limit <= 0` means 100. The monitor must apply that default. Never return more than the effective limit.
- Never return fewer than the effective limit while more events exist. The hub asks for pages of 500 and keeps reading while a page is full. It treats a shorter page as the end of the read, and during the first read that short page also completes the baseline (see [The first read is the baseline](#the-first-read-is-the-baseline)). A monitor that caps its pages below `limit`, for example at 100, completes the baseline after its first page, and every older event it held back is notified on the next poll.
- `seq` is an integer that strictly increases per monitor. Every new event gets a `seq` greater than every earlier one. Never reuse a value and never go backwards, including after a restart. Persist the counter, or derive it from something that only grows.
- The hub stores the highest `seq` it has saved and sends it back as `after`. Receiving `after = X` is the hub's confirmation that everything up to `X` is saved. The monitor may delete those events then, and only then.
- Until the hub confirms, keep the events and keep returning them. If the hub asks again with the same `after`, return the same events again.
- `id` is stable and unique per monitor. It identifies the thing that happened, not the delivery. Sending the same event twice is allowed: the hub deduplicates on `(monitor, id)`, so a second copy is dropped. An event that is re-sent with a new `seq` must keep its old `id`.

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

The baseline is the hub's whole first read of a monitor it has not read to the end before: a new monitor, or one whose stored position was reset. It covers every `Events` page from `after = 0` up to the first page shorter than 500. The events of that read are stored for history and are never notified. The baseline ends when a read reaches its short page without an error; if a read fails halfway, the next poll goes on reading as part of the baseline. Everything stored after that is a candidate for a notification.

The first read happens on the hub's first poll after the monitor is up. That poll comes right after the monitor's process starts: when the first `Watch` signal arrives, or when `Watch` fails (see [How the hub calls a monitor](#how-the-hub-calls-a-monitor)). The hub polls each monitor on its own, so a slow poll of another monitor does not delay it. Each call the poll makes may take up to 30 seconds.

This has one consequence for monitors. If your monitor cannot read its events yet, because it is not logged in or its database is not ready, **fail the `Events` call with gRPC status `UNAVAILABLE`**. Do not answer with an empty list. An empty answer is a success, so it completes the baseline, and every event that shows up later gets notified as if it were new.

When the monitor is working and there is simply nothing new, return an empty list with an OK status.

### Events recorded before the first read ends

Every event the monitor records between its own start and the end of the first read is part of the baseline, unless the monitor holds it back. A monitor that runs a cycle as soon as it starts can record new events in those seconds, and the hub then stores them as history and never announces them.

The recommended behavior, which the first-party monitors `diario`, `sei` and `anhanguera` follow:

1. At start-up, before the handshake, remember the highest `seq` the monitor has issued so far.
2. Until one `Events` call has answered OK with a page shorter than its `limit`, return only events with a `seq` at or below that value. The newer ones do not count as available yet, so the page rule above still holds.
3. After that call, return every event as usual.

The first read then holds only what existed before the monitor started. Events recorded after the start reach the hub on the next poll, and are announced. That poll comes right after the monitor's next `Watch` signal, a heartbeat included, so within 30 seconds; while the `Watch` call is down, within 15 seconds. On a new database, where nothing was issued before the start, everything the monitor records, a welcome event included, is announced on that second read. When the hub already knows the monitor, holding back costs one poll.

What this does not cover:

- A restart of the monitor, or of the hub, which restarts its monitors, after the monitor recorded new events and before the first read ended. The new process counts those events as issued before its start, so the baseline takes them in.
- A hub that received the short page and then failed to store it, or failed to record the baseline as complete. The monitor has already stopped holding back, so the hub's next poll, still part of the baseline, reads the held-back events too and takes them in.

## `monitor.json`

Every monitor ships a manifest named `monitor.json`:

```json
{ "protocol_version": 2, "name": "diario", "command": ["diario-alerr", "monitor"], "apk": ["poppler-utils"] }
```

| Field | Type | Meaning |
|---|---|---|
| `protocol_version` | number | Must be `2`. A manifest with `1`, from before the gRPC service was renamed, is refused with a message saying so. |
| `name` | string | Matches `^[a-z][a-z0-9-]{1,30}$`: lowercase letters, digits and single hyphens, starting with a letter, 2 to 31 characters. It must not end with a hyphen or contain two hyphens in a row, because one monitor's environment prefix must never be the beginning of another's. It must equal the name of the monitor's directory. |
| `command` | string array | The command to run. It must not be empty. |
| `apk` | string array | Alpine packages the monitor needs. The image build installs them. |

Unknown fields are rejected.

The hub resolves `command[0]` first as a file inside the monitor directory, then through `PATH`. Use the second form for interpreters such as `python3`. The monitor runs with the monitor directory as its working directory.

## Environment

A monitor receives only these variables:

- Every hub variable that starts with the monitor's prefix, with the prefix removed. The prefix is the monitor name in upper case, with `-` replaced by `_`, followed by `__`. For a monitor named `diario-alerr` the prefix is `DIARIO_ALERR__`, so `DIARIO_ALERR__WATCH_TERMS=a,b` arrives as `WATCH_TERMS=a,b`.
- `PATH` and `TZ`, copied from the hub.
- `TMPDIR=/tmp`.
- `MONITOR_HUB_DATA_DIR=<DATA_DIR>/monitors/<name>`. This is the only place the monitor should write persistent data. The hub does not create other directories for it.

On top of that, go-plugin adds `MONITOR_HUB_MONITOR` and the `PLUGIN_*` variables described above.

Nothing else from the hub's environment is passed. A monitor cannot read the hub's database URL, notification tokens or another monitor's settings.

A later hub release changes this for a monitor whose `settings.json` has `settings_version` 2. That hub will pass `MONITOR_HUB_CONFIG_FD`, the descriptor of the monitor's [configuration document](#the-configuration-document). It will also stop passing the prefixed variables that a field names in `env`, because their values go into the document. The hub in this release does neither.

## Settings

A monitor can declare its settings in two parts that go together: a `settings.json` file and a `validate-settings` argument. The hub's settings page will draw the monitor's form from the file, and run the monitor with the argument to check new values before it saves them.

That page comes in a later release. The hub in this release does not read `settings.json`, and neither does an older hub, so a monitor can ship the file now. Both parts are optional. A monitor without the file keeps working as before, but nothing about it will be editable from the page.

This section describes version 2 of the file. The Rust SDK and the conformance command refuse any other `settings_version`, 1 included.

### `settings.json`

The file sits next to `monitor.json`, in the monitor directory. It declares the monitor's settings as fields, and the monitor reads their values from one [configuration document](#the-configuration-document). An example:

```json
{
  "settings_version": 2,
  "fields": [
    {"name": "watch_terms", "label": "Watch terms", "type": "list", "items": {"type": "string"},
     "help": "Names or registration numbers to look for in each edition.", "required": true, "env": "WATCH_TERMS"},
    {"name": "check_interval", "label": "Check interval", "type": "duration", "default": "30m", "env": "CHECK_INTERVAL"},
    {"name": "database_url", "label": "Database address", "type": "string", "secret": true, "required": true,
     "env": "DATABASE_URL"},
    {"name": "password_command", "label": "Password command", "type": "string", "secret": true,
     "editable": false, "reason": "It makes the monitor run a program inside the hub's container.",
     "env": "PASSWORD_CMD"},
    {"name": "rules", "label": "Notification rules", "type": "list",
     "items": {"type": "object", "fields": [
       {"name": "process", "label": "Process number (regular expression)", "type": "string", "required": true},
       {"name": "actions", "label": "Actions", "type": "list", "items": {"type": "choice", "options": ["urgent", "ignore"]}}
     ]}}
  ]
}
```

A document for this file could be:

```json
{"watch_terms": ["Paulo Santos", "0034712k"], "check_interval": "30m", "database_url": "postgres://db.example/diario",
 "rules": [{"process": "^0034", "actions": ["urgent"]}]}
```

| Key | Type | Meaning |
|---|---|---|
| `settings_version` | number | Must be `2`. |
| `fields` | array | The top-level fields, one object each. It may be empty. |

Each field has these attributes:

| Attribute | Type | Required | Meaning |
|---|---|---|---|
| `name` | string | yes | The field's key in the document. It matches `^[a-z][a-z0-9_]*$` and differs from the names of the other fields at the same level. |
| `label` | string | yes | The field's label. |
| `help` | string | no | One or two sentences shown under the field. |
| `type` | string | yes | One of the types in the next table. |
| `secret` | boolean | no | `true`: the hub never shows or returns the value. Not for an `object` or a `list`, and not inside a list item. |
| `required` | boolean | no | Only marks the field on the page. The monitor's validation decides whether something is missing. |
| `default` | a value of the field's type | no | The value the document gets when nothing else sets the field. Not for an `object` or a list of objects. |
| `options` | string array | for `choice` | The accepted values. |
| `fields` | array | for `object` | The object's fields, with the same attributes. |
| `items` | object | for `list` | What each item of the list is. |
| `group` | string | no, top level only | A section title. Fields with the same `group` are shown together. |
| `editable` | boolean | no, top level only | `false`: the page shows the field and where its value comes from, but cannot change it. The field then needs `env`. The default is `true`. |
| `reason` | string | with `editable: false` | Why the field cannot be edited. The page shows it. |
| `env` | string | no, top level only | The environment variable that holds the value when there is no document, without the hub's prefix. See [The env bridge](#the-env-bridge). |

An item has a `type`, which is required, plus `options` for a `choice` or `fields` for an `object`, with the meanings above. An item cannot be a `list`.

| Type | In the document |
|---|---|
| `string` | A string. |
| `integer` | An integer that fits in a signed 64-bit integer. |
| `number` | A number. |
| `boolean` | `true` or `false`. |
| `duration` | A string in the syntax of Go's `time.ParseDuration`, such as `15m`, `1h30m`, `-5m` or `300ms`. The monitor parses it. |
| `choice` | A string. The page offers only the `options`. |
| `object` | An object whose keys are the `name`s of its `fields`. |
| `list` | An array of items. |

A `default` is written the way the document holds the value: `"15m"` for a duration, `5` for an integer, `true` for a boolean, `["a", "b"]` for a list of strings.

The file is UTF-8 without a byte order mark and holds exactly one JSON object. Whitespace around the object is fine. Anything else after it is an error. `settings_version` and `fields` must be present, and so must `name`, `label` and `type` in every field and `type` in every item.

`null` counts as absent for `help`, `default`, `options`, `group`, `reason`, `env`, `fields` and `items`. For `secret`, `required` and `editable` it is an error: write `true` or `false`, or leave the attribute out. Keys must match exactly, letter case included, so `"Secret"` is an error. A key written twice in one object is an error too.

Each field must follow these rules, at every level. A broken rule is reported as `settings.json: <path>: ` followed by the text in the second column. The path says where the field is: `fields[1]` is the second top-level field, `fields[2].fields[0]` is the first field of the object `fields[2]`, `fields[2].items` is the item of the list `fields[2]`, and `fields[2].items.fields[0]` is the first field of that item. Indexes count from 0. A broken rule about an item, such as its options, is reported at the item's path.

| Rule | Text |
|---|---|
| `name` matches `^[a-z][a-z0-9_]*$`. Any other character breaks the rule, a trailing newline included. | `name "X" does not match ^[a-z][a-z0-9_]*$` |
| No two fields at the same level have the same `name`. | `name "x" is declared more than once` |
| `label` holds more than whitespace. | `label is empty` |
| A `choice` has at least one option. | `a choice needs at least one option` |
| Only a `choice` has `options`, even an empty list. | `options are only for choice` |
| Every option holds more than whitespace. | `an option is empty` |
| No option is listed twice. | `option "a" is listed more than once` |
| An `object` has at least one field. | `an object needs at least one field` |
| Only an `object` has `fields`. | `fields are only for object` |
| A `list` has `items`. | `a list needs items` |
| Only a `list` has `items`. | `items are only for list` |
| An item is not a `list`. | `a list cannot hold lists` |
| Fields nest at most 4 levels deep. A top-level field is at level 1, and the `fields` of an object, or of an item of a list of objects, are one level below it. | `fields nest more than 4 levels deep` |
| An `object` or a list of objects has no `default`. | `an object or a list of objects cannot have a default` |
| A `default` has the field's type: a string for `string`, `duration` and `choice`, an integer that fits in a signed 64-bit integer for `integer` (no fraction, no exponent, not `-0`), any number for `number`, `true` or `false` for `boolean`, and an array of such values for a `list`. | `default does not fit the field type` |
| The `default` of a `choice`, and each value in the `default` of a list of `choice`, is one of the options. | `default "z" is not one of the options` |
| The `default` of a `duration`, and each value in the `default` of a list of `duration`, follows Go's duration syntax. | `default "15" is not a duration such as 15m` |
| A `secret` field has no `default`, not even `""`. | `a secret field cannot have a default` |
| An `object` or a `list` is not `secret`. | `an object or a list cannot be secret` |
| No field inside a list item is `secret`, at any depth. | `a field inside a list item cannot be secret` |
| A field with `editable: false` has a `reason` that holds more than whitespace. | `editable: false needs a non-empty reason` |
| Only a field with `editable: false` has a `reason`, even an empty one. | `reason is only for fields with editable: false` |
| A field with `editable: false` has `env`. | `editable: false needs env` |
| Only a top-level field has `env`, `group` or `editable`. | `env is only for top-level fields`, with `group` or `editable` in place of `env` |
| `env` matches `^[A-Z][A-Z0-9_]*$`. | `env "x" does not match ^[A-Z][A-Z0-9_]*$` |
| `env` is not reserved. The reserved names are `PATH`, `TZ`, `TMPDIR` and every name that starts with `MONITOR_HUB_`, `PLUGIN_` or `LD_`. | `env "PATH" is reserved by the hub` |
| No two fields have the same `env`. | `env "X" is used by more than one field` |
| Only a field of a simple type, a `choice`, or a list of those has `env`. | `env is only for fields of a simple type, choice, or a list of those` |
| A `group`, when present, holds more than whitespace. | `group is empty` |

Options are compared exactly, character by character. `"a"` and `"A"`, or `"a"` and `"a "`, are two different options, and a `default` must be equal to one of them. The fields are checked depth first: a field, then the fields under it, then the next field. Only the first broken rule of a field is reported, in the order of the table.

The Rust SDK and the conformance command reject an unknown attribute and an unknown `type`, so that a typo such as `secert` cannot leave a secret on show. An older Rust SDK or conformance command rejects a file that uses an attribute or a type added later, so check such a file with the release that added it.

The hub does not check values against these attributes, beyond what the form control allows: a `choice` field offers only its options. Rules such as "set exactly one of these three" belong in the monitor's `validate-settings`.

The Rust SDK's reader and the conformance command apply the same rules and give the same text for every rule in the table above. They differ in a few places, so write the file so that both accept it.

- Rust stops at the first broken field. The conformance command reports every broken field, one line each.
- A lone surrogate escape in a string, such as `"\ud800"`, is an error in Rust. The conformance command accepts it.
- A problem with the file's format stops both readers at once. Such problems are a byte order mark, bytes that are not UTF-8, bad JSON, a value of the wrong JSON type, a missing or unknown attribute, a key written twice or in another letter case, an unknown `type` and a `settings_version` other than `2`. Each side words some of them its own way. When the JSON itself is wrong, each side writes `settings.json is not valid: ` followed by its own decoder's message. Rust reports an unknown `type` that way too. The conformance command writes `type "password" is not one of string, integer, number, boolean, duration, choice, object, list`.

### The configuration document

A monitor that ships `settings.json` reads its settings from one JSON document. The document is an object whose keys are the `name`s of the top-level fields, with values as in the table of types. A field without a value is left out. The document is at most 64 KiB (65536 bytes).

The document arrives as a file the monitor inherits, already open: an anonymous file with no path on disk. `MONITOR_HUB_CONFIG_FD` holds the number of its descriptor, 3 or more. The hub and the monitors run as the same user, so another monitor could read a file with a path, even one with mode `0600`. Read the document once, when the monitor starts, and close the descriptor, so that a program the monitor runs does not inherit it. New settings take effect when the monitor restarts.

A field's value comes from the settings page first, then from the `.env` variable its `env` names, then from its `default`:

- An empty string, `""`, means not set, so it takes the `default` too.
- A missing object is built from the defaults of its fields. It goes into the document if it ends up with at least one key.
- An object, or an item of a list of objects, that lacks a field with a `default` gets that default.
- A secret field never has a default.
- Any other value stays as it is, including `null` and a value of the wrong type, and the monitor refuses it. Deserializers such as serde quote the refused value in their errors, so a monitor never passes that text on.

Without a descriptor, when `MONITOR_HUB_CONFIG_FD` is unset or empty, the monitor builds the same document from its environment through the [env bridge](#the-env-bridge), and applies the defaults the same way. That is how a monitor runs by hand, in tests, under the conformance command and under the hub in this release. `monitor_hub_sdk::config::load` does both; see [`sdk-rust/README.md`](../sdk-rust/README.md).

The shared vectors in [`testdata/config/`](../testdata/config) pin these rules for every implementation: `env.json` for the bridge, and `defaults.json` for the defaults.

### The env bridge

A top-level field of a simple type, a `choice`, or a list of those can name an environment variable in `env`, without the hub's prefix. When there is no descriptor, the field's value comes from that variable, converted to the field's type:

- An unset or empty variable leaves the field out, so its `default` applies.
- `string`, `duration` and `choice` take the text as it is, without trimming. A `choice` is not checked against its options, so the monitor has to refuse a wrong one itself.
- `integer` takes `^[+-]?[0-9]+$` within a signed 64-bit integer. `+3` and `03` become `3`, while `" 3"` and `3.0` are problems.
- `number` takes `^[+-]?([0-9]+(\.[0-9]*)?|\.[0-9]+)([eE][+-]?[0-9]+)?$` when the value is finite. `NaN`, `inf`, `1e999` and `1_0` are problems.
- `boolean` takes exactly `true` or `false`.
- A `list` cuts the text at each `,`, trims Unicode whitespace from each piece, drops the empty pieces and converts each one by the item's type. `" a , b ,,"` becomes `["a", "b"]`.

In a field that is not `secret`, text that is not UTF-8 has each maximal invalid subpart replaced with one U+FFFD, as Rust's `String::from_utf8_lossy` does. So `a\xffb` becomes `a\u{fffd}b`, and `a\xff\xffb` becomes `a\u{fffd}\u{fffd}b`.

Text that does not convert is a problem. It is never dropped, and never replaced with the default. Every such problem is reported, in the order of the fields:

- `path` is the field's path. For a list it ends with the item's index, which counts only the pieces kept, so `N=1, ,x` gives `n[1]`.
- `message` is `<VAR> is not an integer (<text>)`, `is not a number (<text>)` or `is not true or false (<text>)`. `<VAR>` is the variable's name, and `<text>` is the text quoted as a JSON string, or the trimmed piece for a list.
- In a `secret` field the message leaves the text out, as in `PIN is not an integer`. Text that is not UTF-8 there gives `PIN is not valid UTF-8`.

Written as `path: message`, `N=three` gives `n: N is not an integer ("three")`.

### `validate-settings`

A monitor that ships `settings.json` must also accept `<command[0]> validate-settings`. The hub finds `command[0]` the way it finds the monitor's command: first in the monitor directory, then through `PATH`. It runs it in the monitor directory with `validate-settings` as its only argument. The rest of `command` is not passed, and no field can name another program.

So `command[0]` must be the monitor's own program. A monitor started through an interpreter, such as [`examples/python-monitor`](../examples/python-monitor) with `["python3", "main.py"]`, would get `python3 validate-settings`. Before such a monitor ships `settings.json`, it needs an executable of its own, for example a script with a shebang line.

The values to check arrive as a configuration document, through `MONITOR_HUB_CONFIG_FD`, as at start. Without a descriptor, the monitor builds the document from its environment through the env bridge, as at start too. The magic cookie may be missing, so look for `validate-settings` before anything checks the cookie.

The monitor writes one JSON object on stdout and exits:

```json
{"problems": [{"path": "rules[2].process", "message": "not a valid regular expression"}]}
```

| Exit code | `problems` | Meaning |
|---|---|---|
| `0` | empty | The values are valid. |
| `1` | at least one | The values are invalid. |
| anything else | | The validator failed. |

A validator has failed too if it exits with `0` but lists problems, exits with `1` but lists none, prints something other than one JSON object, names a path the file does not declare, or runs for more than 5 seconds. When the validator fails, the hub will save nothing and say that the monitor could not validate the values.

- `problems` is required. Each problem has a `message` that is not empty or only spaces, and a `path` that is either declared by the file or empty. An empty `path` is for a problem with several fields together or with the whole document. `path` may be left out or `null`, which both count as empty. Other keys are ignored, so a problem in the version 1 form, with `name` and no `path`, counts as a problem with the whole document.
- A path starts with the `name` of a top-level field. `.name` goes into an object or an item of a list of objects, and `[index]` picks an item of a list. The index is `0` or digits without a leading zero. `rules`, `rules[2]`, `rules[2].process` and `rules[2].actions[0]` are paths of the example file. `rules.process` and `rules[02]` are not.
- List every problem at once, not only the first.
- Finish within 5 seconds. A validator past the limit is killed without warning, so it must not depend on cleaning up.
- Write the answer and nothing else on stdout, in UTF-8 and at most 64 KiB (65536 bytes). Whitespace around the object is fine. Logs go to stderr.
- Do not require the magic cookie, open a database, use the network, run a program named in the settings (a password command, for example) or write to disk.
- Never put the value of a `secret` field in a message.

Validation cannot prove everything. A wrong password or an unreachable database only shows when the monitor starts, which is what [Start-up errors](#start-up-errors) are for.

With the Rust SDK, `monitor_hub_sdk::settings::answer` writes the answer and returns the exit code. See [`sdk-rust/README.md`](../sdk-rust/README.md).

The Rust testkit and the conformance command read the answer with the same checks, and they differ in a few cases. The conformance command accepts keys in any letter case, such as `"Problems"`. Inside a problem, it also reads `Path` or `PATH` as `path` and checks it against the file, while the Rust testkit ignores such a key. Write the keys in lower case. The Rust testkit also rejects bytes that are not UTF-8 and a lone surrogate escape, such as `"\ud800"`, anywhere in the answer, while the conformance command accepts them. When the answer is not valid JSON, or a value has the wrong type, each side quotes its own decoder's message or names the problem that is not an object.

### Variables the file does not declare

The page will also let its user set variables the file does not declare, such as a proxy variable that a library reads. Only monitors that ship `settings.json` get them. Without the file, the hub cannot tell which names are dangerous, so nothing is editable. Such a name must match `^[A-Z][A-Z0-9_]*$`, must not be reserved, and must not be a name a field declares in `env`.

Declare every variable that makes the monitor run a program or read a file as `editable: false`, with a `reason`. Examples are a command that prints a password and the path of a password file. If the page could change them, anyone who can use the page could run a program inside the hub's container. Those variables stay in `.env`.

When a new version of the monitor declares a variable that was saved as an undeclared one, it becomes that field and keeps the saved value. When a version stops declaring a field, its saved value becomes an undeclared variable.

### Keeping the file and the code in step

Add a test that fails when the code and the file disagree. A document with every declared field must load into the monitor's configuration, and a document with a key the file does not declare must be refused, at every level. So make the configuration refuse unknown keys.

In Rust, `monitor_hub_sdk_testkit::settings::assert_no_drift::<T>` runs that test against your configuration type `T`. `full_document` builds the document with every field, and `undeclared_documents` builds one document with an extra key for each object. `check` checks the file and runs the validator with an empty environment and no descriptor. `command_with_document` runs the monitor with a document on a descriptor, the way the hub will hand it over. See [`sdk-rust/README.md`](../sdk-rust/README.md). In other languages, run `monitor-conformance` with `-monitor-dir` (see [Conformance](#conformance)). It checks the file and the validator, and the comparison with the code is a test you write yourself.

## Image layout

A monitor image puts the manifest and every file the monitor needs under `/monitor/`:

```
/monitor/monitor.json
/monitor/settings.json (optional, see Settings)
/monitor/<executable or script>
/monitor/<other files>
```

The hub image build copies that directory to `/opt/monitor-hub/monitors/<name>/`. The monitor runs from there, as user `65532`, on a read-only root filesystem. It can write to `TMPDIR` and `MONITOR_HUB_DATA_DIR` only.

## Conformance

The `monitor-conformance` command starts your monitor through go-plugin, as the hub does, calls it over gRPC, and reports every rule it breaks. The start is not identical to the hub's: [What the monitor receives](#what-the-monitor-receives) lists the differences. Build the command from this repository, or run it at a released version:

```
go run github.com/ItzTas/monitor-hub/sdk-go/cmd/monitor-conformance@v0.7.0 \
  -min-events 2 -ready-timeout 10s -monitor-dir . -env EVENTS=2 -- ./my-monitor
```

Everything after `--` is the monitor's command line. The flags go before it:

| Flag | Default | Meaning |
|---|---|---|
| `-min-events N` | `0` | The first page of events must hold at least `N` events. |
| `-ready-timeout D` | `0` | How long to wait while the first `Events` call answers `UNAVAILABLE`, for example `10s`. The call is retried every 200 ms, and only for that status. With `0` it is not retried. |
| `-monitor-dir DIR` | none | The monitor directory. When it holds `settings.json`, the command checks the file and runs the monitor's `validate-settings` in `DIR` before it starts the monitor (see [Settings](#settings)). |
| `-env NAME=value` | none | An environment variable for the monitor. Repeat the flag for more. |

`-env` puts the value in the command's argument list, where other users of the same machine can read it (with `ps`, or in `/proc/<pid>/cmdline`). Pass only throwaway test credentials this way, such as the URL of a test database.

### What the monitor receives

- The `-env` variables.
- The variables go-plugin adds for the handshake: `MONITOR_HUB_MONITOR`, `PLUGIN_MIN_PORT`, `PLUGIN_MAX_PORT` and `PLUGIN_PROTOCOL_VERSIONS`.
- Nothing else. The hub passes `PATH`, `TZ`, `TMPDIR` and `MONITOR_HUB_DATA_DIR`, but the command does not: pass the ones your monitor needs with `-env`. Without `TMPDIR`, the monitor creates its socket in its own default directory, which is `/tmp` for the SDKs.
- The working directory of the command, not the monitor directory.
- The command's process group. The hub gives each monitor a process group of its own; the command does not.

Pass whatever configuration the monitor requires to produce events.

With `-monitor-dir`, the command first runs the validator, which receives:

- No environment variables at all: no `PATH`, no magic cookie and none of the `-env` variables.
- No configuration document: no descriptor and no `MONITOR_HUB_CONFIG_FD`. The monitor builds its document from the empty environment through the [env bridge](#the-env-bridge).
- `DIR` as its working directory.
- The first word after `--`, with `validate-settings` as its only argument. The command finds that program as it finds the monitor: a path counts from the current directory, and a bare name is looked up in the command's own `PATH`, never in `DIR`. The hub looks in the monitor directory first, so pass the program as a path.

The command checks `settings.json` on every system, but runs `validate-settings` on Unix systems only (Linux and macOS). Elsewhere it reports one violation saying that the validator check needs a Unix system.

### Output

Exit codes:

| Code | Meaning |
|---|---|
| `0` | The monitor passed. A line on stdout says so. |
| `1` | The monitor broke a rule, or could not be started and reached. Each violation is one line on stderr. |
| `2` | The command was called wrongly, for example with a `-monitor-dir` that is not a directory. A line on stderr says what is wrong. |

The command copies everything the monitor writes to stderr onto its own stderr, with `monitor: ` in front of each line. The monitor's lines come out as the monitor writes them. The command writes its own lines (violations, or a line starting with `monitor-conformance: ` when the monitor could not be started or reached) only after it has stopped the monitor and read the monitor's stderr to the end. So every monitor line comes before them, and when a monitor dies during start-up, its last error is just above the command's line.

The validator's stderr is copied the same way, with the same prefix. It runs before the monitor starts, so its lines come first. Among the command's own lines, the settings violations come first.

### Checks

The suite checks that:

- `Describe` returns a valid `name` (see [`monitor.json`](#monitorjson)), a non-empty `display_name`, and kind ids that are non-empty and unique.
- `Status` returns a state other than `STATE_UNSPECIFIED`.
- The first `Events(after = 0, limit = 1000)` succeeds and returns at least `-min-events` events. Each event has a positive `seq` that increases strictly, a unique non-empty `id`, a non-empty `title`, a `kind` declared by `Describe`, a `severity` other than `SEVERITY_UNSPECIFIED`, and an `occurred_at`.
- `Events` after the last `seq` is empty, and `Events` after the first `seq` equals the first list without its first event.
- When the first list holds at least two events, `Events(after = 0, limit = 1)` returns exactly the first event, and `Events(after = 0, limit = 0)` returns at most 100 events.
- `Check` succeeds.
- `Watch` sends its first signal within 5 seconds, no signal has a negative `latest_seq`, and the call is still open 1 second after the first signal. A monitor whose `Watch` answers `UNIMPLEMENTED` fails with `Watch answered UNIMPLEMENTED; every monitor must implement Watch`. The suite does not check the 30-second heartbeat, the signal after each write, or a call that ends more than 1 second after its first signal. Without the heartbeat, or when the call ends, the hub falls back to polling every 15 seconds. Without the signal after a write, the hub reads the event when a later signal announces its `seq`, or at the 5-minute poll.

With `-monitor-dir`, when `DIR` holds `settings.json`, the suite also checks that:

- The file can be read and follows every rule in [`settings.json`](#settingsjson). Each broken field is one violation, for its first broken rule, with the text of the rules table. A problem with the file's format, such as bad JSON or a field without `name`, stops the reading and is the only violation for the file.
- `validate-settings`, run as described in [What the monitor receives](#what-the-monitor-receives), finishes within 5 seconds, exits with `0` or `1`, writes at most 64 KiB on stdout, and prints one JSON object whose `problems` agree with its exit code, each with a `message` that is not empty or only spaces. An answer with exit code `1` that lists problems passes: a monitor with required settings is expected to answer that way to the empty environment. When the file follows every rule, each problem's `path` must also be empty or a path the file declares; that check comes last. A validator that waits for the magic cookie prints nothing and fails. Only the first problem with the validator is reported.

When `DIR` has no `settings.json`, the suite checks nothing more and does not run the validator.

The checks are limited to 30 seconds plus `-ready-timeout`. The `Watch` check runs last, inside that limit. It takes at most 6 seconds, 5 for the first signal and 1 for the call to stay open. When the earlier checks leave it less than that and the limit runs out during it, the check reports `Watch` as failed. Starting the monitor and its handshake come before that limit, and go-plugin's default start timeout of one minute applies to them. The validator runs earlier still, under its own 5-second limit. Because the suite reads the events more than once and compares the answers, the monitor must keep its events stable while the suite runs: no new events and no changes between the calls.

The Rust SDK's testkit runs this command from `cargo test`. See [`sdk-rust/README.md`](../sdk-rust/README.md).

## Consumers

A consumer is a program that receives the hub's notifications. It can run on any machine and be written in any language. The desktop notification command is one; so is a script that forwards events to a chat of your own. The hub's built-in channels (Telegram, WhatsApp, ntfy, email, webhook, AMQP) are separate and stay inside the hub.

### What a consumer is

A consumer opens one long HTTP request, the stream, and reads server-sent events from it. After it handles events it tells the hub how far it got. The hub remembers that position for the consumer, so a consumer that was off for a day gets what it missed when it comes back.

The routes live under `/consumer/v1/`. The `v1` is the version of this contract. A change that breaks existing consumers goes to `/consumer/v2/`, and the hub keeps serving `v1` while anyone uses it. New JSON fields and new event types can appear inside `v1`, so a consumer must ignore any field and any event type it does not know.

These routes use the consumer's own token, not `API_TOKEN`.

### Consumers and tokens

Each consumer has a name and a token of its own. The name starts with a lowercase letter and then uses lowercase letters, digits and hyphens, 2 to 31 characters in all. It does not end in `-` and has no `--`.

The token is `mhc_` followed by 43 characters of base64url (47 in all). It is shown once, when the consumer is created or its token is replaced. The hub stores only its SHA-256.

The owner manages consumers with four subcommands of the hub's binary, run where the hub runs:

| Command | What it does |
|---|---|
| `monitor-hub consumers add <name>` | Creates the consumer and prints its token, once. |
| `monitor-hub consumers list` | Prints name, position, events behind, last acknowledgement and last time seen. |
| `monitor-hub consumers rotate <name>` | Replaces the token and keeps the position. The old token stops working. |
| `monitor-hub consumers revoke <name>` | Deletes the consumer. Its open stream ends at the next ping. |

A new consumer starts at the end: it receives no history, only events the hub records after it was created.

### The stream

```
GET /consumer/v1/stream
Authorization: Bearer <consumer token>
```

The response is `text/event-stream`. The hub sends, in this order:

1. Every event after the consumer's stored position, oldest first, in pages of 500. Events recorded as a monitor's baseline are never sent.
2. One `caught_up` message.
3. Each new event as the hub stores it.

Three messages carry data.

`event: notification` has `id: <seq>` and one JSON object per event:

```json
{
  "seq": 1521,
  "monitor": "diario",
  "display_name": "Diário Oficial",
  "kind": "match",
  "urgent": false,
  "title": "Nome encontrado na edição de hoje",
  "body": "…",
  "url": "https://…",
  "occurred_at": "2026-11-17T12:00:00Z",
  "detected_at": "2026-11-17T12:00:14Z",
  "icon": { "emoji": "🔔", "image": "/icons/3f1c….png" },
  "monitor_icon": { "emoji": "🔔", "image": "/icons/3f1c….png" }
}
```

`occurred_at` and `detected_at` are RFC 3339 timestamps in UTC and may carry fractional seconds, such as `2026-11-17T12:00:14.25Z`.

`icon` is the icon of the event's kind and `monitor_icon` the monitor's. `image` is a path from the hub's root, such as `/icons/<hash>.png`. It is never empty: at the least it is the default icon's path. Resolve it against the address you use to reach the hub. `/icons/` needs no token and serves `image/png` with `Cache-Control: public, max-age=31536000, immutable` and `Content-Security-Policy: default-src 'none'; sandbox`. The default emoji is `🔔`.

`event: caught_up` is sent once per connection, right after the backlog, with `id: <seq>` and `{"seq": 1530, "backlog": 9}`. The hub does not send it when the connection ends while the backlog is still being sent. `seq` is the larger of the newest `seq` the hub had when the connection opened and the last event sent. `backlog` is how many events the catch-up sent. It is how a consumer tells what it missed from what arrives live.

`event: replaced` carries `{}`. It means a newer connection of the same consumer took this one's place. The hub closes the connection right after.

Every 25 seconds the hub also sends a comment line, `: ping`. It is not a notification. At each ping the hub records that it saw the consumer and checks that the consumer still exists with the same token.

Also:

- **The position is the hub's, not the request's.** The stream always starts after the last acknowledged `seq`. The `Last-Event-ID` header is ignored, because many SSE libraries send it on their own and it would name an event the consumer received but never handled.
- **One token, one connection.** A new connection of a consumer ends its older ones, so a notebook that wakes up is not blocked by a connection the hub still thinks is open. Two programs with the same token would keep knocking each other off, so each program, even two on one machine, should be its own consumer.
- **A token that is missing or unknown** gets `401` with `WWW-Authenticate: Bearer`. A consumer revoked or rotated while connected has its stream ended at the next ping, and its next acknowledgement gets `401`.
- **At most 16 streams are open at once**, across all consumers. The 17th request gets `503` with `Retry-After: 30`, before the stream starts.
- **Any other `5xx`**, for example a database error while the hub checks the token, is temporary. Try again after the same wait as for a dropped connection.
- **Any method other than `GET`** on the stream gets `405`, with an `Allow` header that names `GET`.
- **A position ahead of the hub** (for example after a partial restore of the hub's database) is lowered to the newest `seq` the first time the consumer connects, and the stream starts from the newest event. This is the one case where the hub moves a position backward.

### Acknowledging

```
POST /consumer/v1/ack
Authorization: Bearer <consumer token>
Content-Type: application/json

{"seq": 1530}
```

`seq` must be between 0 and the newest `seq` in the hub; anything else gets `422`. An acknowledgement only moves the position forward: it becomes the larger of its current value and `seq`, so acknowledging an older `seq` changes nothing. The only way a position goes down is the hub lowering one that is ahead of its newest `seq`, described under [The stream](#the-stream). The answer is `200` with `{"position": 1530}`. An unknown token gets `401`.

A consumer acknowledges after it has handled the events, not when they arrive. An event it chose not to show counts as handled. It also acknowledges the `seq` of `caught_up`, so its position moves past events the hub never sent (baseline events).

The guarantee is at least once: an event that was sent and not acknowledged comes again on the next connection.

### Rules for a consumer

1. Ignore JSON fields and event types you do not know.
2. Acknowledge after handling the events, and no more than 10 seconds later. Never acknowledge a `seq` higher than the last one you received, or lower than one you already acknowledged: the hub ignores a lower one, but acknowledgements out of order are a bug in the consumer, and the conformance check reports them. Acknowledge the `caught_up` `seq` too.
3. Wait at least 1 second before reconnecting, and treat a connection that has delivered no byte for 60 seconds as dead. Close a dead connection before you open a new one: if it stays open, the hub sends `replaced` on it when the new one starts.
4. After a `401`, try again no more than once a minute.
5. After `replaced`, wait at least 5 minutes before reconnecting, and tell whoever uses the consumer that another program has the same token.

The contract does not say where a consumer keeps the hub's address and its token. A file, the system keyring, an environment variable: each consumer chooses what suits it.

### Conformance for consumers

`consumer-conformance` checks a consumer against this contract. It plays a hub with a fixed script on `127.0.0.1`, starts your consumer as a child process and reports every rule the consumer broke. Install it with:

```
go install github.com/ItzTas/monitor-hub/sdk-go/cmd/consumer-conformance@v0.5.0
```

Give it, after `--`, the command that starts your consumer. Three markers in any argument after the program are replaced before the command runs, even in the middle of an argument such as `--hub={url}`:

| Marker | Becomes |
|---|---|
| `{url}` | The address of the fake hub, for example `http://127.0.0.1:41873`. |
| `{token_file}` | The path of a temporary file, mode `0600`, that holds the token followed by a newline. |
| `{token}` | The token itself, for consumers that get it some other way. |

```
consumer-conformance -- my-consumer --hub {url} --token-file {token_file}
consumer-conformance -- ./scripts/run-under-conformance.sh {url} {token_file}
```

The second form suits a consumer that keeps its token in a keyring or its own configuration file: a small script of yours receives the address and the file and sets the consumer up your way. The token is made up for each run and the file is deleted at the end, so nothing real is exposed. The consumer inherits the command's environment, plus each `-env NAME=value`. The command joins `NO_PROXY` and `no_proxy` into one list and sets both variables to it, so a proxy setting does not catch the requests to the fake hub. It adds `127.0.0.1` and `localhost` to the list, unless the list is just `*`, which already matches every host. `-env` puts the value in the command's argument list, where other users of the same machine can read it with `ps`.

The fake hub never sends the `: ping` comment. The check starts your consumer twice, each time with a new token, and looks for these:

1. The consumer opens `GET /consumer/v1/stream` with `Authorization: Bearer <token>`. It sends no request to a path the hub does not serve. Besides the stream and the acknowledgement, the fake hub serves `GET /healthz` and the icon named in its notifications.
2. After events 1 to 3 and `caught_up` 3, it acknowledges 3 within 10 seconds. It never acknowledges past the last `seq` sent or below an earlier acknowledgement. It sends the token, a body that holds only `seq`, and either no `Content-Type` or a JSON one such as `application/json`.
3. An event of an unknown type does not drop the connection: the next event is acknowledged on the same connection.
4. When the hub closes the stream, the consumer reconnects no sooner than 1 second later and no later than 30 seconds later. The 30 seconds is the check's own limit, set with `-reconnect-within`, and not one of the rules for a consumer. A consumer that waits longer before its first reconnect fails the check.
5. After a `401`, it makes at most one more request in the next 10 seconds.
6. After `replaced`, it makes no request in the next 10 seconds.

The deadlines are options: `-start-within` (30 s, to open the stream), `-ack-within` (10 s), `-reconnect-within` (30 s) and `-quiet-after` (10 s, the watch after a `401` or `replaced`). Each must be greater than zero. The 1 second minimum before reconnecting is part of the contract and has no option. A consumer that exits at once still takes the whole `-start-within` to be reported, so pass a short one while you test.

Output follows [the monitor check](#output). Exit `0` means the consumer passed, and `consumer conformance passed` goes to stdout. Exit `1` means it broke a rule or could not be started, with one line per problem on stderr. Exit `2` means the command was called wrongly, and the usage line goes to stderr. Everything the consumer writes to stdout or stderr comes out on the command's stderr, line by line, with `consumer: ` in front. The command's own lines come after the consumer has stopped, so when a consumer dies at start-up its last words sit just above the report. When a signal (`SIGINT`, `SIGTERM` or `SIGHUP`) interrupts a run, or the reader of its output goes away, as with `| head -1`, the command stops the consumer, deletes the token file and exits `1` with `consumer-conformance: context canceled`. A signal that was ignored when the command started, as `SIGHUP` is under `nohup`, stays ignored.

## Writing a monitor without an SDK

Generate the stubs from `monitor.proto` with your language's protoc plugin. Pin the generator to a version no newer than the protobuf and gRPC runtimes the monitor ships with, because protobuf refuses to import generated code that is newer than its runtime. [`examples/python-monitor`](../examples/python-monitor) shows this for Python.

`Watch` is a server-streaming call, and every monitor implements it, so your language's gRPC generator has to support server-streaming calls. The Python example implements `Watch` with the stubs `make generate-python` writes, and passes the conformance suite.

## Checklist

- Exit with an error if `MONITOR_HUB_MONITOR` is not `7c0e5a52-monitor-hub`.
- Listen on a Unix socket, without TLS.
- Register `monitorhub.monitor.v1.Monitor` and the health service with `plugin` set to `SERVING`.
- Print `1|2|unix|<socket path>|grpc` as the only line on stdout, within 60 seconds of starting.
- Log to stderr, preferably as hclog JSON.
- Keep `seq` strictly increasing, and delete events only after the hub confirms them through `after`.
- Return exactly `min(limit, available)` events from `Events`, with 100 as the default limit.
- Fail `Events` with `UNAVAILABLE` while the monitor cannot read yet, and keep answering `Describe`, `Status` and `Check`.
- Recommended: until the first short page, return only events issued before the monitor started (see [Events recorded before the first read ends](#events-recorded-before-the-first-read-ends)).
- Implement `Watch`: a signal at once, after every committed write and every status change, and at least every 30 seconds; never before the commit.
- Implement `plugin.GRPCController/Shutdown`, or be ready to be killed after 2 seconds.
- Give every event a non-empty `title` and an `occurred_at`, and pass `monitor-conformance`.
- If you ship `settings.json`, answer `validate-settings` and keep both in step with the code.
- Read your configuration once, at start, from the document (`MONITOR_HUB_CONFIG_FD` or the env bridge), and close the descriptor.
- Write the reason for a failed start as the `error` attribute of an `error` line, with no secret in it.
