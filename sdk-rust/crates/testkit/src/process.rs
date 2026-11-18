use std::collections::VecDeque;
use std::io::{self, ErrorKind, Read};
use std::mem;
use std::os::unix::process::CommandExt;
use std::process::{Child, Command, ExitStatus, Stdio};
use std::sync::mpsc::{self, Receiver};
use std::sync::{Arc, Mutex, PoisonError, Weak};
use std::thread;
use std::time::{Duration, Instant};

use monitor_hub_plugin::settings::ANSWER_LIMIT;

pub(crate) const GRACE: Duration = Duration::from_secs(5);
const POLL: Duration = Duration::from_millis(20);
const OUTPUT_GRACE: Duration = Duration::from_secs(1);
const CHUNK: usize = 8192;
const STDOUT_LIMIT: usize = ANSWER_LIMIT + 1;
const DROPPED: &[u8] = b"(earlier output dropped)\n";

pub(crate) const STDERR_TAIL: usize = 1 << 20;

#[derive(Clone, Copy)]
pub(crate) enum Keep {
    Nothing,
    Head(usize),
    Tail(usize),
}

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

pub(crate) fn execute(
    command: &mut Command,
    limit: Duration,
    stderr: Keep,
) -> Result<Finished, Failure> {
    let mut child = command
        .stdin(Stdio::null())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .process_group(0)
        .spawn()
        .map_err(Failure::Start)?;
    let stdout = drain(child.stdout.take(), Keep::Head(STDOUT_LIMIT));
    let stderr = drain(child.stderr.take(), stderr);
    let mut running = Running(child);
    let status = wait(&mut running.0, limit);
    signal_group(&running.0, "KILL");
    let deadline = Instant::now() + OUTPUT_GRACE;
    let stdout = collect(&stdout, deadline);
    let stderr = String::from_utf8_lossy(&collect(&stderr, deadline)).into_owned();
    match status {
        Some(status) => Ok(Finished {
            status,
            stdout,
            stderr,
        }),
        None => Err(Failure::Timeout(stderr)),
    }
}

struct Output {
    kept: Arc<Mutex<Kept>>,
    done: Receiver<()>,
}

#[derive(Default)]
struct Kept {
    bytes: VecDeque<u8>,
    last_dropped: Option<u8>,
}

impl Kept {
    fn push(&mut self, keep: Keep, chunk: &[u8]) {
        match keep {
            Keep::Nothing => {}
            Keep::Head(limit) => {
                let room = limit.saturating_sub(self.bytes.len()).min(chunk.len());
                self.bytes.extend(&chunk[..room]);
            }
            Keep::Tail(limit) => self.push_tail(limit, chunk),
        }
    }

    fn push_tail(&mut self, limit: usize, chunk: &[u8]) {
        let start = chunk.len().saturating_sub(limit);
        let (dropped, chunk) = chunk.split_at(start);
        let excess = (self.bytes.len() + chunk.len()).saturating_sub(limit);
        if excess > 0 {
            self.last_dropped = self.bytes.get(excess - 1).copied();
            self.bytes.drain(..excess);
        }
        if let Some(&last) = dropped.last() {
            self.last_dropped = Some(last);
        }
        self.bytes.extend(chunk);
    }

    fn finish(self) -> Vec<u8> {
        let bytes = Vec::from(self.bytes);
        let Some(last) = self.last_dropped else {
            return bytes;
        };
        let start = match bytes.iter().position(|&b| b == b'\n') {
            Some(newline) if last != b'\n' => newline + 1,
            _ => 0,
        };
        [DROPPED, &bytes[start..]].concat()
    }
}

fn drain<R: Read + Send + 'static>(stream: Option<R>, keep: Keep) -> Output {
    let kept = Arc::new(Mutex::new(Kept::default()));
    let shared = Arc::downgrade(&kept);
    let (finished, done) = mpsc::channel();
    thread::spawn(move || {
        let _finished = finished;
        if let Some(stream) = stream {
            read_into(stream, &shared, keep);
        }
    });
    Output { kept, done }
}

