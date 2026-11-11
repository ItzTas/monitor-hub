use std::env;
use std::fs;
use std::io::{BufRead, BufReader, Read};
use std::ops::{Deref, DerefMut};
use std::os::unix::fs::{FileTypeExt, PermissionsExt};
use std::os::unix::net::UnixStream;
use std::path::{Path, PathBuf};
use std::process::{Child, Command, ExitStatus, Stdio};
use std::sync::{OnceLock, mpsc};
use std::thread;
use std::time::{Duration, Instant};

use monitor_hub_plugin::{MAGIC_COOKIE_KEY, MAGIC_COOKIE_VALUE, NOT_LAUNCHED_BY_HUB};
use tempfile::TempDir;
use tonic::codegen::http::uri::PathAndQuery;
use tonic::transport::{Channel, Endpoint};
use tonic_health::pb::HealthCheckRequest;
use tonic_health::pb::health_check_response::ServingStatus;
use tonic_health::pb::health_client::HealthClient;

const HANDSHAKE_TIMEOUT: Duration = Duration::from_secs(10);

struct Plugin(Child);

impl Deref for Plugin {
    type Target = Child;

    fn deref(&self) -> &Child {
        &self.0
    }
}

impl DerefMut for Plugin {
    fn deref_mut(&mut self) -> &mut Child {
        &mut self.0
    }
}

impl Drop for Plugin {
    fn drop(&mut self) {
        if matches!(self.0.try_wait(), Ok(None)) {
            let _ = self.0.kill();
            let _ = self.0.wait();
        }
    }
}

fn example() -> &'static Path {
    static PATH: OnceLock<PathBuf> = OnceLock::new();
    PATH.get_or_init(|| {
        let status = Command::new(env!("CARGO"))
            .args([
                "build",
                "--quiet",
                "-p",
                "monitor-hub-plugin",
                "--example",
                "fixed",
            ])
            .status()
            .expect("run cargo build");
        assert!(
            status.success(),
            "cargo build --example fixed failed: {status}"
        );
        let test_binary = env::current_exe().expect("test binary path");
        let profile = test_binary
            .parent()
            .and_then(Path::parent)
            .expect("target profile dir");
        profile.join("examples/fixed")
    })
}

fn command(socket_dir: &Path) -> Command {
    let mut cmd = Command::new(example());
    cmd.env_clear()
        .env("TMPDIR", socket_dir)
        .stdin(Stdio::null())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped());
    cmd
}

fn hub_command(socket_dir: &Path) -> Command {
    let mut cmd = command(socket_dir);
    cmd.env(MAGIC_COOKIE_KEY, MAGIC_COOKIE_VALUE);
    cmd
}

fn spawn(mut cmd: Command) -> Plugin {
    Plugin(cmd.spawn().expect("start example plugin"))
}

fn wait_exit(plugin: &mut Plugin, limit: Duration) -> ExitStatus {
    let deadline = Instant::now() + limit;
    loop {
        if let Some(status) = plugin.try_wait().expect("poll plugin") {
            return status;
        }
        assert!(
            Instant::now() < deadline,
            "plugin still running after {limit:?}"
        );
        thread::sleep(Duration::from_millis(20));
    }
}

fn run_to_exit(cmd: Command) -> (ExitStatus, String, String) {
    let mut plugin = spawn(cmd);
    let status = wait_exit(&mut plugin, HANDSHAKE_TIMEOUT);
    (
        status,
        read_all(plugin.stdout.take()),
        read_all(plugin.stderr.take()),
    )
}

fn read_all(pipe: Option<impl Read>) -> String {
    let mut text = String::new();
    pipe.expect("piped output")
        .read_to_string(&mut text)
        .expect("read output");
    text
}

fn handshake(plugin: &mut Plugin) -> PathBuf {
    let stdout = plugin.stdout.take().expect("piped stdout");
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
        .and_then(|line| line.strip_prefix("1|1|unix|"))
        .and_then(|line| line.strip_suffix("|grpc"))
        .filter(|path| !path.is_empty())
        .unwrap_or_else(|| panic!("unexpected handshake line {line:?}"));
    PathBuf::from(path)
}

async fn connect(socket: &Path) -> Channel {
    Endpoint::from_shared(format!("unix:{}", socket.display()))
        .expect("socket endpoint")
        .connect()
        .await
        .expect("connect to plugin")
}

fn entries(dir: &Path) -> Vec<PathBuf> {
    fs::read_dir(dir)
        .expect("read socket dir")
        .map(|entry| entry.expect("read dir entry").path())
        .collect()
}

