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
tokio = { version = "1.54.1", features = ["macros", "rt-multi-thread", "time"] }
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
            tracing::error!(error = &err as &dyn std::error::Error, "serve failed");
            ExitCode::FAILURE
        }
    }
}
```

`serve` does the whole startup described in the protocol, then runs until the hub asks the plugin to stop or the process receives SIGTERM or SIGINT. Nothing but the handshake line may go to stdout, so never `println!` in a plugin.

`serve` returns a `ServeError` when it stops without a clean shutdown. In every case `main` should exit with a failure code:

- `NotLaunchedByHub`: the magic cookie is missing, so someone ran the binary by hand. `serve` has already printed one line to stderr (the text is in `NOT_LAUNCHED_BY_HUB`), so print nothing more.
- `Signals(error)`: `serve` could not register its SIGTERM and SIGINT handlers. Nothing was written to stdout.
- `Socket(error)`: `serve` could not create the socket, for example because `TMPDIR` does not exist or the path is too long. Nothing was written to stdout.
- `Handshake(error)`: `serve` could not write the handshake line to stdout. It removes the socket before returning.
- `Transport(error)`: the gRPC server failed after the handshake.

Log every variant except `NotLaunchedByHub`, as the example does. The enum is `#[non_exhaustive]`, so a `match` on it needs a final `Err(err)` arm, and a later patch release may add variants.

The `Plugin` trait and its messages are in `monitor_hub_plugin::proto`. The rules for `Events`, `seq`, `after` and `UNAVAILABLE` are in the protocol document.

### `launched_by_hub`

`serve` checks the cookie itself. Call `launched_by_hub()` yourself only when your `main` does work before `serve` that must not happen on a manual run, such as opening a database or applying migrations. Check it first. When it returns `false`, print `NOT_LAUNCHED_BY_HUB` on stderr and exit with code 1, as `serve` would:

```rust
if !monitor_hub_plugin::launched_by_hub() {
    eprintln!("{}", monitor_hub_plugin::NOT_LAUNCHED_BY_HUB);
    return ExitCode::FAILURE;
}
```

## Start-up time

The hub waits 60 seconds for the handshake line, which `serve` prints. When the line has not arrived by then, the hub kills the plugin's process group and starts the plugin again after a delay that grows from 1 second to 5 minutes. Each restart repeats everything `main` does before `serve`. A plugin that connects to a database or applies migrations has two ways to stay inside that budget.

### Prepare, then serve

This is the simple default. `main` connects and migrates before `serve`, with a limit on each connect and one on the whole sequence, and exits on failure:

```rust
use std::error::Error;
use std::process::ExitCode;
use std::time::Duration;

use monitor_hub_plugin::migrate::{Migration, up};
use monitor_hub_plugin::{NOT_LAUNCHED_BY_HUB, ServeError, launched_by_hub, logging, serve};
use tokio::time::timeout;
use tokio_postgres::{Client, NoTls};

const CONNECT_LIMIT: Duration = Duration::from_secs(10);
const PREPARE_LIMIT: Duration = Duration::from_secs(45);
const MIGRATIONS: &[Migration] = &[];

#[tokio::main]
async fn main() -> ExitCode {
    if !launched_by_hub() {
        eprintln!("{NOT_LAUNCHED_BY_HUB}");
        return ExitCode::FAILURE;
    }
    if let Err(err) = logging::init("info") {
        eprintln!("cannot start logging: {err}");
        return ExitCode::FAILURE;
    }
    let url = std::env::var("DATABASE_URL").unwrap_or_default();
    let client = match timeout(PREPARE_LIMIT, prepare(&url)).await {
        Ok(Ok(client)) => client,
        Ok(Err(err)) => {
            tracing::error!(error = &*err as &dyn Error, "cannot prepare the database");
            return ExitCode::FAILURE;
        }
        Err(_) => {
            tracing::error!("preparing the database took longer than {PREPARE_LIMIT:?}");
            return ExitCode::FAILURE;
        }
    };
    match serve(Items { client }).await {
        Ok(()) => ExitCode::SUCCESS,
        Err(ServeError::NotLaunchedByHub) => ExitCode::FAILURE,
        Err(err) => {
            tracing::error!(error = &err as &dyn Error, "serve failed");
            ExitCode::FAILURE
        }
    }
}

async fn prepare(url: &str) -> Result<Client, Box<dyn Error + Send + Sync>> {
    let (mut client, connection) =
        timeout(CONNECT_LIMIT, tokio_postgres::connect(url, NoTls)).await??;
    tokio::spawn(async move {
        if let Err(err) = connection.await {
            tracing::error!(error = &err as &dyn Error, "database connection failed");
        }
    });
    up(&mut client, MIGRATIONS).await?;
    Ok(client)
}
```

