use std::env;
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

fn spawn(switch: &str, socket_dir: &Path) -> Child {
    Command::new(example())
        .env_clear()
        .env("TMPDIR", socket_dir)
        .env(MAGIC_COOKIE_KEY, MAGIC_COOKIE_VALUE)
        .env(switch, "1")
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

fn run_demo() -> Run {
    let dir = TempDir::new().expect("create temp dir");
    let mut child = spawn("LOG_DEMO", dir.path());
    let (first_line, reader) = read_stdout(&mut child);
    let handshake = first_line.recv_timeout(HANDSHAKE_TIMEOUT);
    let kill = Command::new("kill")
        .args(["-TERM", &child.id().to_string()])
        .status()
        .expect("send SIGTERM");
    assert!(kill.success());
    handshake.expect("handshake line in time");
    finish(child, reader)
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

    let handshake = run
        .stdout
        .strip_prefix("1|1|unix|")
        .and_then(|rest| rest.strip_suffix("|grpc\n"))
        .unwrap_or_else(|| panic!("stdout is not exactly the handshake line: {:?}", run.stdout));
    assert!(
        !handshake.contains('\n'),
        "stdout has more than the handshake line: {:?}",
        run.stdout
    );
}

#[test]
fn a_panic_is_one_error_line_on_stderr() {
    let dir = TempDir::new().expect("create temp dir");
    let mut child = spawn("PANIC_DEMO", dir.path());
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
