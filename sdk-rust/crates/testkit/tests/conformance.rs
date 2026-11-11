use std::path::{Path, PathBuf};
use std::process::{self, Command};
use std::sync::OnceLock;
use std::time::Duration;
use std::{env, fs};

use monitor_hub_plugin_testkit::conformance::{Conformance, Outcome, run};

struct Setup {
    example: PathBuf,
}

fn ci() -> bool {
    env::var_os("CI").is_some_and(|value| !value.is_empty())
}

fn unavailable(reason: &str) -> Option<&'static Setup> {
    assert!(!ci(), "required when CI is set: {reason}");
    eprintln!("skipping: {reason}");
    None
}

fn build_command() -> Result<PathBuf, String> {
    let sdk = Path::new(env!("CARGO_MANIFEST_DIR")).join("../../../sdk");
    let dir = Path::new(env!("CARGO_TARGET_TMPDIR"));
    let binary = dir.join("plugin-conformance");
    let staging = dir.join(format!("plugin-conformance.{}", process::id()));
    let output = Command::new("go")
        .current_dir(sdk)
        .args(["build", "-o"])
        .arg(&staging)
        .arg("./cmd/plugin-conformance")
        .output()
        .map_err(|err| format!("cannot run go: {err}"))?;
    if !output.status.success() {
        return Err(format!(
            "go build failed: {}",
            String::from_utf8_lossy(&output.stderr)
        ));
    }
    fs::rename(&staging, &binary).map_err(|err| format!("cannot move the built command: {err}"))?;
    Ok(binary)
}

fn build_example() -> PathBuf {
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
    test_binary
        .parent()
        .and_then(Path::parent)
        .expect("target profile dir")
        .join("examples/fixed")
}

fn setup() -> Option<&'static Setup> {
    static SETUP: OnceLock<Option<Setup>> = OnceLock::new();
    SETUP
        .get_or_init(|| {
            let binary = match build_command() {
                Ok(binary) => binary,
                Err(reason) => {
                    assert!(!ci(), "required when CI is set: {reason}");
                    eprintln!("skipping: {reason}");
                    return None;
                }
            };
            unsafe { env::set_var("PLUGIN_CONFORMANCE", binary) };
            Some(Setup {
                example: build_example(),
            })
        })
        .as_ref()
        .or_else(|| unavailable("go is not available"))
}

fn conformance(setup: &Setup, min_events: usize) -> Conformance {
    Conformance {
        command: vec![setup.example.to_string_lossy().into_owned()],
        env: vec![("EVENTS".into(), "2".into())],
        min_events,
        ready_timeout: Duration::from_secs(5),
    }
}

#[test]
fn a_conforming_plugin_passes() {
    let Some(setup) = setup() else { return };
    assert_eq!(run(&conformance(setup, 2)), Outcome::Passed);
}

#[test]
fn a_violation_is_reported_with_the_command_output() {
    let Some(setup) = setup() else { return };
    let Outcome::Failed(output) = run(&conformance(setup, 5)) else {
        panic!("a plugin with two events must fail min_events 5");
    };
    assert!(output.contains("got 2 events, want at least 5"), "{output}");
}

#[test]
fn an_env_name_that_is_empty_or_holds_an_equals_sign_is_rejected() {
    for name in ["", "A=B"] {
        let conformance = Conformance {
            command: vec!["unused".into()],
            env: vec![(name.into(), "1".into())],
            min_events: 0,
            ready_timeout: Duration::ZERO,
        };
        let Outcome::Failed(message) = run(&conformance) else {
            panic!("the name {name:?} must be rejected");
        };
        assert!(message.contains(&format!("{name:?}")), "{message}");
    }
}
