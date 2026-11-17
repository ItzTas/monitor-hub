use std::io::{self, Read};
use std::os::unix::process::CommandExt;
use std::process::{Child, Command, ExitStatus, Stdio};
use std::thread::{self, JoinHandle};
use std::time::{Duration, Instant};

use monitor_hub_plugin::settings::ANSWER_LIMIT;

pub(crate) const GRACE: Duration = Duration::from_secs(5);
const POLL: Duration = Duration::from_millis(20);
const STDOUT_LIMIT: u64 = ANSWER_LIMIT as u64 + 1;

pub(crate) struct Finished {
    pub(crate) status: ExitStatus,
    pub(crate) stdout: Vec<u8>,
    pub(crate) stderr: String,
}

pub(crate) enum Failure {
    Start(io::Error),
    Timeout(String),
}

struct Running(Child);

impl Drop for Running {
    fn drop(&mut self) {
        if matches!(self.0.try_wait(), Ok(None)) {
            stop(&mut self.0);
        }
    }
}

pub(crate) fn execute(command: &mut Command, limit: Duration) -> Result<Finished, Failure> {
    let mut child = command
        .stdin(Stdio::null())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .process_group(0)
        .spawn()
        .map_err(Failure::Start)?;
    let stdout = drain(child.stdout.take(), STDOUT_LIMIT);
    let stderr = drain(child.stderr.take(), u64::MAX);
    let mut running = Running(child);
    let status = wait(&mut running.0, limit);
    signal_group(&running.0, "KILL");
    let stdout = stdout.join().unwrap_or_default();
    let stderr = String::from_utf8_lossy(&stderr.join().unwrap_or_default()).into_owned();
    match status {
        Some(status) => Ok(Finished {
            status,
            stdout,
            stderr,
        }),
        None => Err(Failure::Timeout(stderr)),
    }
}

fn drain<R: Read + Send + 'static>(stream: Option<R>, keep: u64) -> JoinHandle<Vec<u8>> {
    thread::spawn(move || {
        let mut buf = Vec::new();
        let Some(mut stream) = stream else {
            return buf;
        };
        let _ = stream.by_ref().take(keep).read_to_end(&mut buf);
        let _ = io::copy(&mut stream, &mut io::sink());
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

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn stdout_is_kept_up_to_its_limit_and_the_rest_is_drained() {
        let mut command = Command::new("sh");
        command.args(["-c", "head -c 200000 /dev/zero; echo done"]);

        let Ok(finished) = execute(&mut command, Duration::from_secs(10)) else {
            panic!("sh did not run to its end");
        };

        assert!(finished.status.success(), "{}", finished.status);
        assert_eq!(finished.stdout.len(), ANSWER_LIMIT + 1);
    }
}
