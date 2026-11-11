use std::env;
use std::process::ExitCode;

use monitor_hub_plugin::logging::{self, LoggingError};
use monitor_hub_plugin::proto::plugin_server::Plugin;
use monitor_hub_plugin::proto::{
    CheckRequest, CheckResponse, DescribeRequest, DescribeResponse, Event, EventKind,
    EventsRequest, EventsResponse, Severity, State, StatusRequest, StatusResponse,
};
use monitor_hub_plugin::{ServeError, serve};
use prost_types::Timestamp;
use tonic::{Request, Response, Status};

const BASE_TIME: i64 = 1_767_225_600;
const DEFAULT_LIMIT: i64 = 100;

struct Fixed {
    events: i64,
}

#[tonic::async_trait]
impl Plugin for Fixed {
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

fn demo(events: i64) -> Result<(), LoggingError> {
    if switched_on("PANIC_DEMO") {
        return panic_demo();
    }
    if switched_on("LOG_DEMO") {
        return log_demo(events);
    }
    Ok(())
}

#[tokio::main]
async fn main() -> ExitCode {
    let events = env::var("EVENTS")
        .ok()
        .and_then(|raw| raw.trim().parse().ok())
        .unwrap_or(0);
    if let Err(err) = demo(events) {
        eprintln!("{err}");
        return ExitCode::FAILURE;
    }
    match serve(Fixed { events }).await {
        Ok(()) => ExitCode::SUCCESS,
        Err(ServeError::NotLaunchedByHub) => ExitCode::FAILURE,
        Err(err) => {
            eprintln!("{err}");
            ExitCode::FAILURE
        }
    }
}
