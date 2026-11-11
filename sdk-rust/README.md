# monitor-hub Rust plugin SDK

Two crates for writing monitor-hub plugins in Rust. The plugin protocol itself is described in [`docs/protocol.md`](../docs/protocol.md).

- `monitor-hub-plugin` runs the plugin side of the protocol: the magic cookie check, the Unix socket, the handshake line, the gRPC health service and shutdown. It also writes logs in the format the hub reads and, behind the `postgres` feature, applies goose-format migrations.
- `monitor-hub-plugin-testkit` is for your tests: a throwaway Postgres database and a runner for the hub's conformance suite.

## Setup

Both crates are versioned together and published as git tags of this repository. Depend on them by tag:

```toml
[dependencies]
monitor-hub-plugin = { git = "https://github.com/ItzTas/monitor-hub", tag = "sdk-rust/v0.1.0" }
prost-types = "0.14.4"
tokio = { version = "1.54.1", features = ["macros", "rt-multi-thread"] }
tonic = { version = "0.14.8", default-features = false, features = ["codegen"] }
tracing = "0.1.44"

[dev-dependencies]
monitor-hub-plugin-testkit = { git = "https://github.com/ItzTas/monitor-hub", tag = "sdk-rust/v0.1.0" }
```

Your crate implements the `Plugin` trait the SDK generates with tonic and builds messages with prost, so it needs its own `tonic` and `prost-types` dependencies, in the same 0.14 series as the SDK. This workspace uses `tonic` 0.14.8 and `prost-types` 0.14.4. `tracing` is what you log with. If you use migrations, also add `tokio-postgres = "0.7.18"`.

The SDK pins its toolchain in [`rust-toolchain.toml`](rust-toolchain.toml): Rust 1.99.1 with `rustfmt` and `clippy`, edition 2024. Pin the same version in your plugin's repository so both build with the same compiler.

## A minimal plugin

A complete plugin that reports one fixed event. It is the `fixed` example in [`crates/plugin/examples/fixed.rs`](crates/plugin/examples/fixed.rs), cut down to one event.

```rust
use std::process::ExitCode;

use monitor_hub_plugin::logging;
use monitor_hub_plugin::proto::plugin_server::Plugin;
use monitor_hub_plugin::proto::{
    CheckRequest, CheckResponse, DescribeRequest, DescribeResponse, Event, EventKind,
    EventsRequest, EventsResponse, Severity, State, StatusRequest, StatusResponse,
};
use monitor_hub_plugin::{ServeError, serve};
use prost_types::Timestamp;
use tonic::{Request, Response, Status};

struct Ping;

#[tonic::async_trait]
impl Plugin for Ping {
    async fn describe(
        &self,
        _: Request<DescribeRequest>,
    ) -> Result<Response<DescribeResponse>, Status> {
        Ok(Response::new(DescribeResponse {
            name: "ping".into(),
            display_name: "Ping".into(),
            version: env!("CARGO_PKG_VERSION").into(),
            kinds: vec![EventKind {
                id: "ping".into(),
                description: "A fixed ping".into(),
            }],
        }))
    }

    async fn status(&self, _: Request<StatusRequest>) -> Result<Response<StatusResponse>, Status> {
        Ok(Response::new(StatusResponse {
            state: State::Idle.into(),
            ..StatusResponse::default()
        }))
    }

    async fn check(&self, _: Request<CheckRequest>) -> Result<Response<CheckResponse>, Status> {
        Ok(Response::new(CheckResponse {
            accepted: true,
            message: String::new(),
        }))
    }

    async fn events(
        &self,
        request: Request<EventsRequest>,
    ) -> Result<Response<EventsResponse>, Status> {
        let events = if request.into_inner().after < 1 {
            vec![Event {
                seq: 1,
                id: "ping-1".into(),
                kind: "ping".into(),
                severity: Severity::Info.into(),
                occurred_at: Some(Timestamp {
                    seconds: 1_767_225_600,
                    nanos: 0,
                }),
                title: "ping".into(),
                ..Event::default()
            }]
        } else {
            Vec::new()
        };
        Ok(Response::new(EventsResponse { events }))
    }
}

#[tokio::main]
async fn main() -> ExitCode {
    if let Err(err) = logging::init("info") {
        eprintln!("cannot start logging: {err}");
        return ExitCode::FAILURE;
    }
    match serve(Ping).await {
        Ok(()) => ExitCode::SUCCESS,
        Err(ServeError::NotLaunchedByHub) => ExitCode::FAILURE,
        Err(err) => {
            tracing::error!("{err}");
            ExitCode::FAILURE
        }
    }
}
```

