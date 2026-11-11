use std::env;
use std::fs::{self, File};
use std::io::{BufRead, BufReader, Read};
use std::path::{Path, PathBuf};
use std::process::{Child, Command, Output, Stdio};
use std::sync::{OnceLock, mpsc};
use std::thread;
use std::time::Duration;

use chrono::DateTime;
use monitor_hub_plugin::logging::{LoggingError, init};
use monitor_hub_plugin::{MAGIC_COOKIE_KEY, MAGIC_COOKIE_VALUE};
use serde_json::{Map, Value};
use tempfile::TempDir;

const HANDSHAKE_TIMEOUT: Duration = Duration::from_secs(10);

struct Run {
    stdout: String,
    stderr: String,
    output: Output,
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

fn spawn(switches: &[&str], socket_dir: &Path) -> Child {
    Command::new(example())
        .env_clear()
        .env("TMPDIR", socket_dir)
        .env(MAGIC_COOKIE_KEY, MAGIC_COOKIE_VALUE)
        .envs(switches.iter().map(|switch| (switch, "1")))
        .stdin(Stdio::null())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .spawn()
        .expect("start example plugin")
}

fn read_stdout(child: &mut Child) -> (mpsc::Receiver<String>, thread::JoinHandle<String>) {
    let mut stdout = BufReader::new(child.stdout.take().expect("piped stdout"));
    let (first_line, rx) = mpsc::channel();
    let reader = thread::spawn(move || {
        let mut text = String::new();
        stdout.read_line(&mut text).expect("read stdout");
        let _ = first_line.send(text.clone());
        stdout.read_to_string(&mut text).expect("read stdout");
        text
    });
    (rx, reader)
}

fn serve_then_stop(mut child: Child, at_handshake: impl FnOnce(&Path)) -> Run {
    let (first_line, reader) = read_stdout(&mut child);
    let handshake = first_line.recv_timeout(HANDSHAKE_TIMEOUT);
    if let Ok(line) = &handshake {
        at_handshake(&socket_path(line));
    }
    let kill = Command::new("kill")
        .args(["-TERM", &child.id().to_string()])
        .status()
        .expect("send SIGTERM");
    assert!(kill.success());
    handshake.expect("handshake line in time");
    finish(child, reader)
}

fn socket_path(stdout: &str) -> PathBuf {
    stdout
        .strip_prefix("1|1|unix|")
        .and_then(|rest| rest.strip_suffix("|grpc\n"))
        .filter(|path| !path.contains('\n'))
        .map(PathBuf::from)
        .unwrap_or_else(|| panic!("stdout is not exactly the handshake line: {stdout:?}"))
}

fn run_demo() -> Run {
    let dir = TempDir::new().expect("create temp dir");
    serve_then_stop(spawn(&["LOG_DEMO"], dir.path()), |_| {})
}

fn finish(child: Child, reader: thread::JoinHandle<String>) -> Run {
    let output = child.wait_with_output().expect("wait for the example");
    Run {
        stdout: reader.join().expect("stdout reader"),
        stderr: String::from_utf8(output.stderr.clone()).expect("stderr is UTF-8"),
        output,
    }
}

fn json_lines(stderr: &str) -> Vec<Map<String, Value>> {
    assert!(
        stderr.ends_with('\n'),
        "stderr does not end a line: {stderr:?}"
    );
    stderr
        .lines()
        .map(|line| match serde_json::from_str(line) {
            Ok(Value::Object(object)) => object,
            _ => panic!("stderr line is not a JSON object: {line:?}"),
        })
        .collect()
}

fn text<'a>(object: &'a Map<String, Value>, key: &str) -> &'a str {
    object
        .get(key)
        .and_then(Value::as_str)
        .unwrap_or_else(|| panic!("{key} is not a string in {object:?}"))
}

#[test]
fn each_event_is_one_json_line_on_stderr() {
    let run = run_demo();

    let lines = json_lines(&run.stderr);
    for line in &lines {
        text(line, "@level");
        text(line, "@message");
        let timestamp = text(line, "@timestamp");
        assert!(
            timestamp.ends_with('Z'),
            "timestamp not in UTC: {timestamp}"
        );
        DateTime::parse_from_rfc3339(timestamp).expect("RFC 3339 timestamp");
    }
    let demo: Vec<&Map<String, Value>> = lines
        .iter()
        .filter(|line| text(line, "@module") == "fixed")
        .collect();
    let levels: Vec<&str> = demo.iter().map(|line| text(line, "@level")).collect();
    assert_eq!(levels, ["trace", "debug", "info", "warn", "error"]);
    let info = demo[2];
    assert_eq!(text(info, "@message"), "hello from rust");
    assert_eq!(info.get("answer"), Some(&Value::from(42)));
}

