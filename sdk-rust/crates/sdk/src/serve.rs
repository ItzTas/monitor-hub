use std::future;
use std::io;
use std::time::Duration;

use thiserror::Error;
use tokio::signal::unix::{Signal, SignalKind, signal};
use tokio::sync::{oneshot, watch};
use tokio_stream::wrappers::UnixListenerStream;
use tonic::codegen::BoxStream;
use tonic::transport::Server;
use tonic::{Request, Response, Status};
use tonic_health::ServingStatus;

use crate::controller::Empty;
use crate::controller::grpc_controller_server::{GrpcController, GrpcControllerServer};
use crate::handshake::{NOT_LAUNCHED_BY_HUB, Socket, launched_by_hub};
use crate::monitor::Monitor;
use crate::proto::monitor_server::{Monitor as MonitorService, MonitorServer};
use crate::proto::{
    CheckRequest, CheckResponse, DescribeRequest, DescribeResponse, EventsRequest, EventsResponse,
    StatusRequest, StatusResponse, WatchRequest, WatchResponse,
};
use crate::watch::{Notifier, stream};

const HEALTH_SERVICE: &str = "plugin";
const DRAIN_LIMIT: Duration = Duration::from_secs(1);

#[derive(Debug, Error)]
#[non_exhaustive]
pub enum ServeError {
    #[error("{NOT_LAUNCHED_BY_HUB}")]
    NotLaunchedByHub,
    #[error("cannot register the signal handlers")]
    Signals(#[source] io::Error),
    #[error("cannot create the monitor socket")]
    Socket(#[source] io::Error),
    #[error("cannot write the handshake line")]
    Handshake(#[source] io::Error),
    #[error("the monitor gRPC server failed")]
    Transport(#[source] tonic::transport::Error),
}

/// Call once from `main`: serves `monitor` until the hub stops it; `Watch` reads `notifier`.
pub async fn serve<M: Monitor>(monitor: M, notifier: Notifier) -> Result<(), ServeError> {
    if !launched_by_hub() {
        eprintln!("{NOT_LAUNCHED_BY_HUB}");
        return Err(ServeError::NotLaunchedByHub);
    }
    let signals = Signals::listen().map_err(ServeError::Signals)?;
    let (socket, listener) = Socket::bind().map_err(ServeError::Socket)?;

    let (health, health_service) = tonic_health::server::health_reporter();
    health
        .set_service_status(HEALTH_SERVICE, ServingStatus::Serving)
        .await;
    let (stop, stop_requested) = watch::channel(false);
    let (closing, closed) = watch::channel(false);
    let router = Server::builder()
        .add_service(health_service)
        .add_service(GrpcControllerServer::new(Controller { stop }))
        .add_service(MonitorServer::new(Watched {
            monitor,
            notifier,
            closing: closed,
        }));

    socket.announce().map_err(ServeError::Handshake)?;

    let (draining, drain_started) = oneshot::channel();
    let shutdown = async move {
        signals.or_stop(stop_requested).await;
        closing.send_replace(true);
        let _ = draining.send(());
    };
    let server = router.serve_with_incoming_shutdown(UnixListenerStream::new(listener), shutdown);
    tokio::select! {
        result = server => result.map_err(ServeError::Transport),
        () = drain_deadline(drain_started) => Ok(()),
    }
}

async fn drain_deadline(drain_started: oneshot::Receiver<()>) {
    if drain_started.await.is_err() {
        return future::pending().await;
    }
    tokio::time::sleep(DRAIN_LIMIT).await;
}

struct Signals {
    terminate: Signal,
    interrupt: Signal,
}

impl Signals {
    fn listen() -> io::Result<Self> {
        Ok(Self {
            terminate: signal(SignalKind::terminate())?,
            interrupt: signal(SignalKind::interrupt())?,
        })
    }

    async fn or_stop(mut self, mut stop_requested: watch::Receiver<bool>) {
        tokio::select! {
            _ = self.terminate.recv() => {}
            _ = self.interrupt.recv() => {}
            _ = stop_requested.wait_for(|stop| *stop) => {}
        }
    }
}

struct Controller {
    stop: watch::Sender<bool>,
}

#[tonic::async_trait]
impl GrpcController for Controller {
    async fn shutdown(&self, _: Request<Empty>) -> Result<Response<Empty>, Status> {
        self.stop.send_replace(true);
        Ok(Response::new(Empty {}))
    }
}

struct Watched<M> {
    monitor: M,
    notifier: Notifier,
    closing: watch::Receiver<bool>,
}

#[tonic::async_trait]
impl<M: Monitor> MonitorService for Watched<M> {
    type WatchStream = BoxStream<WatchResponse>;

    async fn describe(
        &self,
        request: Request<DescribeRequest>,
    ) -> Result<Response<DescribeResponse>, Status> {
        self.monitor.describe(request).await
    }

    async fn status(
        &self,
        request: Request<StatusRequest>,
    ) -> Result<Response<StatusResponse>, Status> {
        self.monitor.status(request).await
    }

    async fn check(
        &self,
        request: Request<CheckRequest>,
    ) -> Result<Response<CheckResponse>, Status> {
        self.monitor.check(request).await
    }

    async fn events(
        &self,
        request: Request<EventsRequest>,
    ) -> Result<Response<EventsResponse>, Status> {
        self.monitor.events(request).await
    }

    async fn watch(&self, _: Request<WatchRequest>) -> Result<Response<Self::WatchStream>, Status> {
        Ok(Response::new(stream(&self.notifier, self.closing.clone())))
    }
}