`serve` does the whole startup described in the protocol, then runs until the hub asks the plugin to stop or the process receives SIGTERM or SIGINT. Nothing but the handshake line may go to stdout, so never `println!` in a plugin.

`serve` returns a `ServeError` when it stops without a clean shutdown. In every case `main` should exit with a failure code:

- `NotLaunchedByHub`: the magic cookie is missing, so someone ran the binary by hand. `serve` has already printed one line to stderr (the text is in `NOT_LAUNCHED_BY_HUB`), so print nothing more.
- `Socket(error)`: `serve` could not register its signal handlers or create the socket, for example because `TMPDIR` does not exist or the path is too long. Nothing was written to stdout. Log the error.
- `Transport(error)`: the gRPC server failed after the handshake. Log the error.

The `Plugin` trait and its messages are in `monitor_hub_plugin::proto`. The rules for `Events`, `seq`, `after` and `UNAVAILABLE` are in the protocol document.

### `launched_by_hub`

`serve` checks the cookie itself. Call `launched_by_hub()` yourself only when your `main` does work before `serve` that must not happen on a manual run, such as opening a database or applying migrations. Check it first and exit when it returns `false`.

## Logging

```rust
monitor_hub_plugin::logging::init("info")?;
```

`init` installs a global `tracing` subscriber that writes one JSON object per line to stderr, in the format the hub parses: `@level`, `@message`, `@module` and `@timestamp`, plus one key per event field. The level is `trace`, `debug`, `info`, `warn` or `error`, in any letter case. A field named like one of those keys, or like one of the names the hub renames (`plugin`, `module`, `time`, `level`, `msg`, `source`), is written as `field_<name>`. A line is at most 64 KiB: a longer message is cut and marked `[truncated]`. `init` also installs a panic hook. A panic in any thread or task is reported as one `error` line with its location and message. The hook changes nothing else: a panic in `main` still ends the process, and one in a spawned task or thread does not by itself.

`init` returns `LoggingError::UnknownLevel` for any other level and `LoggingError::AlreadyInitialized` when a global subscriber already exists. When it fails nothing is installed, so a `tracing::error!` at that point goes nowhere. A plugin that reads its level from configuration should print the error to stderr itself and exit, as the example above does.

Only `tracing` events are captured. Crates that log through the `log` crate, such as `tokio-postgres`, write nothing unless your plugin bridges them to `tracing` itself.

## Migrations

Enable the feature:

```toml
monitor-hub-plugin = { git = "https://github.com/ItzTas/monitor-hub", tag = "sdk-rust/v0.1.0", features = ["postgres"] }
```

`monitor_hub_plugin::migrate` applies goose migration files and records them in the same `goose_db_version` table goose uses, so a database can move between this runner and goose. Embed the files in the binary:

```rust
use monitor_hub_plugin::migrate::{MigrateError, Migration, up};

const MIGRATIONS: &[Migration] = &[Migration {
    version: 1,
    name: "00001_create_items.sql",
    sql: include_str!("../migrations/00001_create_items.sql"),
}];

async fn migrate(url: &str) -> Result<Vec<i64>, MigrateError> {
    let (mut client, connection) = tokio_postgres::connect(url, tokio_postgres::NoTls).await?;
    tokio::spawn(async move {
        let _ = connection.await;
    });
    up(&mut client, MIGRATIONS).await
}
```

`up` returns the versions it applied, in ascending order. `version` is the number in the file name. It must be at least 1 and unique.

### File format

A file starts with `-- +goose Up` and may have a `-- +goose Down` section. A statement ends with a semicolon at the end of a line. Wrap a statement that contains semicolons, such as a function body, between `-- +goose StatementBegin` and `-- +goose StatementEnd`. Annotations are matched case-insensitively and must start at the first column. The `Down` section is never run, but it is parsed and must be valid.

`-- +goose NO TRANSACTION`, `-- +goose ENVSUB ON`, `-- +goose ENVSUB OFF` and any other annotation are rejected with `MigrateError::Unsupported`.

### Behavior