#[test]
fn serve_without_cookie_explains_and_fails() {
    let dir = TempDir::new().expect("create temp dir");

    let (status, stdout, stderr) = run_to_exit(command(dir.path()));

    assert_eq!(status.code(), Some(1));
    assert_eq!(stderr, format!("{NOT_LAUNCHED_BY_HUB}\n"));
    assert_eq!(stdout, "");
    assert_eq!(entries(dir.path()), Vec::<PathBuf>::new());
}

#[test]
fn serve_with_unwritable_socket_dir_prints_nothing_on_stdout() {
    let dir = TempDir::new().expect("create temp dir");
    let missing = dir.path().join("missing");

    let (status, stdout, stderr) = run_to_exit(hub_command(&missing));

    assert!(
        matches!(status.code(), Some(code) if code != 0),
        "exit status {status}"
    );
    assert_eq!(stdout, "");
    assert!(
        stderr.contains(&missing.display().to_string()),
        "stderr does not name the socket dir: {stderr:?}"
    );
}

#[test]
fn serve_with_a_too_long_socket_path_prints_nothing_on_stdout() {
    let dir = TempDir::new().expect("create temp dir");
    let deep = dir.path().join("d".repeat(100));
    fs::create_dir(&deep).expect("create deep dir");

    let (status, stdout, stderr) = run_to_exit(hub_command(&deep));

    assert!(
        matches!(status.code(), Some(code) if code != 0),
        "exit status {status}"
    );
    assert_eq!(stdout, "");
    assert!(stderr.contains("too long"), "stderr: {stderr:?}");
    assert_eq!(entries(&deep), Vec::<PathBuf>::new());
}

#[test]
fn handshake_line_names_a_listening_socket() {
    let dir = TempDir::new().expect("create temp dir");
    let mut plugin = spawn(hub_command(dir.path()));

    let socket = handshake(&mut plugin);

    assert!(
        socket.is_absolute(),
        "socket path {socket:?} is not absolute"
    );
    assert_eq!(socket.parent(), Some(dir.path()));
    let kind = fs::metadata(&socket).expect("stat socket").file_type();
    assert!(kind.is_socket(), "{socket:?} is not a socket");
    UnixStream::connect(&socket).expect("connect to socket");

    let kill = Command::new("kill")
        .args(["-TERM", &plugin.id().to_string()])
        .status()
        .expect("send SIGTERM");
    assert!(kill.success());
    assert_eq!(
        wait_exit(&mut plugin, Duration::from_secs(5)).code(),
        Some(0)
    );
    assert!(!socket.exists(), "socket {socket:?} left behind");
}

#[test]
fn the_socket_is_readable_and_writable_by_its_owner_only() {
    let dir = TempDir::new().expect("create temp dir");
    let mut cmd = Command::new("/bin/sh");
    cmd.args(["-c", "umask 0 && exec \"$0\""])
        .arg(example())
        .env_clear()
        .env("TMPDIR", dir.path())
        .env(MAGIC_COOKIE_KEY, MAGIC_COOKIE_VALUE)
        .stdin(Stdio::null())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped());
    let mut plugin = spawn(cmd);

    let socket = handshake(&mut plugin);

    let mode = fs::metadata(&socket)
        .expect("stat socket")
        .permissions()
        .mode();
    assert_eq!(mode & 0o777, 0o600, "mode {mode:o}");
}

#[tokio::test]
async fn health_reports_the_plugin_service_as_serving() {
    let dir = TempDir::new().expect("create temp dir");
    let mut plugin = spawn(hub_command(dir.path()));
    let socket = handshake(&mut plugin);

    let reply = HealthClient::new(connect(&socket).await)
        .check(HealthCheckRequest {
            service: "plugin".into(),
        })
        .await
        .expect("health check");

    assert_eq!(reply.get_ref().status(), ServingStatus::Serving);
}

#[tokio::test]
async fn shutdown_rpc_stops_the_plugin() {
    let dir = TempDir::new().expect("create temp dir");
    let mut plugin = spawn(hub_command(dir.path()));
    let socket = handshake(&mut plugin);
    let mut grpc = tonic::client::Grpc::new(connect(&socket).await);
    grpc.ready().await.expect("channel ready");

    grpc.unary(
        tonic::Request::new(()),
        PathAndQuery::from_static("/plugin.GRPCController/Shutdown"),
        tonic_prost::ProstCodec::<(), ()>::default(),
    )
    .await
    .expect("call Shutdown");

    assert_eq!(
        wait_exit(&mut plugin, Duration::from_secs(2)).code(),
        Some(0)
    );
    assert!(!socket.exists(), "socket {socket:?} left behind");
}
