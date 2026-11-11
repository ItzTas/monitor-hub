use std::collections::hash_map::RandomState;
use std::env;
use std::ffi::OsString;
use std::fs::{self, DirBuilder, Permissions};
use std::hash::BuildHasher;
use std::io::{self, ErrorKind, Read};
use std::os::unix::fs::{DirBuilderExt, MetadataExt, PermissionsExt};
use std::os::unix::process::{CommandExt, ExitStatusExt};
use std::path::{Path, PathBuf};
use std::process::{self, Child, Command, ExitStatus, Stdio};
use std::sync::{Mutex, Once, OnceLock, PoisonError};
use std::thread::{self, JoinHandle};
use std::time::{Duration, Instant};

use crate::ci;

pub const SDK_GO_VERSION: &str = "sdk/v0.2.1";

const GO_PACKAGE: &str = "github.com/ItzTas/monitor-hub/sdk/cmd/plugin-conformance";
const BINARY: &str = "plugin-conformance";
const CACHE_PATH: [&str; 2] = ["monitor-hub", "plugin-conformance"];
const TEMPORARY_PREFIX: &str = "monitor-hub-conformance";
const STAGING_PREFIX: &str = ".install";
const USER_PROBE_PREFIX: &str = ".monitor-hub-user";
const UNIQUE_ATTEMPTS: u64 = 16;
const COMMAND_ENV: &str = "PLUGIN_CONFORMANCE";
const INSTALL_LIMIT: Duration = Duration::from_secs(600);
const ALLOWANCE: Duration = Duration::from_secs(120);
const GRACE: Duration = Duration::from_secs(5);
const POLL: Duration = Duration::from_millis(20);
const EXIT_VIOLATIONS: i32 = 1;
const EXIT_USAGE: i32 = 2;

#[non_exhaustive]
pub struct Conformance {
    pub command: Vec<String>,
    pub env: Vec<(String, String)>,
    pub min_events: usize,
    pub ready_timeout: Duration,
}

impl Conformance {
    pub fn new(program: impl Into<String>) -> Self {
        Self {
            command: vec![program.into()],
            env: Vec::new(),
            min_events: 0,
            ready_timeout: Duration::ZERO,
        }
    }
}

#[derive(Debug, PartialEq, Eq)]
#[non_exhaustive]
pub enum Outcome {
    Passed,
    Skipped(String),
    Failed(String),
}

enum Source {
    Binary(OsString),
    Go(Install),
}

struct Install {
    go: Vec<OsString>,
    package: String,
    version: String,
    env: Vec<(String, String)>,
    cache: Option<PathBuf>,
    limit: Duration,
}