`Items` stands for your type that implements `Plugin`. The limits matter because nothing else bounds these steps:

- `tokio_postgres::connect` has no time limit by default. Against a server that does not answer, it waits until the hub kills the plugin.
- `migrate::up` waits up to about 5 minutes for another runner's lock before it returns `MigrateError::LockTimeout`. Under the hub that error never arrives in this shape: the 45-second limit fires first, or else the hub's 60 seconds. When the limit fires, the process exits, which closes the connection and releases the lock.

### Serve, then prepare

Pick this shape when preparation can take longer than the budget, for example a migration that rebuilds a large index. `main` spawns a task that connects and migrates, then calls `serve` at once, so the handshake goes out within milliseconds. The task hands its result to the plugin, for example through a `tokio::sync::watch` channel or a `tokio::sync::OnceCell`. Until the result is there:

- `events` returns `Err(Status::unavailable(...))`, so the hub does not take an empty answer as its baseline.
- `status` returns `STATE_FAILING` with the reason in `message`.
- `check` returns `accepted: false` with the reason in `message`.
- `describe` answers as usual. The hub calls it on every poll and skips the poll when it fails.

When preparation fails, log the error, keep reporting it through `status`, and retry in the task if retrying can help. In the conformance test, set `ready_timeout` long enough to cover preparation.

## Logging

```rust
monitor_hub_plugin::logging::init("info")?;
```

`init` installs a global `tracing` subscriber that writes one JSON object per line to stderr, in the format the hub parses: `@level`, `@message`, `@module` and `@timestamp`, plus one key per event field. The level is `trace`, `debug`, `info`, `warn` or `error`, in any letter case. A field named like one of those keys, or like one of the names the hub renames (`plugin`, `module`, `time`, `level`, `msg`, `source`), is written as `field_<name>`. A line is at most 64 KiB: a longer message is cut and marked `[truncated]`.

`init` also installs a panic hook, which replaces any hook installed before it. A panic in any thread or task is reported as one `error` line whose `@message` is `panic: <payload>`, with the panic's location in a `location` field. The hook changes nothing else: a panic in `main` still ends the process, and one in a spawned task or thread does not by itself.

Log an error as a field cast to `&dyn std::error::Error`:

```rust
tracing::error!(error = &err as &dyn std::error::Error, "migration failed");
```

The field then holds the error's whole `source()` chain, joined with `": "`, for example `migration 00002_items.sql failed: db error: ERROR: relation "items" already exists`. A cause whose text the line already ends with is written once. Logging the error with `%err` or `"{err}"` writes only the top error, which for `MigrateError` and `tokio_postgres::Error` leaves out the reason.

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
- Runners take the same session-level advisory lock as goose (id 4097083626). A second runner retries every 5 seconds for about 5 minutes, then fails with `LockTimeout`. Before `serve`, the hub's 60-second start-up budget runs out long before that (see [Start-up time](#start-up-time)).
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

`MigrateError` is `#[non_exhaustive]`: a `match` on it needs a wildcard arm. So does one on `logging::LoggingError`.

## Testing with the testkit

### A database

```rust
#[tokio::test]
async fn items_are_stored() {
    let Some(database) = monitor_hub_plugin_testkit::database().await else {
        return;
    };
    let (client, connection) = tokio_postgres::connect(&database.url, tokio_postgres::NoTls)
        .await
        .expect("connect to the test database");
    tokio::spawn(connection);
    client
        .batch_execute("CREATE TABLE items (id integer)")
        .await
        .expect("create a table");
}
```

`database()` returns a new, empty database on every call and drops it when the `Database` value is dropped. If `TEST_DATABASE_URL` is set to a `postgres://` URL, it creates the databases on that server. Otherwise it starts a private cluster with `initdb` and `postgres`, which must be on `PATH`. When neither is available it prints one line to stderr and returns `None`, so the test skips. When the `CI` environment variable is set and not empty, it panics instead, so CI cannot skip the test silently.

### The conformance suite

`monitor_hub_plugin_testkit::conformance::run` runs the hub's `plugin-conformance` command against your built binary. [Conformance](../docs/protocol.md#conformance) lists what it checks. Put the test in `tests/`, so Cargo builds the binary first:

```rust
use std::time::Duration;

use monitor_hub_plugin_testkit::conformance::{Conformance, Outcome, run};

#[test]
fn the_plugin_conforms() {
    let mut conformance = Conformance::new(env!("CARGO_BIN_EXE_my-plugin"));
    conformance.env.push(("EVENTS".into(), "2".into()));
    conformance.min_events = 1;
    conformance.ready_timeout = Duration::from_secs(10);
    match run(&conformance) {
        Outcome::Passed => {}
        Outcome::Skipped(reason) => eprintln!("skipped: {reason}"),
        Outcome::Failed(output) => panic!("{output}"),
        _ => panic!("unexpected outcome"),
    }
}
```