fn read_into(mut stream: impl Read, kept: &Weak<Mutex<Kept>>, keep: Keep) {
    let mut chunk = [0; CHUNK];
    loop {
        let read = match stream.read(&mut chunk) {
            Ok(0) => return,
            Ok(read) => read,
            Err(err) if err.kind() == ErrorKind::Interrupted => continue,
            Err(_) => return,
        };
        let Some(kept) = kept.upgrade() else {
            return;
        };
        kept.lock()
            .unwrap_or_else(PoisonError::into_inner)
            .push(keep, &chunk[..read]);
    }
}

fn collect(output: &Output, deadline: Instant) -> Vec<u8> {
    let _ = output
        .done
        .recv_timeout(deadline.saturating_duration_since(Instant::now()));
    let mut kept = output.kept.lock().unwrap_or_else(PoisonError::into_inner);
    mem::take(&mut *kept).finish()
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

    fn run(script: &str, stderr: Keep) -> Finished {
        let mut command = Command::new("sh");
        command.args(["-c", script]);
        let Ok(finished) = execute(&mut command, Duration::from_secs(10), stderr) else {
            panic!("sh did not run to its end: {script}");
        };
        assert!(finished.status.success(), "{script}: {}", finished.status);
        finished
    }

    #[test]
    fn stdout_is_kept_up_to_its_limit_and_the_rest_is_drained() {
        let finished = run("head -c 200000 /dev/zero; echo done", Keep::Nothing);

        assert_eq!(finished.stdout.len(), ANSWER_LIMIT + 1);
    }

    #[test]
    fn discarded_stderr_keeps_nothing_and_is_drained_to_its_end() {
        let finished = run(
            r#"head -c 50000000 /dev/zero >&2; echo '{"problems":[]}'"#,
            Keep::Nothing,
        );

        assert!(
            finished.stderr.is_empty(),
            "kept {} bytes of stderr",
            finished.stderr.len()
        );
        assert_eq!(finished.stdout, b"{\"problems\":[]}\n");
    }

    #[test]
    fn stderr_shorter_than_the_tail_is_kept_whole() {
        let finished = run("echo one >&2; printf two >&2", Keep::Tail(STDERR_TAIL));

        assert_eq!(finished.stderr, "one\ntwo");
    }

    #[test]
    fn a_long_stderr_keeps_its_last_lines_after_a_marker() {
        let finished = run("seq 1 300000 >&2", Keep::Tail(STDERR_TAIL));

        let Some(kept) = finished.stderr.strip_prefix("(earlier output dropped)\n") else {
            panic!("no marker line: {:.80}", finished.stderr);
        };
        assert!(kept.len() <= STDERR_TAIL, "kept {} bytes", kept.len());
        let numbers: Vec<u32> = kept
            .lines()
            .map(|line| line.parse().expect("a whole line of seq"))
            .collect();
        assert_eq!(numbers.last(), Some(&300_000));
        assert!(numbers.len() > 100_000, "kept {} lines", numbers.len());
        assert!(
            numbers.windows(2).all(|pair| pair[1] == pair[0] + 1),
            "the kept lines are not the last ones in order"
        );
    }

    #[test]
    fn the_tail_starts_at_a_line_boundary() {
        let cases: [(&str, &[&str], &str); 5] = [
            ("fits", &["aa\n", "bb\n"], "aa\nbb\n"),
            (
                "cut inside a line",
                &["aa\nbb\ncc\n"],
                "(earlier output dropped)\nbb\ncc\n",
            ),
            (
                "cut at a newline",
                &["aa\n", "bb\n", "cc\n"],
                "(earlier output dropped)\nbb\ncc\n",
            ),
            (
                "chunk longer than the tail",
                &["x", "aa\nbb\ncc\n"],
                "(earlier output dropped)\nbb\ncc\n",
            ),
            (
                "no newline left",
                &["abcdefghij"],
                "(earlier output dropped)\nefghij",
            ),
        ];
        for (name, chunks, want) in cases {
            let mut kept = Kept::default();
            for chunk in chunks {
                kept.push(Keep::Tail(6), chunk.as_bytes());
            }
            assert_eq!(String::from_utf8_lossy(&kept.finish()), want, "{name}");
        }
    }
}
