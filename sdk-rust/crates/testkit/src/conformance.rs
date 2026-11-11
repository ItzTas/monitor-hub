use std::env;
use std::ffi::OsString;
use std::io::{self, Read};
use std::os::unix::process::{CommandExt, ExitStatusExt};
use std::path::Path;
use std::process::{Child, Command, ExitStatus, Stdio};
use std::thread::{self, JoinHandle};
use std::time::{Duration, Instant};

pub const SDK_GO_VERSION: &str = "sdk/v0.2.0";

const GO_PACKAGE: &str = "github.com/ItzTas/monitor-hub/sdk/cmd/plugin-conformance";
const COMMAND_ENV: &str = "PLUGIN_CONFORMANCE";
const ALLOWANCE: Duration = Duration::from_secs(120);
const GRACE: Duration = Duration::from_secs(5);
const POLL: Duration = Duration::from_millis(20);
const EXIT_VIOLATIONS: i32 = 1;
const EXIT_USAGE: i32 = 2;

pub struct Conformance {
    pub command: Vec<String>,
    pub env: Vec<(String, String)>,
    pub min_events: usize,
    pub ready_timeout: Duration,
}

#[derive(Debug, PartialEq, Eq)]
pub enum Outcome {
    Passed,
    Skipped(String),
    Failed(String),
}

struct Invocation {
    program: OsString,
    prefix: Vec<String>,
    through_go: bool,
}

struct Finished {
    status: ExitStatus,
    stderr: String,
}

struct Running(Child);

impl Drop for Running {
    fn drop(&mut self) {
        if matches!(self.0.try_wait(), Ok(None)) {
            stop(&mut self.0);
        }
    }
}

pub fn run(conformance: &Conformance) -> Outcome {
    if conformance.command.is_empty() {
        return Outcome::Failed("Conformance.command is empty: there is no plugin to run".into());
    }
    let Some(invocation) = locate() else {
        return unavailable(&format!(
            "set {COMMAND_ENV} to a built plugin-conformance binary or put go on PATH"
        ));
    };
    let limit = conformance.ready_timeout + ALLOWANCE;
    match execute(&invocation, &arguments(&invocation, conformance), limit) {
        Ok(finished) => outcome(&invocation, finished),
        Err(Failure::Start(err)) => Outcome::Failed(format!(
            "cannot start {}: {err}",
            invocation.program.to_string_lossy()
        )),
        Err(Failure::Timeout(stderr)) => Outcome::Failed(format!(
            "plugin-conformance did not finish within {limit:?} and was stopped\n{stderr}"
        )),
    }
}

fn unavailable(reason: &str) -> Outcome {
    if env::var_os("CI").is_some_and(|value| !value.is_empty()) {
        panic!("the conformance command is required when CI is set: {reason}");
    }
    Outcome::Skipped(reason.to_owned())
}

fn locate() -> Option<Invocation> {
    if let Some(path) = env::var_os(COMMAND_ENV).filter(|value| !value.is_empty()) {
        return Some(Invocation {
            program: path,
            prefix: Vec::new(),
            through_go: false,
        });
    }
    on_path("go").then(|| {
        let version = SDK_GO_VERSION.rsplit('/').next().unwrap_or(SDK_GO_VERSION);
        Invocation {
            program: "go".into(),
            prefix: vec!["run".into(), format!("{GO_PACKAGE}@{version}")],
            through_go: true,
        }
    })
}

fn on_path(program: &str) -> bool {
    let Some(paths) = env::var_os("PATH") else {
        return false;
    };
    env::split_paths(&paths).any(|dir| Path::new(&dir).join(program).is_file())
}

fn arguments(invocation: &Invocation, conformance: &Conformance) -> Vec<String> {
    let mut args = invocation.prefix.clone();
    args.push("-min-events".into());
    args.push(conformance.min_events.to_string());
    args.push("-ready-timeout".into());
    args.push(format!("{}ms", conformance.ready_timeout.as_millis()));
    for (name, value) in &conformance.env {
        args.push("-env".into());
        args.push(format!("{name}={value}"));
    }
    args.push("--".into());
    args.extend(conformance.command.iter().cloned());
    args
}

enum Failure {
    Start(io::Error),
    Timeout(String),
}

fn execute(invocation: &Invocation, args: &[String], limit: Duration) -> Result<Finished, Failure> {
    let mut child = Command::new(&invocation.program)
        .args(args)
        .stdin(Stdio::null())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .process_group(0)
        .spawn()
        .map_err(Failure::Start)?;
    let stdout = drain(child.stdout.take());
    let stderr = drain(child.stderr.take());
    let mut running = Running(child);
    let status = wait(&mut running.0, limit);
    let _ = stdout.join();
    let stderr = String::from_utf8_lossy(&stderr.join().unwrap_or_default()).into_owned();
    match status {
        Some(status) => Ok(Finished { status, stderr }),
        None => Err(Failure::Timeout(stderr)),
    }
}

fn drain<R: Read + Send + 'static>(stream: Option<R>) -> JoinHandle<Vec<u8>> {
    thread::spawn(move || {
        let mut buf = Vec::new();
        if let Some(mut stream) = stream {
            let _ = stream.read_to_end(&mut buf);
        }
        buf
    })
}

fn wait(child: &mut Child, limit: Duration) -> Option<ExitStatus> {
    let deadline = Instant::now() + limit;
    loop {
        match child.try_wait() {
            Ok(Some(status)) => return Some(status),
            Ok(None) if Instant::now() < deadline => thread::sleep(POLL),
            _ => {
                stop(child);
                return None;
            }
        }
    }
}

fn stop(child: &mut Child) {
    signal_group(child, "TERM");
    let deadline = Instant::now() + GRACE;
    while Instant::now() < deadline {
        if matches!(child.try_wait(), Ok(Some(_))) {
            return;
        }
        thread::sleep(POLL);
    }
    signal_group(child, "KILL");
    let _ = child.kill();
    let _ = child.wait();
}

fn signal_group(child: &Child, signal: &str) {
    let _ = Command::new("kill")
        .args(["-s", signal, "--"])
        .arg(format!("-{}", child.id()))
        .stdout(Stdio::null())
        .stderr(Stdio::null())
        .status();
}

fn outcome(invocation: &Invocation, finished: Finished) -> Outcome {
    let Finished { status, stderr } = finished;
    if status.success() {
        return Outcome::Passed;
    }
    let output = stderr.trim();
    let code = reported_code(invocation, status, output);
    if code == Some(EXIT_USAGE) {
        return Outcome::Failed(format!(
            "plugin-conformance rejected its arguments (a bug in the conformance runner, not a plugin violation):\n{output}"
        ));
    }
    if code == Some(EXIT_VIOLATIONS) && !output.is_empty() {
        return Outcome::Failed(output.to_owned());
    }
    Outcome::Failed(format!(
        "plugin-conformance ended unexpectedly ({}):\n{output}",
        describe(status)
    ))
}

fn reported_code(invocation: &Invocation, status: ExitStatus, stderr: &str) -> Option<i32> {
    let code = status.code()?;
    if !invocation.through_go || code != EXIT_VIOLATIONS {
        return Some(code);
    }
    let last = stderr.lines().next_back()?;
    match last.strip_prefix("exit status ") {
        Some(inner) => inner.parse().ok(),
        None => Some(code),
    }
}

fn describe(status: ExitStatus) -> String {
    if let Some(signal) = status.signal() {
        return format!("killed by signal {signal}");
    }
    status.to_string()
}
