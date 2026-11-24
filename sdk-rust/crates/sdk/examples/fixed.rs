use std::env;
use std::error::Error;
use std::process::ExitCode;

use monitor_hub_sdk::logging::{self, LoggingError};
use monitor_hub_sdk::proto::{
    CheckRequest, CheckResponse, DescribeRequest, DescribeResponse, Event, EventKind,
    EventsRequest, EventsResponse, Severity, State, StatusRequest, StatusResponse,
};
use monitor_hub_sdk::settings::{self, Problem, VALIDATE_SETTINGS_ARG};
use monitor_hub_sdk::{Monitor, Notifier, ServeError, serve};
use prost_types::Timestamp;
use tonic::{Request, Response, Status};

const BASE_TIME: i64 = 1_767_225_600;
const DEFAULT_LIMIT: i64 = 100;

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

fn switched_on(name: &str) -> bool {
    env::var(name).is_ok_and(|value| value == "1")
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

fn start_logging(events: i64) -> Result<(), LoggingError> {
    if switched_on("PANIC_DEMO") {
        return panic_demo();
    }
    if switched_on("LOG_DEMO") {
        return log_demo(events);
    }
    logging::init("info")
}

fn validating() -> bool {
    env::args_os().skip(1).eq([VALIDATE_SETTINGS_ARG])
}

fn problems() -> Vec<Problem> {
    let events = env::var_os("EVENTS").is_none_or(|raw| {
        raw.to_str()
            .is_some_and(|raw| raw.is_empty() || raw.parse::<i64>().is_ok_and(|n| n >= 0))
    });
    let mut problems = Vec::new();
    if !events {
        problems.push(Problem::new("events", "use a whole number of 0 or more"));
    }
    for (variable, path) in [("LOG_DEMO", "log_demo"), ("PANIC_DEMO", "panic_demo")] {
        let switch =
            env::var_os(variable).is_none_or(|raw| ["", "0", "1"].iter().any(|ok| raw == *ok));
        if !switch {
            problems.push(Problem::new(path, "use 0 or 1"));
        }
    }
    problems
}

#[tokio::main]
async fn main() -> ExitCode {
    if validating() {
        return settings::answer(&problems());
    }
    let events = env::var("EVENTS")
        .ok()
        .and_then(|raw| raw.trim().parse().ok())
        .unwrap_or(0);
    if let Err(err) = start_logging(events) {
        tracing::error!("{err}");
        return ExitCode::FAILURE;
    }
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
