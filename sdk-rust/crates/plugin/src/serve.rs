use std::future;
use std::io;
use std::time::Duration;

use thiserror::Error;
use tokio::signal::unix::{Signal, SignalKind, signal};
use tokio::sync::{oneshot, watch};
use tokio_stream::wrappers::UnixListenerStream;
use tonic::transport::Server;
use tonic::{Request, Response, Status};
use tonic_health::ServingStatus;

use crate::controller::Empty;
use crate::controller::grpc_controller_server::{GrpcController, GrpcControllerServer};
use crate::handshake::{NOT_LAUNCHED_BY_HUB, Socket, launched_by_hub};
use crate::proto::plugin_server::{Plugin, PluginServer};

const HEALTH_SERVICE: &str = "plugin";
const DRAIN_LIMIT: Duration = Duration::from_secs(1);

#[derive(Debug, Error)]
pub enum ServeError {
    #[error("{NOT_LAUNCHED_BY_HUB}")]
    NotLaunchedByHub,
    #[error("cannot register the signal handlers")]
    Signals(#[source] io::Error),
    #[error("cannot create the plugin socket")]
    Socket(#[source] io::Error),
    #[error("cannot write the handshake line")]
    Handshake(#[source] io::Error),
    #[error("the plugin gRPC server failed")]
    Transport(#[source] tonic::transport::Error),
}

pub async fn serve<P: Plugin>(plugin: P) -> Result<(), ServeError> {
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
    let router = Server::builder()
        .add_service(health_service)
        .add_service(GrpcControllerServer::new(Controller { stop }))
        .add_service(PluginServer::new(plugin));

    socket.announce().map_err(ServeError::Handshake)?;

    let (draining, drain_started) = oneshot::channel();
    let shutdown = async move {
        signals.or_stop(stop_requested).await;
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