`Conformance::new(program)` sets `command` to `[program]`, with no environment, `min_events` 0 and `ready_timeout` 0. Change the public fields by assignment: `Conformance` is `#[non_exhaustive]`, so other crates cannot build it with a struct literal, and `Outcome` is `#[non_exhaustive]` too, so a `match` on it needs a wildcard arm.

`command` is the plugin's command line, and arguments are pushed onto it. `min_events` and `ready_timeout` become the `-min-events` and `-ready-timeout` flags. The run is stopped after `ready_timeout` plus two minutes, and no plugin process is left behind.

`env` holds the plugin's environment. Besides it, the plugin gets only the handshake variables go-plugin sets (`MONITOR_HUB_PLUGIN`, `PLUGIN_MIN_PORT`, `PLUGIN_MAX_PORT`, `PLUGIN_PROTOCOL_VERSIONS`). The hub would also pass `PATH`, `TZ`, `TMPDIR` and `MONITOR_HUB_DATA_DIR`; the conformance command does not, so add the ones the plugin needs to `env`. The plugin runs in the test's working directory, not in a plugin directory, and shares the conformance command's process group. Each entry of `env` reaches the command as a `-env NAME=value` argument, which other users of the same machine can read (with `ps`, or in `/proc/<pid>/cmdline`). Pass only throwaway test credentials, such as the URL of a test database from `database()`.

`run` finds the command in this order:

1. The `PLUGIN_CONFORMANCE` environment variable: the path of an already built `plugin-conformance` binary.
2. `go install github.com/ItzTas/monitor-hub/sdk/cmd/plugin-conformance@v0.2.0`, when `go` is on `PATH`. The Go tag is `sdk/v0.2.0`, and the constant `SDK_GO_VERSION` holds it. The binary goes to `$XDG_CACHE_HOME/monitor-hub/plugin-conformance/v0.2.0/`, or `$HOME/.cache/monitor-hub/plugin-conformance/v0.2.0/` when `XDG_CACHE_HOME` is unset or not an absolute path. It is installed once per version and reused by every later test run. `run` creates each of those directories with mode 0700 and refuses one that is a symbolic link or belongs to another user. `go install` writes to a temporary directory next to the binary, which is then renamed into place, so test processes that install at the same time do not corrupt each other. `go install` is stopped after ten minutes. When neither `XDG_CACHE_HOME` nor `HOME` is usable, the binary goes to a private temporary directory, one per test process, which stays behind after the process ends.
3. Otherwise `Outcome::Skipped`, or a panic when `CI` is set and not empty.

`Outcome::Failed` carries one of three things:

- The conformance command's stderr: one line per violation, preceded by every line the plugin wrote to its stderr, each prefixed with `plugin: `. When the plugin cannot start, for example because its configuration is wrong or its database refuses the connection, its own error lines come just before the command's `plugin-conformance: ` line.
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

### `Cargo.lock`

Every crate in `Cargo.lock` must have been published at least two weeks before it entered the lock. A bare `cargo update` breaks that rule, because it moves every crate to its newest compatible release. These crates are held below their newest release on purpose:

| Crate | Locked | Newer release left out when the lock was made |
|---|---|---|
| `tonic`, `tonic-health`, `tonic-prost`, `tonic-prost-build`, `tonic-build` | 0.14.8 | 0.14.9 |
| `prost`, `prost-types`, `prost-derive`, `prost-build` | 0.14.4 | 0.14.5 |
| `fixedbitset` | 0.5.7 | 0.5.9 |
| `itoa` | 1.0.18 | 1.0.19 |
| `log` | 0.4.34 | 0.4.35 |
| `mime` | 0.3.17 | 0.3.18 |
| `smallvec` | 1.16.4 | 1.16.5 |
| `syn` (3.x) | 3.0.9 | 3.0.10 |
| `const-oid` | 0.10.2 | 0.10.3 |
| `ctutils` | 0.4.6 | 0.4.8 |
| `unicode-properties` | 0.1.4 | 0.1.5 |
| `wasm-bindgen`, `wasm-bindgen-macro`, `wasm-bindgen-macro-support`, `wasm-bindgen-shared` | 0.2.129 | 0.2.130 |
| `js-sys`, `web-sys` | 0.3.106 | 0.3.107 |

To add or bump a dependency, update only what it needs and pin each new crate to a release that is old enough:

```
cargo update -p <crate> --precise <version>
```

Then check every new or changed entry in `Cargo.lock` against its publish date on crates.io. A plugin repository that copies this lock as its starting point follows the same rule.
