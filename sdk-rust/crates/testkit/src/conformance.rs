use std::collections::HashMap;
use std::env;
use std::ffi::OsString;
use std::fs;
use std::io::{self, ErrorKind, Read};
use std::os::unix::process::{CommandExt, ExitStatusExt};
use std::path::{Path, PathBuf};
use std::process::{self, Child, Command, ExitStatus, Stdio};
use std::sync::{Mutex, OnceLock, PoisonError};
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

enum Source {
    Binary(OsString),
    Go {
        package: String,
        version: String,
        env: Vec<(String, String)>,
    },
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
    if let Some(problem) = problem(conformance) {
        return Outcome::Failed(problem);
    }
    let Some(source) = locate() else {
        return unavailable(&format!(
            "set {COMMAND_ENV} to a built plugin-conformance binary or put go on PATH"
        ));
    };
    run_from(source, conformance)
}

fn problem(conformance: &Conformance) -> Option<String> {
    if conformance.command.is_empty() {
        return Some("Conformance.command is empty: there is no plugin to run".into());
    }
    let (name, _) = conformance
        .env
        .iter()
        .find(|(name, _)| name.is_empty() || name.contains('='))?;
    Some(format!(
        "Conformance.env has the invalid variable name {name:?}: a name must not be empty or contain '='"
    ))
}

fn run_from(source: Source, conformance: &Conformance) -> Outcome {
    let program = match obtain(source) {
        Ok(program) => program,
        Err(message) => return Outcome::Failed(message),
    };
    let limit = conformance.ready_timeout + ALLOWANCE;
    match execute(&program, &arguments(conformance), limit) {
        Ok(finished) => outcome(finished),
        Err(Failure::Start(err)) => {
            Outcome::Failed(format!("cannot start {}: {err}", program.to_string_lossy()))
        }
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

fn locate() -> Option<Source> {
    if let Some(path) = env::var_os(COMMAND_ENV).filter(|value| !value.is_empty()) {
        return Some(Source::Binary(path));
    }
    on_path("go").then(|| Source::Go {
        package: GO_PACKAGE.to_owned(),
        version: SDK_GO_VERSION
            .rsplit('/')
            .next()
            .unwrap_or(SDK_GO_VERSION)
            .to_owned(),
        env: Vec::new(),
    })
}

fn obtain(source: Source) -> Result<OsString, String> {
    match source {
        Source::Binary(path) => Ok(path),
        Source::Go {
            package,
            version,
            env,
        } => install(&package, &version, &env).map(PathBuf::into_os_string),
    }
}

fn install(package: &str, version: &str, extra: &[(String, String)]) -> Result<PathBuf, String> {
    static BUILT: OnceLock<Mutex<HashMap<String, PathBuf>>> = OnceLock::new();
    let key = format!("{package}@{version}");
    let mut built = BUILT
        .get_or_init(Mutex::default)
        .lock()
        .unwrap_or_else(PoisonError::into_inner);
    if let Some(binary) = built.get(&key) {
        return Ok(binary.clone());
    }
    let failed = |detail: &str| {
        format!("the conformance command {key} could not be fetched or built:\n{detail}")
    };
    let dir = env::temp_dir().join(format!("monitor-hub-conformance-{}", process::id()));
    match fs::create_dir(&dir) {
        Ok(()) => {}
        Err(err) if err.kind() == ErrorKind::AlreadyExists => {}
        Err(err) => return Err(failed(&format!("cannot create {}: {err}", dir.display()))),
    }
    let output = Command::new("go")
        .args(["install", &key])
        .env("GOBIN", &dir)
        .envs(extra.iter().map(|(name, value)| (name, value)))
        .stdin(Stdio::null())
        .output();
    let detail = match &output {
        Ok(output) if output.status.success() => None,
        Ok(output) => Some(String::from_utf8_lossy(&output.stderr).trim().to_owned()),
        Err(err) => Some(format!("cannot run go: {err}")),
    };
    if let Some(detail) = detail {
        let _ = fs::remove_dir(&dir);
        return Err(failed(&detail));
    }
    let binary = dir.join(package.rsplit('/').next().unwrap_or(package));
    built.insert(key, binary.clone());
    Ok(binary)
}

fn on_path(program: &str) -> bool {
    let Some(paths) = env::var_os("PATH") else {
        return false;
    };
    env::split_paths(&paths).any(|dir| Path::new(&dir).join(program).is_file())
}

fn arguments(conformance: &Conformance) -> Vec<String> {
    let mut args = Vec::new();
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

fn execute(program: &OsString, args: &[String], limit: Duration) -> Result<Finished, Failure> {
    let mut child = Command::new(program)
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
    let mut reaped = false;
    while Instant::now() < deadline {
        reaped = reaped || matches!(child.try_wait(), Ok(Some(_)));
        if reaped && !group_alive(child) {
            return;
        }
        thread::sleep(POLL);
    }
    signal_group(child, "KILL");
    let _ = child.kill();
    let _ = child.wait();
}

fn signal_group(child: &Child, signal: &str) -> bool {
    Command::new("kill")
        .args(["-s", signal, "--"])
        .arg(format!("-{}", child.id()))
        .stdout(Stdio::null())
        .stderr(Stdio::null())
        .status()
        .is_ok_and(|status| status.success())
}

fn group_alive(child: &Child) -> bool {
    signal_group(child, "0")
}

fn outcome(finished: Finished) -> Outcome {
    let Finished { status, stderr } = finished;
    if status.success() {
        return Outcome::Passed;
    }
    let output = stderr.trim();
    let code = status.code();
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

fn describe(status: ExitStatus) -> String {
    if let Some(signal) = status.signal() {
        return format!("killed by signal {signal}");
    }
    status.to_string()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_command_that_cannot_be_fetched_is_not_a_plugin_violation() {
        if !on_path("go") {
            assert!(
                env::var_os("CI").is_none_or(|value| value.is_empty()),
                "go is required when CI is set"
            );
            eprintln!("skipping: go is not available");
            return;
        }
        let source = Source::Go {
            package: GO_PACKAGE.to_owned(),
            version: "v9.9.9".to_owned(),
            env: vec![
                ("GOPROXY".to_owned(), "off".to_owned()),
                ("GOFLAGS".to_owned(), "-mod=mod".to_owned()),
            ],
        };
        let conformance = Conformance {
            command: vec!["unused".into()],
            env: Vec::new(),
            min_events: 0,
            ready_timeout: Duration::ZERO,
        };
        let Outcome::Failed(message) = run_from(source, &conformance) else {
            panic!("a version that cannot exist must fail");
        };
        assert!(message.starts_with("the conformance command "), "{message}");
        assert!(
            message.contains("could not be fetched or built:"),
            "{message}"
        );
    }
}
