# monitor-hub Rust SDK

Two crates for writing monitor-hub monitors in Rust. The monitor protocol itself is described in [`docs/protocol.md`](../docs/protocol.md).

- `monitor-hub-sdk` runs the monitor side of the protocol: the magic cookie check, the Unix socket, the handshake line, the gRPC health service, `Watch` and shutdown. It also writes logs in the format the hub reads, reads the configuration document, and answers `validate-settings` for monitors that declare their settings.
- `monitor-hub-sdk-testkit` is for your tests: a runner for the hub's conformance suite, and checks for `settings.json`, `validate-settings` and the configuration that reads them.

## Setup

Both crates are versioned together and published as git tags of this repository. Depend on them by tag:

```toml
[dependencies]
monitor-hub-sdk = { git = "https://github.com/ItzTas/monitor-hub", tag = "sdk-rust/v0.5.0" }
prost-types = "0.14.4"
tokio = { version = "1.54.1", features = ["macros", "rt-multi-thread", "time"] }
tonic = { version = "0.14.8", default-features = false, features = ["codegen"] }
tracing = "0.1.44"

[dev-dependencies]
monitor-hub-sdk-testkit = { git = "https://github.com/ItzTas/monitor-hub", tag = "sdk-rust/v0.5.0" }
```

Your crate implements the SDK's `Monitor` trait with `#[tonic::async_trait]` and builds messages with prost, so it needs its own `tonic` and `prost-types` dependencies, in the same 0.14 series as the SDK. This workspace uses `tonic` 0.14.8 and `prost-types` 0.14.4. `tracing` is what you log with.

Both crates build on Unix systems only.

The SDK pins its toolchain in [`rust-toolchain.toml`](rust-toolchain.toml): Rust 1.99.1 with `rustfmt` and `clippy`, edition 2024. Pin the same version in your monitor's repository so both build with the same compiler.

## Changes from 0.4