enum Location {
    Cache(PathBuf),
    Temporary(String),
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
    let mut command = Command::new(&program);
    command.args(arguments(conformance));
    match execute(&mut command, limit) {
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
    if ci::active() {
        panic!("the conformance command is required when CI is set: {reason}");
    }
    Outcome::Skipped(reason.to_owned())
}

fn locate() -> Option<Source> {
    if let Some(path) = env::var_os(COMMAND_ENV).filter(|value| !value.is_empty()) {
        return Some(Source::Binary(path));
    }
    on_path("go").then(|| {
        Source::Go(Install {
            go: vec!["go".into()],
            package: GO_PACKAGE.to_owned(),
            version: SDK_GO_VERSION
                .rsplit('/')
                .next()
                .unwrap_or(SDK_GO_VERSION)
                .to_owned(),
            env: Vec::new(),
            cache: cache_base(env::var_os("XDG_CACHE_HOME"), env::var_os("HOME")),
            limit: INSTALL_LIMIT,
        })
    })
}

fn cache_base(xdg: Option<OsString>, home: Option<OsString>) -> Option<PathBuf> {
    let absolute = |value: OsString| Some(PathBuf::from(value)).filter(|path| path.is_absolute());
    xdg.and_then(absolute)
        .or_else(|| home.and_then(absolute).map(|home| home.join(".cache")))
}

fn obtain(source: Source) -> Result<OsString, String> {
    match source {
        Source::Binary(path) => Ok(path),
        Source::Go(install) => {
            let key = format!("{}@{}", install.package, install.version);
            self::install(&install, &key)
                .map(PathBuf::into_os_string)
                .map_err(|detail| {
                    format!(
                        "the conformance command {key} could not be fetched or built:\n{detail}"
                    )
                })
        }
    }
}

fn install(install: &Install, key: &str) -> Result<PathBuf, String> {
    static INSTALLING: Mutex<()> = Mutex::new(());
    let _installing = INSTALLING.lock().unwrap_or_else(PoisonError::into_inner);
    let owner = current_user()?;
    let dir = match location(install.cache.as_deref(), &install.version, owner) {
        Location::Cache(dir) => dir,
        Location::Temporary(reason) => {
            report_fallback(&reason);
            temporary_dir()?
        }
    };
    let binary = dir.join(BINARY);
    if reusable(&binary, owner) {
        return Ok(binary);
    }
    remove_stale_staging(&dir, install.limit);
    let staging = unique_dir(&dir, STAGING_PREFIX)?;
    let installed = go_install(install, key, &staging).and_then(|()| {
        fs::rename(staging.join(BINARY), &binary).map_err(|err| {
            format!(
                "cannot move the built command to {}: {err}",
                binary.display()
            )
        })
    });
    let _ = fs::remove_dir_all(&staging);
    installed.map(|()| binary)
}

fn location(base: Option<&Path>, version: &str, owner: u32) -> Location {
    let Some(base) = base else {
        return Location::Temporary(
            "neither XDG_CACHE_HOME nor HOME is an absolute path".to_owned(),
        );
    };
    match cache_dir(base, version, owner) {
        Ok(dir) => Location::Cache(dir),
        Err(reason) => Location::Temporary(reason),
    }
}

fn cache_dir(base: &Path, version: &str, owner: u32) -> Result<PathBuf, String> {
    if fs::symlink_metadata(base).is_err_and(|err| err.kind() == ErrorKind::NotFound) {
        DirBuilder::new()
            .recursive(true)
            .mode(0o700)
            .create(base)
            .map_err(|err| format!("cannot create {}: {err}", base.display()))?;
    }
    let found = owned_dir(base, owner)?;
    if found.mode() & 0o022 != 0 {
        return Err(format!("{} is writable by group or others", base.display()));
    }
    let mut dir = base.to_owned();
    for part in CACHE_PATH.into_iter().chain([version]) {
        dir.push(part);
        private_dir(&dir, owner)?;
    }
    Ok(dir)
}

fn private_dir(dir: &Path, owner: u32) -> Result<(), String> {
    match DirBuilder::new().mode(0o700).create(dir) {
        Ok(()) => {}
        Err(err) if err.kind() == ErrorKind::AlreadyExists => {}
        Err(err) => return Err(format!("cannot create {}: {err}", dir.display())),
    }
    owned_dir(dir, owner)?;
    fs::set_permissions(dir, Permissions::from_mode(0o700))
        .map_err(|err| format!("cannot make {} private: {err}", dir.display()))
}

fn owned_dir(dir: &Path, owner: u32) -> Result<fs::Metadata, String> {
    let found = fs::symlink_metadata(dir)
        .map_err(|err| format!("cannot inspect {}: {err}", dir.display()))?;
    if found.file_type().is_symlink() {
        return Err(format!("{} is a symbolic link", dir.display()));
    }
    if !found.is_dir() {
        return Err(format!("{} is not a directory", dir.display()));
    }
    if found.uid() != owner {
        return Err(format!(
            "{} belongs to user {}, not to user {owner}",
            dir.display(),
            found.uid()
        ));
    }
    Ok(found)
}

fn reusable(binary: &Path, owner: u32) -> bool {
    fs::symlink_metadata(binary)
        .is_ok_and(|found| found.is_file() && found.uid() == owner && found.mode() & 0o100 != 0)
}

fn remove_stale_staging(dir: &Path, limit: Duration) {
    let Ok(entries) = fs::read_dir(dir) else {
        return;
    };
    let staging = format!("{STAGING_PREFIX}-");
    for entry in entries.flatten() {
        let stale = entry.file_name().to_string_lossy().starts_with(&staging)
            && entry
                .metadata()
                .and_then(|found| found.modified())
                .is_ok_and(|modified| modified.elapsed().is_ok_and(|age| age > limit));
        if stale {
            let _ = fs::remove_dir_all(entry.path());
        }
    }
}

fn report_fallback(reason: &str) {
    static REPORTED: Once = Once::new();
    REPORTED.call_once(|| {
        eprintln!(
            "the conformance command is installed in a temporary directory for this process, because the cache directory cannot be used: {reason}"
        );
    });
}

fn current_user() -> Result<u32, String> {
    static USER: OnceLock<Result<u32, String>> = OnceLock::new();
    USER.get_or_init(|| {
        let probe = unique_dir(&env::temp_dir(), USER_PROBE_PREFIX)?;
        let owner = fs::symlink_metadata(&probe)
            .map(|found| found.uid())
            .map_err(|err| format!("cannot inspect {}: {err}", probe.display()));
        let _ = fs::remove_dir(&probe);
        owner
    })
    .clone()
}

fn temporary_dir() -> Result<PathBuf, String> {
    static DIR: OnceLock<Result<PathBuf, String>> = OnceLock::new();
    DIR.get_or_init(|| unique_dir(&env::temp_dir(), TEMPORARY_PREFIX))
        .clone()
}

fn unique_dir(parent: &Path, prefix: &str) -> Result<PathBuf, String> {
    let state = RandomState::new();
    for attempt in 0..UNIQUE_ATTEMPTS {
        let dir = parent.join(format!(
            "{prefix}-{}-{:016x}",
            process::id(),
            state.hash_one(attempt)
        ));
        match DirBuilder::new().mode(0o700).create(&dir) {
            Ok(()) => return Ok(dir),
            Err(err) if err.kind() == ErrorKind::AlreadyExists => {}
            Err(err) => return Err(format!("cannot create {}: {err}", dir.display())),
        }
    }
    Err(format!(
        "cannot create a directory with a unique name in {}",
        parent.display()
    ))
}

fn go_install(install: &Install, key: &str, staging: &Path) -> Result<(), String> {
    let Some((go, leading)) = install.go.split_first() else {
        return Err("no go command to run".to_owned());
    };
    let mut command = Command::new(go);
    command
        .args(leading)
        .args(["install", key])
        .env("GOBIN", staging)
        .envs(install.env.iter().map(|(name, value)| (name, value)));
    match execute(&mut command, install.limit) {
        Ok(finished) if finished.status.success() => Ok(()),
        Ok(finished) => Err(finished.stderr.trim().to_owned()),
        Err(Failure::Start(err)) => Err(format!("cannot run go: {err}")),
        Err(Failure::Timeout(stderr)) => Err(format!(
            "go install did not finish within {:?} and was stopped\n{}",
            install.limit,
            stderr.trim()
        )),
    }
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

fn execute(command: &mut Command, limit: Duration) -> Result<Finished, Failure> {
    let mut child = command
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
    use std::fs::File;
    use std::os::unix::fs::{PermissionsExt, symlink};
    use std::time::SystemTime;

    use tempfile::TempDir;

    use super::*;

    const SUCCEEDS: &str = "echo call >>\"$FAKE_GO_CALLS\"\nprintf '#!/bin/sh\\n' >\"$GOBIN/plugin-conformance\"\nchmod 755 \"$GOBIN/plugin-conformance\"\n";
    const FAILS: &str = "echo 'go: module not found' >&2\nexit 1\n";
    const HANGS: &str = "exec sleep 30\n";

    struct Fake {
        dir: TempDir,
    }

    impl Fake {
        fn new(script: &str) -> Fake {
            let dir = TempDir::new().expect("create temp dir");
            fs::write(dir.path().join("go.sh"), script).expect("write the fake go");
            Fake { dir }
        }

        fn install(&self, cache: Option<PathBuf>, limit: Duration) -> Install {
            Install {
                go: vec!["sh".into(), self.dir.path().join("go.sh").into()],
                package: GO_PACKAGE.to_owned(),
                version: "v1.2.3".to_owned(),
                env: vec![(
                    "FAKE_GO_CALLS".to_owned(),
                    self.calls_file().to_string_lossy().into_owned(),
                )],
                cache,
                limit,
            }
        }

        fn calls_file(&self) -> PathBuf {
            self.dir.path().join("calls")
        }

        fn calls(&self) -> usize {
            fs::read_to_string(self.calls_file()).map_or(0, |calls| calls.lines().count())
        }
    }

    fn mode(path: &Path) -> u32 {
        fs::symlink_metadata(path)
            .expect("stat")
            .permissions()
            .mode()
            & 0o7777
    }

    fn names(dir: &Path) -> Vec<String> {
        fs::read_dir(dir)
            .expect("read dir")
            .map(|entry| {
                entry
                    .expect("dir entry")
                    .file_name()
                    .to_string_lossy()
                    .into_owned()
            })
            .collect()
    }

    #[test]
    fn go_mode_installs_once_per_version_into_a_private_cache_dir() {
        let fake = Fake::new(SUCCEEDS);
        let cache = TempDir::new().expect("create temp dir");
        let install = fake.install(Some(cache.path().to_owned()), INSTALL_LIMIT);

        let first = obtain(Source::Go(
            fake.install(Some(cache.path().to_owned()), INSTALL_LIMIT),
        ));
        let second = obtain(Source::Go(install));

        let tool = cache.path().join("monitor-hub");
        let version = tool.join("plugin-conformance/v1.2.3");
        let binary = version.join("plugin-conformance").into_os_string();
        assert_eq!(first, Ok(binary.clone()));
        assert_eq!(second, Ok(binary));
        assert_eq!(fake.calls(), 1);
        for dir in [&tool, &tool.join("plugin-conformance"), &version] {
            assert_eq!(mode(dir), 0o700, "{}", dir.display());
        }
        assert_eq!(names(&version), ["plugin-conformance"]);
    }

    fn owner() -> u32 {
        current_user().expect("find the current user")
    }

    fn set_mode(path: &Path, mode: u32) {
        fs::set_permissions(path, Permissions::from_mode(mode)).expect("chmod");
    }

    fn temporary_reason(location: Location) -> String {
        match location {
            Location::Temporary(reason) => reason,
            Location::Cache(dir) => {
                panic!("expected the temporary fallback, got {}", dir.display())
            }
        }
    }

    #[test]
    fn the_current_user_is_the_owner_of_what_this_process_creates() {
        let dir = TempDir::new().expect("create temp dir");

        assert_eq!(owner(), fs::metadata(dir.path()).expect("stat").uid());
    }

    #[test]
    fn an_unusable_or_unsafe_cache_base_falls_back_to_a_temporary_dir() {
        let root = TempDir::new().expect("create temp dir");
        let path = |name: &str| root.path().join(name);
        fs::write(path("file"), "").expect("write a file");
        fs::create_dir(path("real")).expect("create a dir");
        symlink(path("real"), path("link")).expect("symlink");
        fs::create_dir(path("open")).expect("create a dir");
        set_mode(&path("open"), 0o777);
        fs::create_dir(path("locked")).expect("create a dir");
        set_mode(&path("locked"), 0o555);
        fs::create_dir(path("inner")).expect("create a dir");
        symlink(path("real"), path("inner").join("monitor-hub")).expect("symlink");
        let cases = [
            (Some(path("file")), owner(), "is not a directory"),
            (Some(path("link")), owner(), "is a symbolic link"),
            (
                Some(path("open")),
                owner(),
                "is writable by group or others",
            ),
            (Some(path("locked").join("cache")), owner(), "cannot create"),
            (Some(path("real")), owner() + 1, "belongs to user"),
            (Some(path("inner")), owner(), "is a symbolic link"),
            (
                None,
                owner(),
                "neither XDG_CACHE_HOME nor HOME is an absolute path",
            ),
        ];

        for (base, uid, want) in cases {
            let reason = temporary_reason(location(base.as_deref(), "v1.2.3", uid));
            assert!(reason.contains(want), "{base:?}: {reason}");
        }
        set_mode(&path("locked"), 0o755);
    }

    #[test]
    fn an_existing_cache_dir_with_a_wider_mode_is_made_private() {
        let cache = TempDir::new().expect("create temp dir");
        let tool = cache.path().join("monitor-hub");
        fs::create_dir(&tool).expect("create a dir");
        set_mode(&tool, 0o755);

        let found = location(Some(cache.path()), "v1.2.3", owner());

        assert!(matches!(found, Location::Cache(_)));
        assert_eq!(mode(&tool), 0o700);
    }

    #[test]
    fn a_cached_binary_is_reused_only_when_it_is_an_executable_file_of_the_user() {
        let dir = TempDir::new().expect("create temp dir");
        let binary = dir.path().join("plugin-conformance");
        fs::write(&binary, "").expect("write a file");
        set_mode(&binary, 0o755);

        assert!(reusable(&binary, owner()));
        assert!(!reusable(&binary, owner() + 1));
        set_mode(&binary, 0o644);
        assert!(!reusable(&binary, owner()));
    }

    #[test]
    fn a_cached_binary_without_the_execute_bit_is_reinstalled() {
        let fake = Fake::new(SUCCEEDS);
        let cache = TempDir::new().expect("create temp dir");
        let binary = PathBuf::from(
            obtain(Source::Go(
                fake.install(Some(cache.path().to_owned()), INSTALL_LIMIT),
            ))
            .expect("first install"),
        );
        set_mode(&binary, 0o644);

        let again = obtain(Source::Go(
            fake.install(Some(cache.path().to_owned()), INSTALL_LIMIT),
        ));

        assert_eq!(again, Ok(binary.clone().into_os_string()));
        assert_eq!(fake.calls(), 2);
        assert_eq!(mode(&binary), 0o755);
    }

    #[test]
    fn staging_dirs_older_than_the_install_limit_are_removed_when_an_install_starts() {
        let fake = Fake::new(SUCCEEDS);
        let cache = TempDir::new().expect("create temp dir");
        let version = cache.path().join("monitor-hub/plugin-conformance/v1.2.3");
        fs::create_dir_all(&version).expect("create the version dir");
        let (stale, fresh) = (
            version.join(".install-stale"),
            version.join(".install-fresh"),
        );
        fs::create_dir_all(stale.join("leftover")).expect("create a stale dir");
        fs::create_dir(&fresh).expect("create a fresh dir");
        File::open(&stale)
            .and_then(|dir| dir.set_modified(SystemTime::now() - 2 * INSTALL_LIMIT))
            .expect("age the stale dir");

        obtain(Source::Go(
            fake.install(Some(cache.path().to_owned()), INSTALL_LIMIT),
        ))
        .expect("install");

        let mut left = names(&version);
        left.sort();
        assert_eq!(left, [".install-fresh", "plugin-conformance"]);
    }

    #[test]
    fn without_a_usable_cache_base_go_mode_installs_into_one_private_temporary_dir() {
        let fake = Fake::new(SUCCEEDS);
        let locked = TempDir::new().expect("create temp dir");
        set_mode(locked.path(), 0o555);

        let without = obtain(Source::Go(fake.install(None, INSTALL_LIMIT))).map(PathBuf::from);
        let uncreatable = obtain(Source::Go(
            fake.install(Some(locked.path().join(".cache")), INSTALL_LIMIT),
        ))
        .map(PathBuf::from);

        set_mode(locked.path(), 0o755);
        let binary = without.expect("install without a base");
        let dir = binary.parent().expect("install dir").to_owned();
        let name = dir
            .file_name()
            .expect("dir name")
            .to_string_lossy()
            .into_owned();
        let _ = fs::remove_dir_all(&dir);
        assert_eq!(uncreatable, Ok(binary.clone()));
        assert_eq!(dir.parent(), Some(env::temp_dir().as_path()));
        assert!(name.starts_with("monitor-hub-conformance-"), "{name}");
        assert_eq!(binary.file_name(), Some("plugin-conformance".as_ref()));
        assert_eq!(fake.calls(), 1);
    }

    #[test]
    fn a_command_that_cannot_be_fetched_is_not_a_plugin_violation() {
        let fake = Fake::new(FAILS);
        let cache = TempDir::new().expect("create temp dir");

        let outcome = run_from(
            Source::Go(fake.install(Some(cache.path().to_owned()), INSTALL_LIMIT)),
            &Conformance::new("unused"),
        );

        let Outcome::Failed(message) = outcome else {
            panic!("a failed go install must fail the run");
        };
        assert!(message.starts_with("the conformance command "), "{message}");
        assert!(
            message.ends_with("could not be fetched or built:\ngo: module not found"),
            "{message}"
        );
        assert!(names(&cache.path().join("monitor-hub/plugin-conformance/v1.2.3")).is_empty());
    }

    #[test]
    fn a_go_install_that_hangs_is_stopped_at_its_limit() {
        let fake = Fake::new(HANGS);
        let cache = TempDir::new().expect("create temp dir");
        let started = Instant::now();

        let Err(message) = obtain(Source::Go(
            fake.install(Some(cache.path().to_owned()), Duration::from_millis(200)),
        )) else {
            panic!("a hanging go install must fail");
        };

        assert!(message.contains("did not finish within 200ms"), "{message}");
        assert!(started.elapsed() < GRACE, "took {:?}", started.elapsed());
    }

    #[test]
    fn the_cache_dir_is_xdg_cache_home_then_home_dot_cache() {
        let cases = [
            ((Some("/x"), Some("/h")), Some("/x")),
            ((Some("relative"), Some("/h")), Some("/h/.cache")),
            ((Some(""), Some("/h")), Some("/h/.cache")),
            ((None, Some("/h")), Some("/h/.cache")),
            ((None, Some("h")), None),
            ((None, None), None),
        ];
        for ((xdg, home), want) in cases {
            assert_eq!(
                cache_base(xdg.map(OsString::from), home.map(OsString::from)),
                want.map(PathBuf::from),
                "XDG_CACHE_HOME={xdg:?} HOME={home:?}"
            );
        }
    }
}
