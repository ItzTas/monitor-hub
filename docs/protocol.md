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
- The standard [gRPC health service](https://github.com/grpc/grpc/blob/master/doc/health-checking.md) (`grpc.health.v1.Health`), with the service name `plugin` set to `SERVING`. The hub calls `Check` with `service = "plugin"` and requires the call to succeed, which means the name must be registered. The hub does not read the status in the reply, but report `SERVING` anyway: that is the contract.

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

Print the line only after the server is listening. The hub trims whitespace and splits on `|`, so the path cannot contain `|`. If the hub does not get a valid line within its start timeout (one minute by default), it gives up and kills the process.

Some gRPC libraries expect the address as `unix:/tmp/plugin123456`. Use that form when you bind, and print the bare path in the handshake.

### stdout and stderr

stdout is for the handshake line and nothing else. The hub reads the first line from stdout as the handshake and discards the rest. A stray `print` before the handshake breaks startup.

Send all logs to stderr. The hub collects stderr and writes it to its own log, tagged with the plugin name.

## Shutdown

To stop a plugin, the hub:

1. Calls `plugin.GRPCController/Shutdown` with an empty message and ignores the result.
2. Closes the gRPC connection.
3. Waits up to 2 seconds for the process to exit.
4. Kills the process if it is still running.

`plugin.GRPCController` is a one-method service from go-plugin: `rpc Shutdown(Empty) returns (Empty)`, where `Empty` is a message with no fields. You do not have to implement it. If you do not, the call fails and the hub ignores the error. The hub then closes the connection, waits the 2 seconds, and kills the process.

The clean way is to implement `Shutdown`: stop the gRPC server, delete the socket file, and exit.

## The `Plugin` service

### `Describe`

Called once after startup. Return:

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

`Events(after, limit)` returns the events whose `seq` is greater than `after`, in increasing `seq` order, up to `limit` of them.

- `limit <= 0` means 100. The plugin must apply that default. Never return more than the effective limit.
- `seq` is an integer that strictly increases per plugin. Every new event gets a `seq` greater than every earlier one. Never reuse a value and never go backwards, including after a restart. Persist the counter, or derive it from something that only grows.
- The hub stores the highest `seq` it has saved and sends it back as `after`. Receiving `after = X` is the hub's confirmation that everything up to `X` is saved. The plugin may delete those events then, and only then.
- Until the hub confirms, keep the events and keep returning them. If the hub asks again with the same `after`, return the same events again.
- `id` is stable and unique per plugin. It identifies the thing that happened, not the delivery. Sending the same event twice is allowed: the hub deduplicates on `(plugin, id)`, so a second copy is dropped. An event that is re-sent with a new `seq` must keep its old `id`.

The other `Event` fields:

| Field | Meaning |
|---|---|
| `kind` | One of the kind ids from `Describe`. |
| `severity` | `SEVERITY_INFO` or `SEVERITY_URGENT`. Never send `SEVERITY_UNSPECIFIED`. |
| `occurred_at` | When the event happened, not when it was reported. |
| `title`, `body` | Text for the notification. |
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
| `name` | string | Matches `^[a-z][a-z0-9-]{1,30}$`. It must equal the name of the plugin's directory. |
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

## Checklist

- Exit with an error if `MONITOR_HUB_PLUGIN` is not `7c0e5a52-monitor-hub`.
- Listen on a Unix socket, without TLS.
- Register `monitorhub.plugin.v1.Plugin` and the health service with `plugin` set to `SERVING`.
- Print `1|1|unix|<socket path>|grpc` as the only line on stdout.
- Log to stderr.
- Keep `seq` strictly increasing, and delete events only after the hub confirms them through `after`.
- Fail `Events` with `UNAVAILABLE` while the plugin cannot read yet.
- Implement `plugin.GRPCController/Shutdown`, or be ready to be killed after 2 seconds.