#[test]
fn logging_never_writes_to_stdout() {
    let run = run_demo();

    socket_path(&run.stdout);
}

#[test]
fn a_panic_is_one_error_line_on_stderr() {
    let dir = TempDir::new().expect("create temp dir");
    let mut child = spawn(&["PANIC_DEMO"], dir.path());
    let (_, reader) = read_stdout(&mut child);

    let run = finish(child, reader);

    assert_eq!(run.output.status.code(), Some(101));
    assert_eq!(run.stdout, "");
    let lines = json_lines(&run.stderr);
    assert_eq!(lines.len(), 1, "stderr: {:?}", run.stderr);
    let line = &lines[0];
    assert_eq!(text(line, "@level"), "error");
    assert_eq!(text(line, "panic"), "demo panic");
    let location = text(line, "location");
    assert!(
        location.starts_with("crates/plugin/examples/fixed.rs:"),
        "location: {location}"
    );
}

#[test]
fn unknown_level_is_an_error() {
    assert_eq!(init("loud"), Err(LoggingError::UnknownLevel("loud".into())));
}

#[test]
fn a_second_init_is_an_error() {
    assert_eq!(init("Info"), Ok(()));

    assert_eq!(init("debug"), Err(LoggingError::AlreadyInitialized));
}

#[test]
fn a_socket_cleanup_failure_is_a_json_line() {
    let dir = TempDir::new().expect("create temp dir");
    let mut socket = PathBuf::new();

    let run = serve_then_stop(spawn(&[], dir.path()), |path| {
        fs::remove_file(path).expect("remove the socket");
        fs::create_dir(path).expect("put a directory where the socket was");
        socket = path.to_owned();
    });

    assert_eq!(run.output.status.code(), Some(0));
    assert_eq!(socket_path(&run.stdout), socket);
    let lines = json_lines(&run.stderr);
    assert_eq!(lines.len(), 1, "stderr: {:?}", run.stderr);
    let line = &lines[0];
    assert_eq!(text(line, "@level"), "warn");
    assert_eq!(text(line, "@message"), "cannot remove the plugin socket");
    assert_eq!(text(line, "socket"), socket.to_str().expect("UTF-8 path"));
    text(line, "error");
}

#[test]
fn a_serve_failure_is_a_json_error_line() {
    let dir = TempDir::new().expect("create temp dir");
    let missing = dir.path().join("missing");
    let mut child = spawn(&[], &missing);
    let (_, reader) = read_stdout(&mut child);

    let run = finish(child, reader);

    assert_eq!(run.output.status.code(), Some(1));
    assert_eq!(run.stdout, "");
    let error = serve_error(&run.stderr);
    let want = format!(
        "cannot create the plugin socket: listen on {}/plugin-",
        missing.display()
    );
    assert!(error.starts_with(&want), "error: {error}");
    assert!(error.ends_with("(os error 2)"), "error: {error}");
}

#[test]
fn a_handshake_write_failure_is_a_json_error_line_and_leaves_no_socket() {
    let dir = TempDir::new().expect("create temp dir");
    let full = File::create("/dev/full").expect("open /dev/full");
    let child = Command::new(example())
        .env_clear()
        .env("TMPDIR", dir.path())
        .env(MAGIC_COOKIE_KEY, MAGIC_COOKIE_VALUE)
        .stdin(Stdio::null())
        .stdout(full)
        .stderr(Stdio::piped())
        .spawn()
        .expect("start example plugin");

    let run = finish(child, thread::spawn(String::new));

    assert_eq!(run.output.status.code(), Some(1));
    assert_eq!(
        serve_error(&run.stderr),
        "cannot write the handshake line: No space left on device (os error 28)"
    );
    assert_eq!(fs::read_dir(dir.path()).expect("read dir").count(), 0);
}

fn serve_error(stderr: &str) -> String {
    let lines = json_lines(stderr);
    assert_eq!(lines.len(), 1, "stderr: {stderr:?}");
    assert_eq!(text(&lines[0], "@level"), "error");
    assert_eq!(text(&lines[0], "@message"), "serve failed");
    text(&lines[0], "error").to_owned()
}
