# Plugin protocol

monitor-hub runs each monitor as a plugin: a separate process that the hub starts and talks to over gRPC. This document is enough to write a plugin in any language. You do not need to read the Go code. It describes two contracts: the one between the hub and its plugins, and, from [Consumers](#consumers), the one between the hub and the programs that receive its notifications.

The contract lives in [`proto/monitorhub/plugin/v1/plugin.proto`](../proto/monitorhub/plugin/v1/plugin.proto). Generate stubs for your language from that file. The Go stubs are in `sdk-go/pluginpb`.

The process model comes from [hashicorp/go-plugin](https://github.com/hashicorp/go-plugin) v1.8.0. The hub is the go-plugin client and your plugin is the go-plugin server. The sections below say exactly what that requires.

## How a plugin runs

1. The hub starts your command. Its working directory is the plugin directory.
2. Your process checks the magic cookie, opens a gRPC server on a Unix socket, and prints one handshake line on stdout.
3. The hub reads the line, connects to the socket, and checks health.
4. From then on the hub calls `Describe`, `Status`, `Check` and `Events` whenever it wants, and calls `Watch` to learn when to poll (see [`Watch`](#watch)).
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

### Start-up errors

When a plugin exits before its handshake line, its last `error` line on stderr should say why. The hub's settings page, which comes in a later release, will show that reason after a save restarts the plugin. The hub will keep the last JSON line with `@level` `error` that the plugin wrote before it exited and show the line's `error` attribute, cut to 500 characters. When the line has no `error` attribute, it will show `@message` instead.

So write the reason in the `error` attribute of an `error` line, and exit. Never put a secret value in it, such as a password, a token or a database URL that holds a password.

```json
{"@level":"error","@message":"fatal","@timestamp":"2026-01-01T00:00:00.000000Z","error":"connecting to the database: connection refused"}
```

With the Rust SDK, log the error as a field cast to `&dyn Error`. The field then holds the error's whole `source()` chain:

```rust
tracing::error!(error = &err as &dyn std::error::Error, "fatal");
```

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

The hub polls each plugin on its own, one poll at a time, so a slow plugin does not delay the others. Each poll calls `Describe` first. When `Describe` fails, or returns a `name` other than the plugin's name in `plugin.json`, the hub logs a warning and skips the rest of that poll. Otherwise it calls `Status` and reads `Events` at the same time, or calls only `Status` when the poll does not need events (see the list below). Each of these calls has a 30-second time limit.

The hub polls a plugin:

- Right after the plugin's process starts: when the first [`Watch`](#watch) signal arrives, or when `Watch` answers `UNIMPLEMENTED` or fails. When the `Watch` call is open but has not sent a signal yet, the first poll comes 15 seconds after the hub starts, or 15 seconds after its last full poll of the plugin, and the hub keeps polling every 15 seconds until the first signal.
- On every `Watch` signal. It calls `Status` every time. It reads `Events` only on the first signal of a `Watch` call, while the plugin's baseline is not complete (see [The first read is the baseline](#the-first-read-is-the-baseline)), or when the signal's `latest_seq` is greater than the highest `seq` the hub has stored for the plugin.
- 15 seconds after the previous poll ends, when the plugin does not implement `Watch` or its `Watch` call is down.
- Every 5 minutes while `Watch` is up, counted from the end of the last full poll. A full poll reads `Events` whatever `latest_seq` says: the poll after the first signal of a call is one, and so is this 5-minute poll. The polls that follow later signals, heartbeats included, follow the rule of the previous item and do not postpone the 5-minute poll.

Requests that arrive while a poll runs, for example a burst of signals, become one more poll. It starts 250 ms after the running poll ends, and it reads `Events` when any of the merged requests needed them.

The hub calls `Plugin.Check` (the `Check` of `monitorhub.plugin.v1.Plugin`, not the health service's `Check` from [Startup](#3-serve-the-grpc-services)) only when someone asks it, through the hub's API, to run a cycle now. That call has a 10-second time limit.

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

### `Watch`

`Watch` is optional. It lets the plugin ask the hub to poll now, so a notification goes out as soon as the plugin records an event instead of up to 15 seconds later. The hub sends an empty `WatchRequest` and reads a stream of `WatchResponse` messages. Each message is a signal. It carries `latest_seq` and nothing else: no state and no event. The hub reads those through its usual poll (see [How the hub calls a plugin](#how-the-hub-calls-a-plugin)).

A plugin that implements `Watch` follows these rules:

- Send a signal as soon as the call starts, with the highest `seq` the plugin has issued, or 0 when it has issued none.
- Send a signal after every write of events, with the highest `seq` written. Send it once the write is committed, never before the commit.
- Send a signal when the answer `Status` would give changes, for example when the plugin pauses, resumes or starts failing.
- Send a signal at least every 30 seconds, with the same `latest_seq` when nothing changed. This heartbeat tells the hub that the call is alive.
- Signals may be merged. When several are due before the previous one has gone out, send one, with the newest `latest_seq`.
- `latest_seq` is a hint. The hub uses it only to decide whether to read `Events`, and it may be greater than what `Events` returns at that moment, for example while the plugin holds events back during the first read (see [Events recorded before the first read ends](#events-recorded-before-the-first-read-ends)).
- Keep the call open for as long as the process runs. It ends when the hub cancels it or the plugin shuts down.

The first signal of a call always makes the hub read `Events`, and so does every signal until the hub's first read of the plugin is complete. After that, the hub reads `Events` only when a signal's `latest_seq` is greater than the position the hub has stored. So after recording events, signal with their `seq`. A signal that repeats the old `latest_seq` only makes the hub read `Status` again.

A plugin that does not implement `Watch` answers `UNIMPLEMENTED`, and nothing else changes for it: the hub polls it every 15 seconds. `protocol_version` stays `1` in both cases.

What the hub does with the call:

- When `Watch` answers `UNIMPLEMENTED` before its first signal, the hub logs `the plugin has no Watch; polling it every 15s`, polls at once and then every 15 seconds. It does not call `Watch` again until the plugin's process restarts.
- On the first signal of a call, the hub logs `watching a plugin` and polls.
- When 90 seconds pass without a signal, the hub cancels the call, polls at once, goes back to polling every 15 seconds, and opens `Watch` again after a delay.
- Any other error, and the end of the call, are handled the same way. For these and for the 90 seconds of silence, the hub logs a warning, `the plugin's Watch stopped; polling it every 15s`, with the error and the delay.
- The delay before the hub opens `Watch` again grows: 1 second, 5 seconds, 30 seconds, then 2 minutes for every further attempt. It goes back to 1 second after a call that stayed open at least 30 seconds and sent at least two signals, or when the plugin's process changes.
- When the plugin's process restarts, the hub cancels the old call and opens `Watch` on the new process.
- A signal that comes early, twice or never changes only when the hub polls, never what it stores. When a signal announces a `seq` that `Events` does not return yet, the hub reads `Events`, stores nothing and keeps its position. The next signal, or the 5-minute poll, reads again.

With the Rust SDK, use `Notifier` and `serve_with_notifier` (see [Signalling the hub](../sdk-rust/README.md#signalling-the-hub)). In Go, implement `Watch(*pluginpb.WatchRequest, grpc.ServerStreamingServer[pluginpb.WatchResponse]) error` on the server type that embeds `pluginpb.UnimplementedPluginServer`. Without it, the embedded method answers `UNIMPLEMENTED`. Return when `stream.Context()` is done.

## Scheduling

The plugin schedules its own cycles. The hub never tells it when to run, except through `Check`. The hub only pulls events and status, and a plugin can ask it to pull now through [`Watch`](#watch).

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

The baseline is the hub's whole first read of a plugin it has not read to the end before: a new plugin, or one whose stored position was reset. It covers every `Events` page from `after = 0` up to the first page shorter than 500. The events of that read are stored for history and are never notified. The baseline ends when a read reaches its short page without an error; if a read fails halfway, the next poll goes on reading as part of the baseline. Everything stored after that is a candidate for a notification.

The first read happens on the hub's first poll after the plugin is up. That poll comes right after the plugin's process starts: when the first `Watch` signal arrives, or when `Watch` answers `UNIMPLEMENTED` (see [How the hub calls a plugin](#how-the-hub-calls-a-plugin)). The hub polls each plugin on its own, so a slow poll of another plugin does not delay it. Each call the poll makes may take up to 30 seconds.

This has one consequence for plugins. If your plugin cannot read its events yet, because it is not logged in or its database is not ready, **fail the `Events` call with gRPC status `UNAVAILABLE`**. Do not answer with an empty list. An empty answer is a success, so it completes the baseline, and every event that shows up later gets notified as if it were new.

When the plugin is working and there is simply nothing new, return an empty list with an OK status.

### Events recorded before the first read ends

Every event the plugin records between its own start and the end of the first read is part of the baseline, unless the plugin holds it back. A plugin that runs a cycle as soon as it starts can record new events in those seconds, and the hub then stores them as history and never announces them.

The recommended behavior, which both first-party plugins, `diario` and `sei`, follow:

1. At start-up, before the handshake, remember the highest `seq` the plugin has issued so far.
2. Until one `Events` call has answered OK with a page shorter than its `limit`, return only events with a `seq` at or below that value. The newer ones do not count as available yet, so the page rule above still holds.
3. After that call, return every event as usual.

The first read then holds only what existed before the plugin started. Events recorded after the start reach the hub on the next poll, and are announced. That poll comes right after the plugin's next `Watch` signal, a heartbeat included, so within 30 seconds; without `Watch`, within 15 seconds. On a new database, where nothing was issued before the start, everything the plugin records, a welcome event included, is announced on that second read. When the hub already knows the plugin, holding back costs one poll.

What this does not cover:

- A restart of the plugin, or of the hub, which restarts its plugins, after the plugin recorded new events and before the first read ended. The new process counts those events as issued before its start, so the baseline takes them in.
- A hub that received the short page and then failed to store it, or failed to record the baseline as complete. The plugin has already stopped holding back, so the hub's next poll, still part of the baseline, reads the held-back events too and takes them in.

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

## Settings

A plugin can declare its settings in two parts that go together: a `settings.json` file and a `validate-settings` argument. The hub's settings page will draw the plugin's form from the file, and run the plugin with the argument to check new values before it saves them.

That page comes in a later release. The hub in this release does not read `settings.json`, and neither does an older hub, so a plugin can ship the file now. Both parts are optional. A plugin without the file keeps working as before, but nothing about it will be editable from the page.

### `settings.json`

The file sits next to `plugin.json`, in the plugin directory. An example:

```json
{
  "settings_version": 1,
  "fields": [
    {
      "name": "WATCH_TERMS",
      "label": "Watch terms",
      "help": "Names or registration numbers to look for in each edition, separated by commas.",
      "type": "list",
      "required": true
    },
    {
      "name": "CHECK_INTERVAL_MINUTES",
      "label": "Check interval (minutes)",
      "type": "number",
      "default": "30"
    },
    {
      "name": "DATABASE_URL",
      "label": "Database address",
      "type": "string",
      "secret": true,
      "required": true
    }
  ]
}
```

| Key | Type | Meaning |
|---|---|---|
| `settings_version` | number | Must be `1`. |
| `fields` | array | One object per setting. It may be empty. |

Each field has these attributes:

| Attribute | Type | Required | Meaning |
|---|---|---|---|
| `name` | string | yes | The environment variable the plugin reads, without the hub's prefix. |
| `label` | string | yes | The field's label. |
| `help` | string | no | One or two sentences shown under the field. |
| `type` | string | yes | `string`, `list`, `integer`, `number`, `duration` or `choice`. A `choice` field becomes a list of options. The other types become a text box, and the type only hints at the keyboard and the example. A `list` is the comma-separated text the plugin reads. |
| `secret` | boolean | no | `true`: the hub never shows or returns the value. It works with any type. |
| `required` | boolean | no | Only marks the field on the page. The plugin's validation decides whether something is missing. |
| `default` | string | no | The value the plugin uses when the field is empty. The page only shows it. |
| `options` | string array | for `choice` | The accepted values. |
| `group` | string | no | A section title. Fields with the same `group` are shown together. |
| `editable` | boolean | no | `false`: the page shows the field and where its value comes from, but cannot change it. The default is `true`. |
| `reason` | string | with `editable: false` | Why the field cannot be edited. The page shows it. |

The file is UTF-8 without a byte order mark and holds exactly one JSON object. Whitespace around the object is fine. Anything else after it is an error. `settings_version` and `fields` must be present, and so must `name`, `label` and `type` in every field.

`null` counts as absent for `help`, `default`, `options`, `group` and `reason`. For `secret`, `required` and `editable` it is an error: write `true` or `false`, or leave the attribute out. Keys must match exactly, letter case included, so `"Secret"` is an unknown attribute.

Each field must follow these rules. A broken rule is reported as `settings.json: fields[<index>]: ` followed by the text in the second column. `<index>` counts from 0.

| Rule | Text |
|---|---|
| `name` matches `^[A-Z][A-Z0-9_]*$`. Any other character breaks the rule, a trailing newline included. | `name "x" does not match ^[A-Z][A-Z0-9_]*$` |
| `name` is not reserved. The reserved names are `PATH`, `TZ`, `TMPDIR` and every name that starts with `MONITOR_HUB_`, `PLUGIN_` or `LD_`. | `name "PATH" is reserved by the hub` |
| No two fields have the same `name`. | `name "X" is declared more than once` |
| `label` holds more than whitespace. | `label is empty` |
| A `choice` field has at least one option. | `a choice field needs at least one option` |
| Only a `choice` field has `options`, even an empty list. | `options are only for choice fields` |
| Every option holds more than whitespace. | `an option is empty` |
| No option is listed twice. | `option "a" is listed more than once` |
| The `default` of a `choice` field is one of its options. | `default "z" is not one of the options` |
| A `secret` field has no `default`, not even `""`. | `a secret field cannot have a default` |
| A field with `editable: false` has a `reason` that holds more than whitespace. | `editable: false needs a non-empty reason` |
| Only a field with `editable: false` has a `reason`, even an empty one. | `reason is only for fields with editable: false` |
| A `group`, when present, holds more than whitespace. | `group is empty` |

Options are compared exactly, character by character. `"a"` and `"A"`, or `"a"` and `"a "`, are two different options, and a `default` must be equal to one of them. Only the first broken rule of a field is reported.

The Rust SDK and the conformance command reject an unknown attribute and an unknown `type`, so that a typo such as `secert` cannot leave a secret on show. The hub's reader will ignore them instead and treat an unknown `type` as `string`, so that a new optional attribute does not break an older hub. A change that an older hub cannot safely ignore raises `settings_version`. The hub will treat a plugin whose `settings_version` it does not know as one that declares nothing.

The hub will apply the `options` rules only to the types it knows. For a `type` it does not know, it reads the field as `string` and ignores `options`, so a later type can carry options without raising `settings_version`.

An older Rust SDK or conformance command rejects a file that uses an attribute or a type added later, so check such a file with the release that added it. A new optional attribute does not affect a file written for an older release.

The hub does not check values against these attributes, beyond what the form control allows: a `choice` field offers only its options. Rules such as "set exactly one of these three" belong in the plugin's `validate-settings`.

Every value is text, the same text the plugin reads from its environment. A `list` value is separated by commas. An empty value means "not set": the page can save an empty value to hide one that comes from `.env`, so read an empty variable as a missing one.

A `settings.json` that cannot be read, or that breaks a rule, will not stop the hub or the plugin. The hub will log the error and treat the plugin as if it declared nothing.

The Rust SDK's reader and the conformance command apply the same rules and give the same text for every rule in the table above. They differ in a few places, and the Rust reader is the stricter one. Write the file so that both accept it.

- A key repeated in one object, such as two `name` keys in a field, and a lone surrogate escape in a string, such as `"\ud800"`, are errors in Rust. The conformance command accepts both.
- Rust stops at the first broken field. The conformance command reports every broken field, one line each.
- When the JSON itself is wrong (bad syntax, a wrong value type, an unknown attribute), each side writes `settings.json is not valid: ` followed by its own decoder's message, so the details differ. Rust reports an unknown `type` the same way. The conformance command reports it as a field rule: `type "password" is not one of string, list, integer, number, duration, choice`. A missing key, a negative `settings_version`, and `null` inside `options` or `fields` are errors on both sides, but each side words them its own way.

### `validate-settings`

A plugin that ships `settings.json` must also accept `<command[0]> validate-settings`. The hub finds `command[0]` the way it finds the plugin's command: first in the plugin directory, then through `PATH`. It runs it in the plugin directory with `validate-settings` as its only argument. The rest of `command` is not passed, and no field can name another program.

So `command[0]` must be the plugin's own program. A plugin started through an interpreter, such as [`examples/python-plugin`](../examples/python-plugin) with `["python3", "main.py"]`, would get `python3 validate-settings`. Before such a plugin ships `settings.json`, it needs an executable of its own, for example a script with a shebang line.

The values to check arrive in the environment, under the names the plugin reads when it starts. The hub will build that environment the same way it builds the one the plugin runs with. The magic cookie may be missing, so look for `validate-settings` before anything checks the cookie.

The plugin writes one JSON object on stdout and exits:

```json
{"problems": [{"name": "WATCH_TERMS", "message": "set at least one name or registration number"}]}
```

| Exit code | `problems` | Meaning |
|---|---|---|
| `0` | empty | The values are valid. |
| `1` | at least one | The values are invalid. |
| anything else | | The validator failed. |

A validator that exits with `0` but lists problems, exits with `1` but lists none, prints something other than one JSON object, names a field the file does not declare, or runs for more than 5 seconds has failed too. When the validator fails, the hub will save nothing and say that the plugin could not validate the values.

- `problems` is required. Each problem has a `message` that is not empty or only spaces, and a `name` that is either a field the file declares or empty, for a problem with several fields together. `name` may be left out. Other keys are ignored.
- List every problem at once, not only the first.
- Finish within 5 seconds. A validator past the limit is killed without warning, so it must not depend on cleaning up.
- Write the answer and nothing else on stdout, in UTF-8 and at most 64 KiB (65536 bytes). Whitespace around the object is fine. Logs go to stderr.
- Do not require the magic cookie, open a database, use the network, run a program named in the settings (a password command, for example) or write to disk.
- Never put the value of a `secret` field in a message.

Validation cannot prove everything. A wrong password or an unreachable database only shows when the plugin starts, which is what [Start-up errors](#start-up-errors) are for.

With the Rust SDK, `monitor_hub_plugin::settings::answer` writes the answer and returns the exit code. See [`sdk-rust/README.md`](../sdk-rust/README.md).

The Rust testkit and the conformance command read the answer with the same checks, and they differ in a few cases. The conformance command accepts keys in any letter case, such as `"Problems"`, and a `null` `name`; the Rust testkit rejects both. Inside a problem, the conformance command also reads `Name` or `NAME` as `name` and checks it against the declared names, while the Rust testkit ignores such a key. Write the keys in lower case, and leave `name` out or empty instead of `null`. The Rust testkit also rejects bytes that are not UTF-8 and a lone surrogate escape, such as `"\ud800"`, anywhere in the answer, while the conformance command accepts them. When the answer is not valid JSON, or a value has the wrong type, each side quotes its own decoder's message or names the problem that is not an object.

### Variables the file does not declare

The page will also let its user set variables the file does not declare, such as a proxy variable that a library reads. Only plugins that ship `settings.json` get them. Without the file, the hub cannot tell which names are dangerous, so nothing is editable. Such a name must match `^[A-Z][A-Z0-9_]*$`, must not be reserved, and must not be a field the file declares with `editable: false`.

Declare every variable that makes the plugin run a program or read a file as `editable: false`, with a `reason`. Examples are a command that prints a password and the path of a password file. If the page could change them, anyone who can use the page could run a program inside the hub's container. Those variables stay in `.env`.

When a new version of the plugin declares a variable that was saved as an undeclared one, it becomes that field and keeps the saved value. When a version stops declaring a field, its saved value becomes an undeclared variable.

### Keeping the file and the code in step

Add a test that fails when the code reads a variable that `settings.json` does not declare, or when the file declares one the code does not read. Keep the names your configuration code reads in one list, so the test can compare that list with the file.

In Rust, `monitor_hub_plugin_testkit::settings::assert_no_drift` makes that comparison, and `monitor_hub_plugin_testkit::settings::check` checks the file and runs the validator with an empty environment. See [`sdk-rust/README.md`](../sdk-rust/README.md). In other languages, run `plugin-conformance` with `-plugin-dir` (see [Conformance](#conformance)). It checks the file and the validator, and the comparison with the code is a test you write yourself.

## Image layout

A plugin image puts the manifest and every file the plugin needs under `/plugin/`:

```
/plugin/plugin.json
/plugin/settings.json (optional, see Settings)
/plugin/<executable or script>
/plugin/<other files>
```

The hub image build copies that directory to `/opt/monitor-hub/plugins/<name>/`. The plugin runs from there, as user `65532`, on a read-only root filesystem. It can write to `TMPDIR` and `MONITOR_HUB_DATA_DIR` only.

## Conformance

The `plugin-conformance` command starts your plugin through go-plugin, as the hub does, calls it over gRPC, and reports every rule it breaks. The start is not identical to the hub's: [What the plugin receives](#what-the-plugin-receives) lists the differences. Build the command from this repository, or run it at a released version:

```
go run github.com/ItzTas/monitor-hub/sdk-go/cmd/plugin-conformance@v0.4.0 \
  -min-events 2 -ready-timeout 10s -plugin-dir . -env EVENTS=2 -- ./my-plugin
```

Releases up to v0.3.0 were published as `github.com/ItzTas/monitor-hub/sdk` and still install from that path.

Everything after `--` is the plugin's command line. The flags go before it:

| Flag | Default | Meaning |
|---|---|---|
| `-min-events N` | `0` | The first page of events must hold at least `N` events. |
| `-ready-timeout D` | `0` | How long to wait while the first `Events` call answers `UNAVAILABLE`, for example `10s`. The call is retried every 200 ms, and only for that status. With `0` it is not retried. |
| `-plugin-dir DIR` | none | The plugin directory. When it holds `settings.json`, the command checks the file and runs the plugin's `validate-settings` in `DIR` before it starts the plugin (see [Settings](#settings)). |
| `-env NAME=value` | none | An environment variable for the plugin. Repeat the flag for more. |

`-env` puts the value in the command's argument list, where other users of the same machine can read it (with `ps`, or in `/proc/<pid>/cmdline`). Pass only throwaway test credentials this way, such as the URL of a test database.

### What the plugin receives

- The `-env` variables.
- The variables go-plugin adds for the handshake: `MONITOR_HUB_PLUGIN`, `PLUGIN_MIN_PORT`, `PLUGIN_MAX_PORT` and `PLUGIN_PROTOCOL_VERSIONS`.
- Nothing else. The hub passes `PATH`, `TZ`, `TMPDIR` and `MONITOR_HUB_DATA_DIR`, but the command does not: pass the ones your plugin needs with `-env`. Without `TMPDIR`, the plugin creates its socket in its own default directory, which is `/tmp` for the SDKs.
- The working directory of the command, not the plugin directory.
- The command's process group. The hub gives each plugin a process group of its own; the command does not.

Pass whatever configuration the plugin requires to produce events.

With `-plugin-dir`, the command first runs the validator, which receives:

- No environment variables at all: no `PATH`, no magic cookie and none of the `-env` variables.
- `DIR` as its working directory.
- The first word after `--`, with `validate-settings` as its only argument. The command finds that program as it finds the plugin: a path counts from the current directory, and a bare name is looked up in the command's own `PATH`, never in `DIR`. The hub looks in the plugin directory first, so pass the program as a path.

The command checks `settings.json` on every system, but runs `validate-settings` on Unix systems only (Linux and macOS). Elsewhere it reports one violation saying that the validator check needs a Unix system.

### Output

Exit codes:

| Code | Meaning |
|---|---|
| `0` | The plugin passed. A line on stdout says so. |
| `1` | The plugin broke a rule, or could not be started and reached. Each violation is one line on stderr. |
| `2` | The command was called wrongly, for example with a `-plugin-dir` that is not a directory. A line on stderr says what is wrong. |

The command copies everything the plugin writes to stderr onto its own stderr, with `plugin: ` in front of each line. The plugin's lines come out as the plugin writes them. The command writes its own lines (violations, or a line starting with `plugin-conformance: ` when the plugin could not be started or reached) only after it has stopped the plugin and read the plugin's stderr to the end. So every plugin line comes before them, and when a plugin dies during start-up, its last error is just above the command's line.

The validator's stderr is copied the same way, with the same prefix. It runs before the plugin starts, so its lines come first. Among the command's own lines, the settings violations come first.

### Checks

The suite checks that:

- `Describe` returns a valid `name` (see [`plugin.json`](#pluginjson)), a non-empty `display_name`, and kind ids that are non-empty and unique.
- `Status` returns a state other than `STATE_UNSPECIFIED`.
- The first `Events(after = 0, limit = 1000)` succeeds and returns at least `-min-events` events. Each event has a positive `seq` that increases strictly, a unique non-empty `id`, a non-empty `title`, a `kind` declared by `Describe`, a `severity` other than `SEVERITY_UNSPECIFIED`, and an `occurred_at`.
- `Events` after the last `seq` is empty, and `Events` after the first `seq` equals the first list without its first event.
- When the first list holds at least two events, `Events(after = 0, limit = 1)` returns exactly the first event, and `Events(after = 0, limit = 0)` returns at most 100 events.
- `Check` succeeds.
- When the plugin implements `Watch`, the first signal arrives within 5 seconds, no signal has a negative `latest_seq`, and the call is still open 1 second after the first signal. A plugin that answers `UNIMPLEMENTED` passes without this check. The suite does not check the 30-second heartbeat, the signal after each write, or a call that ends more than 1 second after its first signal. Without the heartbeat, or when the call ends, the hub falls back to polling every 15 seconds. Without the signal after a write, the hub reads the event when a later signal announces its `seq`, or at the 5-minute poll.

With `-plugin-dir`, when `DIR` holds `settings.json`, the suite also checks that:

- The file can be read and follows every rule in [`settings.json`](#settingsjson). Each broken field is one violation, for its first broken rule, except that a field without `name`, `label` or `type` gives one line for each attribute it lacks.
- `validate-settings`, run as described in [What the plugin receives](#what-the-plugin-receives), finishes within 5 seconds, exits with `0` or `1`, writes at most 64 KiB on stdout, and prints one JSON object whose `problems` agree with its exit code, each with a `message` that is not empty or only spaces. An answer with exit code `1` that lists problems passes: a plugin with required settings is expected to answer that way to the empty environment. When the file follows every rule, each problem's `name` must also be empty or a field the file declares; that check comes last. A validator that waits for the magic cookie prints nothing and fails. Only the first problem with the validator is reported.

When `DIR` has no `settings.json`, the suite checks nothing more and does not run the validator.

The checks are limited to 30 seconds plus `-ready-timeout`. The `Watch` check runs last, inside that limit. It takes at most 6 seconds, 5 for the first signal and 1 for the call to stay open. When the earlier checks leave it less than that and the limit runs out during it, the check reports `Watch` as failed. Starting the plugin and its handshake come before that limit, and go-plugin's default start timeout of one minute applies to them. The validator runs earlier still, under its own 5-second limit. Because the suite reads the events more than once and compares the answers, the plugin must keep its events stable while the suite runs: no new events and no changes between the calls.

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

1. Every event after the consumer's stored position, oldest first, in pages of 500. Events recorded as a plugin's baseline are never sent.
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

The second form suits a consumer that keeps its token in a keyring or its own configuration file: a small script of yours receives the address and the file and sets the consumer up your way. The token is made up for each run and the file is deleted at the end, so nothing real is exposed. The consumer inherits the command's environment, plus each `-env NAME=value`. The command adds `127.0.0.1,localhost` to the end of `NO_PROXY` and `no_proxy`, so a proxy setting does not catch the requests to the fake hub. `-env` puts the value in the command's argument list, where other users of the same machine can read it with `ps`.

The fake hub never sends the `: ping` comment. The check starts your consumer twice, each time with a new token, and looks for these:

1. The consumer opens `GET /consumer/v1/stream` with `Authorization: Bearer <token>`. It sends no request to a path the hub does not serve. Besides the stream and the acknowledgement, the fake hub serves `GET /healthz` and the icon named in its notifications.
2. After events 1 to 3 and `caught_up` 3, it acknowledges 3 within 10 seconds. It never acknowledges past the last `seq` sent or below an earlier acknowledgement. It sends the token, a body that holds only `seq`, and either no `Content-Type` or a JSON one such as `application/json`.
3. An event of an unknown type does not drop the connection: the next event is acknowledged on the same connection.
4. When the hub closes the stream, the consumer reconnects, and not sooner than 1 second later.
5. After a `401`, it makes at most one more request in the next 10 seconds.
6. After `replaced`, it makes no request in the next 10 seconds.

The deadlines are options: `-start-within` (30 s, to open the stream), `-ack-within` (10 s), `-reconnect-within` (30 s) and `-quiet-after` (10 s, the watch after a `401` or `replaced`). Each must be greater than zero. The 1 second minimum before reconnecting is part of the contract and has no option. A consumer that exits at once still takes the whole `-start-within` to be reported, so pass a short one while you test.

Output follows [the plugin check](#output). Exit `0` means the consumer passed, and `consumer conformance passed` goes to stdout. Exit `1` means it broke a rule or could not be started, with one line per problem on stderr. Exit `2` means the command was called wrongly, and the usage line goes to stderr. Everything the consumer writes to stdout or stderr comes out on the command's stderr, line by line, with `consumer: ` in front. The command's own lines come after the consumer has stopped, so when a consumer dies at start-up its last words sit just above the report.

## Writing a plugin without an SDK

Generate the stubs from `plugin.proto` with your language's protoc plugin. Pin the generator to a version no newer than the protobuf and gRPC runtimes the plugin ships with, because protobuf refuses to import generated code that is newer than its runtime. [`examples/python-plugin`](../examples/python-plugin) shows this for Python.

`Watch` is a server-streaming call. To implement it, your language's gRPC generator has to support those; otherwise leave `Watch` out. A server that does not implement the method answers `UNIMPLEMENTED`, which the hub accepts: the Python example's stubs come from a `plugin.proto` without `Watch`, and it passes the conformance suite.

## Checklist

- Exit with an error if `MONITOR_HUB_PLUGIN` is not `7c0e5a52-monitor-hub`.
- Listen on a Unix socket, without TLS.
- Register `monitorhub.plugin.v1.Plugin` and the health service with `plugin` set to `SERVING`.
- Print `1|1|unix|<socket path>|grpc` as the only line on stdout, within 60 seconds of starting.
- Log to stderr, preferably as hclog JSON.
- Keep `seq` strictly increasing, and delete events only after the hub confirms them through `after`.
- Return exactly `min(limit, available)` events from `Events`, with 100 as the default limit.
- Fail `Events` with `UNAVAILABLE` while the plugin cannot read yet, and keep answering `Describe`, `Status` and `Check`.
- Recommended: until the first short page, return only events issued before the plugin started (see [Events recorded before the first read ends](#events-recorded-before-the-first-read-ends)).
- Optional: implement `Watch`: a signal at once, after every committed write and every status change, and at least every 30 seconds; never before the commit.
- Implement `plugin.GRPCController/Shutdown`, or be ready to be killed after 2 seconds.
- Give every event a non-empty `title` and an `occurred_at`, and pass `plugin-conformance`.
- If you ship `settings.json`, answer `validate-settings` and keep both in step with the code.
- Write the reason for a failed start as the `error` attribute of an `error` line, with no secret in it.
