use std::env;
use std::error::Error;
use std::process::ExitCode;

use monitor_hub_sdk::config;
use monitor_hub_sdk::logging::{self, LoggingError};
use monitor_hub_sdk::proto::{
    CheckRequest, CheckResponse, DescribeRequest, DescribeResponse, Event, EventKind,
    EventsRequest, EventsResponse, Severity, State, StatusRequest, StatusResponse,
};
use monitor_hub_sdk::settings::{self, Problem, VALIDATE_SETTINGS_ARG};
use monitor_hub_sdk::{Monitor, Notifier, ServeError, serve};
use prost_types::Timestamp;
use serde::Deserialize;
use serde_json::Value;
use tonic::{Request, Response, Status};

const SETTINGS: &str = include_str!("fixed-monitor/settings.json");
const BASE_TIME: i64 = 1_767_225_600;
const DEFAULT_LIMIT: i64 = 100;
// serde's errors quote the refused value, which may be a secret.
const CONFIG_TYPE_MISMATCH: &str =
    "the configuration does not fit the monitor's configuration type";

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Config {
    events: i64,
    log_demo: String,
    panic_demo: String,
}

impl Config {
    fn problems(&self) -> Vec<Problem> {
        let mut problems = Vec::new();
        if self.events < 0 {
            problems.push(Problem::new("events", "use a whole number of 0 or more"));
        }
        for (path, value) in [
            ("log_demo", &self.log_demo),
            ("panic_demo", &self.panic_demo),
        ] {
            if value != "0" && value != "1" {
                problems.push(Problem::new(path, "use 0 or 1"));
            }
        }
        problems
    }
}

// Reads the configuration document, which load takes once per process.
fn read_config() -> Result<Config, Vec<Problem>> {
    let document = config::load(SETTINGS).map_err(|err| err.problems())?;
    let config: Config = serde_json::from_value(Value::Object(document))
        .map_err(|_| vec![Problem::new("", CONFIG_TYPE_MISMATCH)])?;
    let problems = config.problems();
    if !problems.is_empty() {
        return Err(problems);
    }
    Ok(config)
}

struct Fixed {
    events: i64,
}

#[tonic::async_trait]
impl Monitor for Fixed {
    async fn describe(
        &self,
        _: Request<DescribeRequest>,
    ) -> Result<Response<DescribeResponse>, Status> {
        Ok(Response::new(DescribeResponse {
            name: "example-rust".into(),
            display_name: "Rust example".into(),
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
        let request = request.into_inner();
        let limit = if request.limit > 0 {
            i64::from(request.limit)
        } else {
            DEFAULT_LIMIT
        };
        let first = request.after.max(0).saturating_add(1);
        let last = self.events.min(first.saturating_add(limit) - 1);
        let events = (first..=last).map(event).collect();
        Ok(Response::new(EventsResponse { events }))
    }
}

fn event(seq: i64) -> Event {
    Event {
        seq,
        id: format!("e{seq}"),
        kind: "ping".into(),
        severity: Severity::Info.into(),
        occurred_at: Some(Timestamp {
            seconds: BASE_TIME + seq * 60,
            nanos: 0,
        }),
        title: format!("ping {seq}"),
        ..Event::default()
    }
}

fn log_demo(events: i64) -> Result<(), LoggingError> {
    logging::init("trace")?;
    tracing::trace!("log demo starting");
    tracing::debug!(events, "fixed events configured");
    tracing::info!(answer = 42, "hello from rust");
    tracing::warn!(ready = false, "warning from rust");
    tracing::error!(reason = "demo", "error from rust");
    Ok(())
}

fn panic_demo() -> Result<(), LoggingError> {
    logging::init("info")?;
    panic!("demo panic");
}

fn start_logging(config: &Config) -> Result<(), LoggingError> {
    if config.panic_demo == "1" {
        return panic_demo();
    }
    if config.log_demo == "1" {
        return log_demo(config.events);
    }
    logging::init("info")
}

// The demos depend on the configuration that failed, so the log starts as without them.
fn refuse(problems: &[Problem]) -> ExitCode {
    let _ = logging::init("info");
    let problems: Vec<String> = problems
        .iter()
        .map(|problem| match problem.path.as_str() {
            "" => problem.message.clone(),
            path => format!("{path}: {}", problem.message),
        })
        .collect();
    tracing::error!("invalid configuration: {}", problems.join("; "));
    ExitCode::FAILURE
}

fn validating() -> bool {
    env::args_os().skip(1).eq([VALIDATE_SETTINGS_ARG])
}

#[tokio::main]
async fn main() -> ExitCode {
    if validating() {
        return settings::answer(&read_config().err().unwrap_or_default());
    }
    let config = match read_config() {
        Ok(config) => config,
        Err(problems) => return refuse(&problems),
    };
    if let Err(err) = start_logging(&config) {
        tracing::error!("{err}");
        return ExitCode::FAILURE;
    }
    let events = config.events;
    let notifier = Notifier::new();
    notifier.events_recorded(events);
    match serve(Fixed { events }, notifier).await {
        Ok(()) => ExitCode::SUCCESS,
        Err(ServeError::NotLaunchedByHub) => ExitCode::FAILURE,
        Err(err) => {
            tracing::error!(error = &err as &dyn Error, "serve failed");
            ExitCode::FAILURE
        }
    }
}