0.5 reads `settings.json` version 2 and adds the [`config`](#reading-the-configuration) module, which reads the configuration document. These changes break code written for 0.4:

- `settings::parse` refuses `settings_version` 1 with `SettingsError::UnsupportedVersion`, like every version other than 2.
- `Problem` has `path` in place of `name`, and an answer writes `path`.
- `Field.default` is a `serde_json::Value`, not a string, and `Field.editable` is an `Option<bool>`.
- `SettingsError::Field` has a `path` such as `fields[2].fields[0]` in place of an `index`.
- `FieldProblem` follows the version 2 rules. For example, `ReservedName` is now `EnvReserved`, about `env`.
- The testkit's `assert_no_drift` takes your configuration type, `assert_no_drift::<Config>(settings_json)`, in place of a list of names.

## A minimal monitor

A complete monitor that reports one fixed event. It is the `fixed` example in [`crates/sdk/examples/fixed.rs`](crates/sdk/examples/fixed.rs), cut down to one event.

```rust
use std::process::ExitCode;

use monitor_hub_sdk::proto::{
    CheckRequest, CheckResponse, DescribeRequest, DescribeResponse, Event, EventKind,
    EventsRequest, EventsResponse, Severity, State, StatusRequest, StatusResponse,
};
use monitor_hub_sdk::{Monitor, Notifier, ServeError, logging, serve};
use prost_types::Timestamp;
use tonic::{Request, Response, Status};

struct Ping;

#[tonic::async_trait]
impl Monitor for Ping {
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
    let notifier = Notifier::new();
    notifier.events_recorded(1);
    match serve(Ping, notifier).await {
        Ok(()) => ExitCode::SUCCESS,
        Err(ServeError::NotLaunchedByHub) => ExitCode::FAILURE,
        Err(err) => {
            tracing::error!(error = &err as &dyn std::error::Error, "serve failed");
            ExitCode::FAILURE
        }
    }
}
```

`serve` takes the monitor and a `Notifier`. It does the whole startup described in the protocol, answers `Watch` from the notifier (see [Signalling the hub](#signalling-the-hub)), and runs until the hub asks the monitor to stop or the process receives SIGTERM or SIGINT. `Ping` holds one event with `seq` 1, so `main` reports 1 as the highest `seq` before it serves. Nothing but the handshake line may go to stdout, so never `println!` in a monitor.

`serve` returns a `ServeError` when it stops without a clean shutdown. In every case `main` should exit with a failure code:

- `NotLaunchedByHub`: the magic cookie is missing, so someone ran the binary by hand. `serve` has already printed one line to stderr (the text is in `NOT_LAUNCHED_BY_HUB`), so print nothing more.
- `Signals(error)`: `serve` could not register its SIGTERM and SIGINT handlers. Nothing was written to stdout.
- `Socket(error)`: `serve` could not create the socket, for example because `TMPDIR` does not exist or the path is too long. Nothing was written to stdout.
- `Handshake(error)`: `serve` could not write the handshake line to stdout. It removes the socket before returning.
- `Transport(error)`: the gRPC server failed after the handshake.

Log every variant except `NotLaunchedByHub`, as the example does. The enum is `#[non_exhaustive]`, so a `match` on it needs a final `Err(err)` arm, and a later patch release may add variants.

The trait is `monitor_hub_sdk::Monitor`, and its messages are in `monitor_hub_sdk::proto`. Implement it with `#[tonic::async_trait]` on the `impl` block, as the example does. It has four methods, `describe`, `status`, `check` and `events`; `serve` answers `Watch` itself. The rules for `Events`, `seq`, `after` and `UNAVAILABLE` are in the protocol document.

### `launched_by_hub`

`serve` checks the cookie itself. Call `launched_by_hub()` yourself only when your `main` does work before `serve` that must not happen on a manual run, such as opening a database or applying migrations. Check it first. When it returns `false`, print `NOT_LAUNCHED_BY_HUB` on stderr and exit with code 1, as `serve` would:

```rust
if !monitor_hub_sdk::launched_by_hub() {
    eprintln!("{}", monitor_hub_sdk::NOT_LAUNCHED_BY_HUB);
    return ExitCode::FAILURE;
}
```

## Start-up time

The hub waits 60 seconds for the handshake line, which `serve` prints. When the line has not arrived by then, the hub kills the monitor's process group and starts the monitor again after a delay that grows from 1 second to 5 minutes. Each restart repeats everything `main` does before `serve`. A monitor that connects to a database or applies migrations has two ways to stay inside that budget.

### Prepare, then serve

This is the simple default. `main` calls a `prepare` function before `serve`, with a limit on the whole call, and exits on failure. `prepare` is your own function: it connects to the database, applies your migrations with the library you chose, and reads the highest `seq` the monitor has issued.

```rust
use std::error::Error;
use std::process::ExitCode;
use std::time::Duration;

use monitor_hub_sdk::{NOT_LAUNCHED_BY_HUB, Notifier, ServeError, launched_by_hub, logging, serve};
use tokio::time::timeout;

const PREPARE_LIMIT: Duration = Duration::from_secs(45);

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
    let (client, latest_seq) = match timeout(PREPARE_LIMIT, prepare(&url)).await {
        Ok(Ok(prepared)) => prepared,
        Ok(Err(err)) => {
            tracing::error!(error = &*err as &dyn Error, "cannot prepare the database");
            return ExitCode::FAILURE;
        }
        Err(_) => {
            tracing::error!("preparing the database took longer than {PREPARE_LIMIT:?}");
            return ExitCode::FAILURE;
        }
    };
    let notifier = Notifier::new();
    notifier.events_recorded(latest_seq);
    match serve(Items { client }, notifier).await {
        Ok(()) => ExitCode::SUCCESS,
        Err(ServeError::NotLaunchedByHub) => ExitCode::FAILURE,
        Err(err) => {
            tracing::error!(error = &err as &dyn Error, "serve failed");
            ExitCode::FAILURE
        }
    }
}

async fn prepare(url: &str) -> Result<(Client, i64), Box<dyn Error + Send + Sync>> {
    // Connect, apply your migrations and read the highest seq issued so far.
    todo!()
}
```

`Items` stands for your type that implements `Monitor`, and `Client` for the database client it holds. Nothing else bounds these steps, so:

- Give each connection its own time limit, because many drivers have none by default. Against a server that does not answer, such a connect waits until the hub kills the monitor.
- A migration lock taken by a preparation that the limit cut stays held while its connection is open. Exiting the process closes the connection and releases it.

### Serve, then prepare

Pick this shape when preparation can take longer than the budget, for example a migration that rebuilds a large index. `main` spawns a task that connects and migrates, then calls `serve` at once, so the handshake goes out within milliseconds. The task hands its result to the monitor, for example through a `tokio::sync::watch` channel or a `tokio::sync::OnceCell`. Until the result is there:

- `events` returns `Err(Status::unavailable(...))`, so the hub does not take an empty answer as its baseline.
- `status` returns `STATE_FAILING` with the reason in `message`.
- `check` returns `accepted: false` with the reason in `message`.
- `describe` answers as usual. The hub calls it on every poll and skips the poll when it fails.

When preparation is done, the task calls `events_recorded` with the highest `seq` the monitor has issued, and the signal that goes out tells the hub about any events it has not stored yet. When preparation fails, log the error, keep reporting it through `status`, and retry in the task if retrying can help. In the conformance test, set `ready_timeout` long enough to cover preparation.

## Signalling the hub

`serve` answers [`Watch`](../docs/protocol.md#watch) from the `Notifier` you pass it. The rest of the monitor uses clones of that notifier to tell the hub when to poll, so a notification goes out as soon as the monitor records an event:

```rust
use std::error::Error;
use std::process::ExitCode;
use std::time::Duration;

use monitor_hub_sdk::{NOT_LAUNCHED_BY_HUB, Notifier, ServeError, launched_by_hub, logging, serve};

async fn run_cycles(notifier: Notifier) {
    loop {
        if let Err(err) = cycle(&notifier).await {
            tracing::error!(error = &*err as &dyn Error, "cycle failed");
        }
        tokio::time::sleep(Duration::from_secs(30 * 60)).await;
    }
}

async fn cycle(notifier: &Notifier) -> Result<(), Box<dyn Error + Send + Sync>> {
    let _cycle = notifier.status_scope();
    if let Some(seq) = check_and_record().await? {
        notifier.events_recorded(seq);
    }
    Ok(())
}

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
    let notifier = Notifier::new();
    notifier.events_recorded(highest_issued_seq().await);
    tokio::spawn(run_cycles(notifier.clone()));
    match serve(Ping, notifier).await {
        Ok(()) => ExitCode::SUCCESS,
        Err(ServeError::NotLaunchedByHub) => ExitCode::FAILURE,
        Err(err) => {
            tracing::error!(error = &err as &dyn Error, "serve failed");
            ExitCode::FAILURE
        }
    }
}
```

`Ping` is the monitor from [A minimal monitor](#a-minimal-monitor). `check_and_record` and `highest_issued_seq` stand for your own code. The first runs a cycle and returns the highest `seq` it committed, or `None` when it recorded nothing. The second returns the highest `seq` the monitor has issued so far, or 0. The cycles start only after the cookie check, so a manual run does no work (see [`launched_by_hub`](#launched_by_hub)).

- `Notifier::new()` creates the notifier. Give a clone to every part of the monitor that records events or changes what `status` answers. All clones share one value.
- Call `events_recorded(seq)` once before `serve`, with the highest `seq` the monitor has issued. The first signal of every `Watch` call carries the current value.
- Call `events_recorded(seq)` after each write of events is committed, with the highest `seq` written. Never call it before the commit. The notifier keeps the highest value it was given, so a lower `seq` does not lower it, and every call sends a signal.
- Call `status_scope()` at the start of a cycle and keep what it returns until the cycle ends. It signals when it is created and again when it is dropped, however the cycle ends: normally, through `?`, or in a panic. Bind it to a name such as `_cycle`, as the example does. `let _ = notifier.status_scope();` drops the scope at once, so both signals go out right there and none when the cycle ends. A bare `notifier.status_scope();` gets a compiler warning, because `StatusScope` is `#[must_use]`. The compiler suggests `let _ = ...` to silence it, which is the mistake above: bind the scope to a name instead.
- Call `status_changed()` for a one-off change outside a cycle, such as a pause, a failure or a return to normal. It sends a signal with the same `latest_seq`.

The SDK sends the first signal as soon as the hub opens the call. While the hub reads slowly, signals that pile up are merged: at most one waits to be read, and the one sent after it carries the newest `latest_seq`. When 30 seconds pass without a signal, the SDK sends a heartbeat. When the monitor shuts down, it ends every open `Watch` call.

`events_recorded`, `status_scope` and `status_changed` never block and never fail, even when no one is watching.

In tests, `notifier.subscribe()` returns `Signals`. `latest_seq()` returns the current value and marks it seen. `changed().await` waits for a signal not yet seen and returns the `latest_seq` at that moment. Wrap it in a `timeout`, because it never returns once every clone of the `Notifier` is gone. In this test, `record_one_event` stands for the code under test, which commits the event with `seq` 1 and calls `events_recorded(1)`:

```rust
use std::time::Duration;

use monitor_hub_sdk::Notifier;
use tokio::time::timeout;

#[tokio::test]
async fn recording_an_event_signals_the_hub() {
    let notifier = Notifier::new();
    let mut signals = notifier.subscribe();
    record_one_event(&notifier).await;
    let seq = timeout(Duration::from_secs(5), signals.changed())
        .await
        .expect("a signal within 5 seconds");
    assert_eq!(seq, 1);
}
```

## Logging

```rust
monitor_hub_sdk::logging::init("info")?;
```

`init` installs a global `tracing` subscriber that writes one JSON object per line to stderr, in the format the hub parses: `@level`, `@message`, `@module` and `@timestamp`, plus one key per event field. The level is `trace`, `debug`, `info`, `warn` or `error`, in any letter case. A field named like one of those keys, or like one of the names the hub renames (`monitor`, `module`, `time`, `level`, `msg`, `source`), is written as `field_<name>`. A line is at most 64 KiB: a longer message is cut and marked `[truncated]`.

`init` also installs a panic hook, which replaces any hook installed before it. A panic in any thread or task is reported as one `error` line whose `@message` is `panic: <payload>`, with the panic's location in a `location` field. The hook changes nothing else: a panic in `main` still ends the process, and one in a spawned task or thread does not by itself.

Log an error as a field cast to `&dyn std::error::Error`:

```rust
tracing::error!(error = &err as &dyn std::error::Error, "serve failed");
```

The field then holds the error's whole `source()` chain, joined with `": "`, for example `cannot create the monitor socket: <cause>`. A cause whose text the line already ends with is written once. Logging the error with `%err` or `"{err}"` writes only the first link of that chain. For `ServeError` the first link leaves out the reason: `"{err}"` on a `ServeError::Socket` prints only `cannot create the monitor socket`.

`init` returns `LoggingError::UnknownLevel` for any other level and `LoggingError::AlreadyInitialized` when a global subscriber already exists. When it fails nothing is installed, so a `tracing::error!` at that point goes nowhere. A monitor that reads its level from configuration should print the error to stderr itself and exit, as the example above does.

Only `tracing` events are captured. Crates that log through the `log` crate write nothing unless your monitor bridges them to `tracing` itself.

## Settings

A monitor can declare its settings in a `settings.json` file next to `monitor.json`, read their values from one configuration document, and check a set of values when it is run with the `validate-settings` argument. [Settings](../docs/protocol.md#settings) in the protocol document has the file's format, the document and the rules for the answer. The `monitor_hub_sdk::config` module reads the document, and `monitor_hub_sdk::settings` covers the file and the answer.

### Reading the configuration

Embed `settings.json` in the binary, give it to `config::load`, and turn the document into a struct of your own with `serde_json::from_value`. Your crate needs `serde` with the `derive` feature and `serde_json` for that; this workspace uses 1.0.229 and 1.0.154. Look for `validate-settings` first in `main`, before logging starts and before anything checks the magic cookie, because the validator runs without the cookie. The [`fixed`](crates/sdk/examples/fixed.rs) example does the same:

```rust
use std::env;
use std::process::ExitCode;

use monitor_hub_sdk::settings::{self, Problem, VALIDATE_SETTINGS_ARG};
use monitor_hub_sdk::{Notifier, ServeError, config, logging, serve};
use serde::Deserialize;
use serde_json::Value;

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Config {
    #[serde(default)]
    watch_terms: Vec<String>,
    check_interval: String,
}

fn read_config() -> Result<Config, Vec<Problem>> {
    let document = config::load(include_str!("../settings.json")).map_err(|err| err.problems())?;
    let config: Config = serde_json::from_value(Value::Object(document))
        .map_err(|_| {
            vec![Problem::new("", "the configuration does not fit the monitor's configuration type")]
        })?;
    if config.watch_terms.is_empty() {
        return Err(vec![Problem::new("watch_terms", "set at least one term")]);
    }
    Ok(config)
}

#[tokio::main]
async fn main() -> ExitCode {
    if env::args_os().skip(1).eq([VALIDATE_SETTINGS_ARG]) {
        return settings::answer(&read_config().err().unwrap_or_default());
    }
    if let Err(err) = logging::init("info") {
        eprintln!("cannot start logging: {err}");
        return ExitCode::FAILURE;
    }
    let config = match read_config() {
        Ok(config) => config,
        Err(problems) => {
            let reasons: Vec<String> = problems
                .iter()
                .map(|problem| format!("{}: {}", problem.path, problem.message))
                .collect();
            tracing::error!(error = %reasons.join("; "), "invalid configuration");
            return ExitCode::FAILURE;
        }
    };
    tracing::info!(terms = config.watch_terms.len(), interval = %config.check_interval, "configured");
    let notifier = Notifier::new();
    notifier.events_recorded(1);
    match serve(Ping, notifier).await {
        Ok(()) => ExitCode::SUCCESS,
        Err(ServeError::NotLaunchedByHub) => ExitCode::FAILURE,
        Err(err) => {
            tracing::error!(error = &err as &dyn std::error::Error, "serve failed");
            ExitCode::FAILURE
        }
    }
}
```

`Ping` is the monitor from [A minimal monitor](#a-minimal-monitor), and the file declares `watch_terms` as a list of strings and `check_interval` as a duration with a default. `include_str!` takes a path relative to the source file, so `../settings.json` fits a file in `src/` and a `settings.json` at the crate root.

`config::load(settings_json)` parses the file. Then it reads the document from the descriptor that `MONITOR_HUB_CONFIG_FD` names, and closes the descriptor. When that variable is unset or empty, it builds the document from the environment through the env bridge instead. It applies the defaults either way and returns a `config::Document`, which is a `serde_json` map. The descriptor is read once per process: a second `load` that finds `MONITOR_HUB_CONFIG_FD` returns `LoadError::AlreadyTaken`, so load once and keep the result.

`load` does not check the values against their types, so a value of the wrong type stays in the document until `serde_json::from_value` refuses it. Put `#[serde(deny_unknown_fields)]` on every struct, so a key the file does not declare is refused too. The testkit then proves that the struct and the file agree (see [the testkit's Settings](#settings-1)). A field without a default that the document may leave out, such as `watch_terms` above, needs `#[serde(default)]` or an `Option`. Then your own check can report the missing value with its path, instead of `serde` failing with a general error.

`load` returns a `LoadError`:

- `Settings(error)`: the file does not parse.
- `Descriptor(text)`: `MONITOR_HUB_CONFIG_FD` is not a number of 3 or more.
- `AlreadyTaken`: the process has already read the descriptor.
- `Read(error)`: the descriptor cannot be read.
- `TooLarge`: the document is larger than 64 KiB.
- `NotAnObject`: the document is not a JSON object.
- `Environment(problems)`: environment variables hold text that does not convert, one `Problem` each.

`err.problems()` returns the `Environment` problems as they are. Any other error becomes one problem with an empty path, with the error and its causes as the message. Either way the result is ready for `settings::answer`. `LoadError` is `#[non_exhaustive]`, so a `match` on it needs a wildcard arm.

The steps of `load` are public too. `from_descriptor(settings, fd)` reads a document from a descriptor you own. `from_lookup(settings, lookup)` builds one through the bridge from any lookup, such as `|name| std::env::var_os(name)`. `with_defaults(settings, document)` applies the defaults, and `descriptor_number(text)` reads the variable's value. `CONFIG_FD_ENV` holds the variable's name and `DOCUMENT_LIMIT` the 64 KiB limit.

### Answering `validate-settings`

`settings::answer(problems)` writes `{"problems":[...]}` and a newline on stdout, flushes it, and returns the exit code: `0` when the list is empty, `1` when it is not, and `2` when stdout cannot be written. Write nothing else on stdout. `write_answer(out, problems)` does the same into any `Write` and returns the code as a `u8`. `VALIDATE_SETTINGS_LIMIT` (5 seconds) and `ANSWER_LIMIT` (65536 bytes) hold the protocol's limits.

`Problem::new(path, message)` builds one problem. `path` names the value it is about, such as `watch_terms` or `rules[2].process`, or is `""` for a problem with several fields together or with the whole document. Collect every problem before you answer, and never put the value of a secret field in a message. Errors from `serde` quote the value they refused, so never pass them on. The example above answers with a fixed message instead.

### Reading the file

`parse(bytes)` reads a `settings.json` by the protocol's rules. It returns `Settings`, with `settings_version` and `fields`, or the first error it finds as a `SettingsError`:

- `ByteOrderMark`: the file starts with a UTF-8 byte order mark.
- `Format(error)`: the JSON itself is wrong, for example an unknown attribute or `type`, a key written twice, or an array where an object belongs. The `serde_json` error is its source.
- `UnsupportedVersion(version)`: `settings_version` is not 2.
- `Field { path, problem }`: the field at `path`, such as `fields[2].items.fields[0]`, breaks a rule, described by a `FieldProblem`.

A `Field` holds the file's attributes, with `kind` for `type`. Its `default` is a `serde_json::Value`, and its `editable` is an `Option<bool>` that `is_editable()` reads. `fields` and `items` hold the fields of an object and the `Item` of a list, which has `kind`, `options` and `fields`. `SETTINGS_VERSION` is 2 and `MAX_DEPTH` is 4.

`Settings::declares(path)` says whether a path, such as `rules[2].process`, names a value of the document. The empty path does. `parse_duration(text)` reads Go's duration syntax, such as `1h30m`, into nanoseconds, or returns `None`; the rules use it to check a `duration` default. `is_reserved(name)` says whether the hub reserves an environment variable name.

`Settings`, `Field`, `Item`, `FieldType`, `SettingsError`, `FieldProblem`, `Problem` and `Answer` are `#[non_exhaustive]`. A `match` on `FieldType`, `SettingsError` or `FieldProblem` needs a wildcard arm, a pattern on `SettingsError::Field` needs `..`, and other crates build a `Problem` with `Problem::new`.

## Testing with the testkit

### The conformance suite

`monitor_hub_sdk_testkit::conformance::run` runs the hub's `monitor-conformance` command against your built binary. [Conformance](../docs/protocol.md#conformance) lists what it checks. Put the test in `tests/`, so Cargo builds the binary first:

```rust
use std::time::Duration;

use monitor_hub_sdk_testkit::conformance::{Conformance, Outcome, run};

#[test]
fn the_monitor_conforms() {
    let mut conformance = Conformance::new(env!("CARGO_BIN_EXE_my-monitor"));
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

`Conformance::new(program)` sets `command` to `[program]`, with no environment, `min_events` 0, `ready_timeout` 0 and no `monitor_dir`. Change the public fields by assignment: `Conformance` is `#[non_exhaustive]`, so other crates cannot build it with a struct literal, and `Outcome` is `#[non_exhaustive]` too, so a `match` on it needs a wildcard arm.

`command` is the monitor's command line, and arguments are pushed onto it. `min_events` and `ready_timeout` become the `-min-events` and `-ready-timeout` flags. The run is stopped after `ready_timeout` plus two minutes, and no monitor process is left behind.

`monitor_dir` becomes the `-monitor-dir` flag. Set it to the directory that holds your `settings.json`, for example `Some(env!("CARGO_MANIFEST_DIR").into())`. The command then reads that file by its own Go rules and runs the monitor's `validate-settings` in that directory. A monitor in any other language gets the same check from `monitor-conformance -monitor-dir`; for a Rust monitor it comes on top of `settings::check`.

`env` holds the monitor's environment. Besides it, the monitor gets only the handshake variables go-plugin sets (`MONITOR_HUB_MONITOR`, `PLUGIN_MIN_PORT`, `PLUGIN_MAX_PORT`, `PLUGIN_PROTOCOL_VERSIONS`). The hub would also pass `PATH`, `TZ`, `TMPDIR` and `MONITOR_HUB_DATA_DIR`; the conformance command does not, so add the ones the monitor needs to `env`. The monitor runs in the test's working directory, not in a monitor directory, and shares the conformance command's process group. Each entry of `env` reaches the command as a `-env NAME=value` argument, which other users of the same machine can read (with `ps`, or in `/proc/<pid>/cmdline`). Pass only throwaway test credentials, such as the URL of a test database.

`run` finds the command in this order:

1. The `MONITOR_CONFORMANCE` environment variable: the path of an already built `monitor-conformance` binary.
2. `go install github.com/ItzTas/monitor-hub/sdk-go/cmd/monitor-conformance@v0.7.0`, when `go` is on `PATH`. The Go tag is `sdk-go/v0.7.0`, and the constant `SDK_GO_VERSION` holds it. The binary goes to `monitor-hub/monitor-conformance/v0.7.0/` under a cache base: `$XDG_CACHE_HOME` when it is an absolute path, otherwise `$HOME/.cache` when `HOME` is absolute.
   - `run` uses the base only when it exists or can be created, is a real directory and not a symbolic link, belongs to you, and is not writable by group or others. It creates a missing base with mode 0700. Each directory below the base must belong to you too, and `run` sets it to mode 0700. `run` checks the base itself but not its parents: a base inside a directory that other users can rename is your responsibility.
   - The binary is installed once per version. Later test runs reuse it while it is a regular file that you own and can execute; otherwise `run` installs it again. `go install` writes to a staging directory next to the binary, which is then renamed into place, so test processes that install at the same time do not corrupt each other. `go install` is stopped after ten minutes, and the next install removes staging directories older than that, such as those left by a killed run.
   - In every other case the binary goes to a private temporary directory, one per test process, which stays behind after the process ends: when neither variable gives an absolute path, or when the base or a directory below it fails the checks above, for example `HOME=/` in a container started with `--user`. `run` then says once on stderr why it did not use the cache. Unsetting both `XDG_CACHE_HOME` and `HOME` always selects the temporary directory; `go` itself then needs `GOCACHE` and `GOPATH` set.
3. Otherwise `Outcome::Skipped`, or a panic when `CI` is set and not empty.

`Outcome::Failed` carries one of four things:

- A message that starts with `Conformance.` when a field of your `Conformance` is wrong: `command` is empty, a name in `env` is empty or holds `=`, or `monitor_dir` is not a directory. The command is not run.
- The conformance command's stderr: one line per violation, preceded by every line the monitor wrote to its stderr, each prefixed with `monitor: `. When the monitor cannot start, for example because its configuration is wrong or its database refuses the connection, its own error lines come just before the command's `monitor-conformance: ` line.
- A message that starts with "the conformance command" and says it could not be fetched or built, followed by the Go tool's output. This happens in the `go` mode, before your monitor is started.
- A message about an unexpected end: the command did not finish in time, could not be started, was killed, or rejected its arguments with exit code 2. The runner checks the arguments before it starts the command, so that last case is a bug in the runner, not in your monitor.

Of the command's stderr, and of the Go tool's output, only the last 1 MiB is kept. It starts at the beginning of a line whenever that last part holds a line break. When anything was cut, the first line is `(earlier output dropped)`.

### Settings

`monitor_hub_sdk_testkit::settings` has checks for a monitor that ships `settings.json`. The first runs your built binary, so put it in `tests/`:

```rust
use monitor_hub_sdk_testkit::settings::check;

#[test]
fn the_settings_contract_holds() {
    let violations = check(
        env!("CARGO_MANIFEST_DIR"),
        env!("CARGO_BIN_EXE_my-monitor"),
    );
    assert!(violations.is_empty(), "{violations:#?}");
}
```

`check(dir, program)` reads `dir/settings.json` with `parse`, then runs `program validate-settings` in `dir` with an empty environment and no descriptor, so the monitor builds its document from nothing through the env bridge. It returns one line per violation, and an empty list when both pass. It reports at most one problem with the file and one with the validator. A validator that exits with code 1 and lists problems passes: with an empty environment, that is the expected answer of a monitor with required settings. When the file parses, each problem's path must be empty or declared by the file. What the validator writes to stderr is read and thrown away. The file must exist: without it, `check` reports that it cannot read it. `check` stops a validator that runs longer than 5 seconds, together with its process group: it sends SIGTERM, then SIGKILL five seconds later, so a validator that ignores SIGTERM makes `check` take about 11 seconds.

`check` runs `program` exactly as given, made absolute against the current directory. A bare name such as `validator` becomes `<current directory>/validator` and is not looked up in `PATH`, so pass a full path, for example `env!("CARGO_BIN_EXE_<name>")`.

`assert_no_drift::<T>(settings_json)` proves that your configuration type `T` and the file agree. It panics unless `T` reads a document with a value for every declared field and refuses every document that adds a key the file does not declare. There is one such document for each object in the full document, nested ones included. It also panics when the file does not parse. Test it next to your configuration:

```rust
#[cfg(test)]
mod tests {
    #[test]
    fn settings_json_declares_what_the_configuration_reads() {
        let settings = include_str!("../settings.json");
        monitor_hub_sdk_testkit::settings::assert_no_drift::<super::Config>(settings);
    }
}
```

The second half can only pass when every struct of `T` has `#[serde(deny_unknown_fields)]`. For tests of your own, `full_document(settings_json)` returns the document with every field, and `undeclared_documents(settings_json)` returns the documents with an extra key, each with the path of that key. In the full document, a field holds its `default`, or else a sample of its type: `"x"`, `1`, `1.5`, `true`, `"1m"`, the first option, an object with every field, or a list of one item.

`command_with_document(program, args, document)` runs the monitor with a document on a descriptor, the way the hub will hand it over:

```rust
use monitor_hub_sdk_testkit::settings::command_with_document;

#[test]
fn the_validator_reads_the_document() {
    let document = br#"{"watch_terms": ["Paulo Santos"]}"#;
    let mut run = command_with_document(
        env!("CARGO_BIN_EXE_my-monitor"),
        &["validate-settings"],
        document,
    )
    .unwrap();
    let output = run.command.output().unwrap();
    assert_eq!(output.status.code(), Some(0));
}
```

It returns a `DocumentCommand`, whose `command` field is a `std::process::Command`. That command runs `program` with `args` through `/bin/sh`, with the document on descriptor 3 and `MONITOR_HUB_CONFIG_FD=3`. The environment starts empty: add variables with `env` or `envs`, and never call `env_clear`, which would remove `MONITOR_HUB_CONFIG_FD`. The document is a temporary file that goes away with the `DocumentCommand`, so keep that alive until the command has started.

The conformance command reads the file with its own rules, which accept a few things `parse` rejects (see [Settings](../docs/protocol.md#settings)). To run that check too, set `monitor_dir` on the conformance run.

## Working on the SDK

From the root of the monitor-hub repository:

```
make generate-rust
make check-rust
```

`make generate-rust` regenerates `crates/sdk/src/generated/` from `proto/monitorhub/monitor/v1/monitor.proto` and `crates/sdk/proto/controller.proto`. It needs `protoc`. Never edit the generated files by hand.

`make check-rust` runs `cargo fmt --all --check`, `cargo clippy --workspace --all-targets --all-features -- -D warnings` and `cargo test --workspace --all-features`. The tests need `protoc` and Go. Locally they skip what is missing. Under `CI` they fail.

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

To add or bump a dependency, update only what it needs and pin each new crate to a release that is old enough:

```
cargo update -p <crate> --precise <version>
```

Then check every new or changed entry in `Cargo.lock` against its publish date on crates.io. A monitor repository that copies this lock as its starting point follows the same rule.