- Each migration runs in its own transaction together with its row in `goose_db_version`. The table is created, with its version 0 row, when it does not exist.
- A version counts as applied when it has any row in `goose_db_version`, as in goose's provider.
- An unapplied migration with a lower version than an applied one fails with `OutOfOrder`. An applied version that has no file in your list is fine.
- Every migration in the list is parsed before anything touches the database, including the ones already applied. A file that uses a rejected annotation makes `up` fail even if goose applied it long ago. Leaving that file out of the list is safe.
- Runners take the same session-level advisory lock as goose (id 4097083626). A second runner retries every 5 seconds for about 5 minutes, then fails with `LockTimeout`.
- The lock is held for the whole run and released at the end of `up`. If the `up` future is dropped or cancelled after the lock is taken, for example by a timeout or a shutdown, the lock stays held for as long as that connection stays open, and every other runner, goose included, waits and then fails. Give `up` a connection of its own, and when you time it out or cancel it, drop that client instead of reusing it.
- When a migration fails, the error does not say which earlier versions of the same call were committed. They stay committed, and calling `up` again continues from the right place.

### Errors

| Variant | Meaning |
|---|---|
| `Unsupported { migration, annotation }` | The file uses an annotation the runner does not support. |
| `Invalid { migration, reason }` | The file is malformed, or its version is below 1 or used twice. |
| `OutOfOrder { missing, current }` | An unapplied version is lower than the database's current version. |
| `MissingZeroVersion` | `goose_db_version` exists but has no rows. |
| `LockTimeout` | Another process held the lock for more than 5 minutes. |
| `Failed { migration, source }` | A statement in that migration failed. Its transaction was rolled back. |
| `Database` | The runner's own queries failed. |

## Testing with the testkit

### A database

```rust
let Some(database) = monitor_hub_plugin_testkit::database().await else {
    return;
};
let (client, connection) = tokio_postgres::connect(&database.url, tokio_postgres::NoTls).await?;
```

`database()` returns a new, empty database on every call and drops it when the `Database` value is dropped. If `TEST_DATABASE_URL` is set to a `postgres://` URL, it creates the databases on that server. Otherwise it starts a private cluster with `initdb` and `postgres`, which must be on `PATH`. When neither is available it prints one line to stderr and returns `None`, so the test skips. When the `CI` environment variable is set and not empty, it panics instead, so CI cannot skip the test silently.

### The conformance suite

`monitor_hub_plugin_testkit::conformance::run` runs the hub's `plugin-conformance` command against your built binary. [Conformance](../docs/protocol.md#conformance) lists what it checks. Put the test in `tests/`, so Cargo builds the binary first:

```rust
use std::time::Duration;

use monitor_hub_plugin_testkit::conformance::{Conformance, Outcome, run};

#[test]
fn the_plugin_conforms() {
    let conformance = Conformance {
        command: vec![env!("CARGO_BIN_EXE_my-plugin").into()],
        env: vec![],
        min_events: 1,
        ready_timeout: Duration::from_secs(10),
    };
    match run(&conformance) {
        Outcome::Passed => {}
        Outcome::Skipped(reason) => eprintln!("skipped: {reason}"),
        Outcome::Failed(output) => panic!("{output}"),
    }
}
```

`command` is the plugin's command line. `env` is the only environment the plugin receives. `min_events` and `ready_timeout` become the `-min-events` and `-ready-timeout` flags. The run is stopped after `ready_timeout` plus two minutes, and no plugin process is left behind.

`run` finds the command in this order:

1. The `PLUGIN_CONFORMANCE` environment variable: the path of an already built `plugin-conformance` binary.
2. `go install github.com/ItzTas/monitor-hub/sdk/cmd/plugin-conformance@v0.2.0` into a temporary directory, once per process, then that binary, when `go` is on `PATH`. The Go tag is `sdk/v0.2.0`, and the constant `SDK_GO_VERSION` holds it.
3. Otherwise `Outcome::Skipped`, or a panic when `CI` is set and not empty.

`Outcome::Failed` carries one of three things:

- The conformance command's stderr, one line per violation.
- A message that starts with "the conformance command" and says it could not be fetched or built, followed by the Go tool's output. This happens in the `go` mode, before your plugin is started.
- A message about an unexpected end: the command was called wrongly (a bug in the runner, not in your plugin), did not finish in time, could not be started, or was killed.

## Working on the SDK

From the root of the monitor-hub repository:

```
make generate-rust
make check-rust
```

`make generate-rust` regenerates `crates/plugin/src/generated/` from `proto/monitorhub/plugin/v1/plugin.proto` and `crates/plugin/proto/controller.proto`. It needs `protoc`. Never edit the generated files by hand.

`make check-rust` runs `cargo fmt --all --check`, `cargo clippy --workspace --all-targets --all-features -- -D warnings` and `cargo test --workspace --all-features`. The tests need `protoc`, Postgres and Go. Locally they skip what is missing. Under `CI` they fail.
