use std::io::{BufRead, BufReader};
use std::ops::{Deref, DerefMut};
use std::path::{Path, PathBuf};
use std::process::{Child, Command, ExitStatus, Stdio};
use std::sync::mpsc;
use std::thread;
use std::time::{Duration, Instant};

use monitor_hub_sdk::proto::{WatchRequest, WatchResponse};
use monitor_hub_sdk::{MAGIC_COOKIE_KEY, MAGIC_COOKIE_VALUE};
use tempfile::TempDir;
use tonic::Request;
use tonic::codec::Streaming;
use tonic::codegen::http::uri::PathAndQuery;
use tonic::transport::{Channel, Endpoint};

#[path = "support/example.rs"]
mod example;

const HANDSHAKE_TIMEOUT: Duration = Duration::from_secs(10);

struct Example(Child);

impl Deref for Example {
    type Target = Child;

    fn deref(&self) -> &Child {
        &self.0
    }
}

impl DerefMut for Example {
    fn deref_mut(&mut self) -> &mut Child {
        &mut self.0
    }
}

impl Drop for Example {
    fn drop(&mut self) {
        if matches!(self.0.try_wait(), Ok(None)) {
            let _ = self.0.kill();
            let _ = self.0.wait();
        }
    }
}

fn start(socket_dir: &Path, events: &str) -> Example {
    let child = Command::new(example::fixed())
        .env_clear()
        .env("TMPDIR", socket_dir)
        .env(MAGIC_COOKIE_KEY, MAGIC_COOKIE_VALUE)
        .env("EVENTS", events)
        .stdin(Stdio::null())
        .stdout(Stdio::piped())
        .stderr(Stdio::null())
        .spawn()
        .expect("start example monitor");
    Example(child)
}

fn handshake(monitor: &mut Example) -> PathBuf {
    let stdout = monitor.stdout.take().expect("piped stdout");
    let (tx, rx) = mpsc::channel();
    thread::spawn(move || {
        let mut line = String::new();
        let read = BufReader::new(stdout).read_line(&mut line).map(|_| line);
        let _ = tx.send(read);
    });
    let line = rx
        .recv_timeout(HANDSHAKE_TIMEOUT)
        .expect("handshake line in time")
        .expect("read stdout");
    let path = line
        .strip_suffix('\n')
        .and_then(|line| line.strip_prefix("1|2|unix|"))
        .and_then(|line| line.strip_suffix("|grpc"))
        .filter(|path| !path.is_empty())
        .unwrap_or_else(|| panic!("unexpected handshake line {line:?}"));
    PathBuf::from(path)
}

fn wait_exit(monitor: &mut Example, limit: Duration) -> ExitStatus {
    let deadline = Instant::now() + limit;
    loop {
        if let Some(status) = monitor.try_wait().expect("poll monitor") {
            return status;
        }
        assert!(
            Instant::now() < deadline,
            "monitor still running after {limit:?}"
        );
        thread::sleep(Duration::from_millis(20));
    }
}

async fn connect(socket: &Path) -> tonic::client::Grpc<Channel> {
    let channel = Endpoint::from_shared(format!("unix:{}", socket.display()))
        .expect("socket endpoint")
        .connect()
        .await
        .expect("connect to monitor");
    let mut grpc = tonic::client::Grpc::new(channel);
    grpc.ready().await.expect("channel ready");
    grpc
}

async fn watch(grpc: &mut tonic::client::Grpc<Channel>) -> Streaming<WatchResponse> {
    grpc.server_streaming(
        Request::new(WatchRequest {}),
        PathAndQuery::from_static("/monitorhub.monitor.v1.Monitor/Watch"),
        tonic_prost::ProstCodec::<WatchRequest, WatchResponse>::default(),
    )
    .await
    .expect("open Watch")
    .into_inner()
}

async fn first_signal(stream: &mut Streaming<WatchResponse>) -> WatchResponse {
    tokio::time::timeout(Duration::from_secs(5), stream.message())
        .await
        .expect("first signal within 5 seconds")
        .expect("read first signal")
        .expect("the stream sends a first signal")
}

#[tokio::test]
async fn the_example_signals_at_once_and_keeps_watch_open() {
    let dir = TempDir::new().expect("create temp dir");
    let mut monitor = start(dir.path(), "3");
    let socket = handshake(&mut monitor);
    let mut grpc = connect(&socket).await;

    let mut stream = watch(&mut grpc).await;

    assert_eq!(first_signal(&mut stream).await.latest_seq, 3);
    let next = tokio::time::timeout(Duration::from_millis(500), stream.message()).await;
    assert!(next.is_err(), "Watch did not stay open: {next:?}");
}

#[tokio::test]
async fn shutdown_ends_open_watch_streams() {
    let dir = TempDir::new().expect("create temp dir");
    let mut monitor = start(dir.path(), "3");
    let socket = handshake(&mut monitor);
    let mut grpc = connect(&socket).await;
    let mut stream = watch(&mut grpc).await;
    first_signal(&mut stream).await;

    grpc.ready().await.expect("channel ready");
    let started = Instant::now();
    grpc.unary(
        Request::new(()),
        PathAndQuery::from_static("/plugin.GRPCController/Shutdown"),
        tonic_prost::ProstCodec::<(), ()>::default(),
    )
    .await
    .expect("call Shutdown");
    let end = tokio::time::timeout(Duration::from_millis(800), stream.message())
        .await
        .expect("Watch ends within 800 ms of Shutdown");

    assert!(
        matches!(end, Ok(None) | Err(_)),
        "Watch sent a signal instead of ending: {end:?}"
    );
    assert!(
        started.elapsed() < Duration::from_millis(800),
        "Watch ended {:?} after Shutdown",
        started.elapsed()
    );
    assert_eq!(
        wait_exit(&mut monitor, Duration::from_secs(2)).code(),
        Some(0)
    );
}
