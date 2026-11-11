use std::path::{Path, PathBuf};
use std::process::{self, Command};
use std::sync::OnceLock;
use std::time::Duration;
use std::{env, fs};

use monitor_hub_plugin_testkit::conformance::{Conformance, Outcome, run};
use tempfile::TempDir;

#[path = "../src/ci.rs"]
mod ci;
#[path = "../../plugin/tests/support/example.rs"]
mod example;

struct Setup {
    example: PathBuf,
}

fn unavailable(reason: &str) -> Option<&'static Setup> {
    assert!(!ci::active(), "required when CI is set: {reason}");
    eprintln!("skipping: {reason}");
    None
}

fn build_command() -> Option<PathBuf> {
    let sdk = Path::new(env!("CARGO_MANIFEST_DIR")).join("../../../sdk");
    let dir = Path::new(env!("CARGO_TARGET_TMPDIR"));
    let binary = dir.join("plugin-conformance");
    let staging = dir.join(format!("plugin-conformance.{}", process::id()));
    let output = match Command::new("go")
        .current_dir(sdk)
        .args(["build", "-o"])
        .arg(&staging)
        .arg("./cmd/plugin-conformance")
        .output()
    {
        Ok(output) => output,
        Err(err) => {
            unavailable(&format!("cannot run go: {err}"));
            return None;
        }
    };
    assert!(
        output.status.success(),
        "go build ./cmd/plugin-conformance failed:\n{}",
        String::from_utf8_lossy(&output.stderr)
    );
    fs::rename(&staging, &binary).expect("move the built command into place");
    Some(binary)
}

fn setup() -> Option<&'static Setup> {
    static SETUP: OnceLock<Option<Setup>> = OnceLock::new();
    SETUP
        .get_or_init(|| {
            let binary = build_command()?;
            unsafe { env::set_var("PLUGIN_CONFORMANCE", binary) };
            Some(Setup {
                example: example::fixed().to_owned(),
            })
        })
        .as_ref()
        .or_else(|| unavailable("go is not available"))
}

fn conformance(setup: &Setup, min_events: usize) -> Conformance {
    let mut conformance = Conformance::new(setup.example.to_string_lossy());
    conformance.env.push(("EVENTS".into(), "2".into()));
    conformance.min_events = min_events;
    conformance.ready_timeout = Duration::from_secs(5);
    conformance
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
fn a_plugin_that_fails_before_the_handshake_shows_its_stderr_first() {
    let Some(setup) = setup() else { return };
    let dir = TempDir::new().expect("create temp dir");
    let mut conformance = conformance(setup, 0);
    let missing = dir.path().join("missing");
    conformance
        .env
        .push(("TMPDIR".into(), missing.to_string_lossy().into_owned()));

    let Outcome::Failed(output) = run(&conformance) else {
        panic!("a plugin that cannot create its socket must fail");
    };

    let lines: Vec<&str> = output.lines().collect();
    let plugin = lines.iter().position(|line| {
        line.starts_with("plugin: {") && line.contains("cannot create the plugin socket")
    });
    let own = lines
        .iter()
        .position(|line| line.starts_with("plugin-conformance: "));
    assert!(
        matches!((plugin, own), (Some(plugin), Some(own)) if plugin < own),
        "{output}"
    );
}

#[test]
fn an_env_name_that_is_empty_or_holds_an_equals_sign_is_rejected() {
    for name in ["", "A=B"] {
        let mut conformance = Conformance::new("unused");
        conformance.env.push((name.into(), "1".into()));
        let Outcome::Failed(message) = run(&conformance) else {
            panic!("the name {name:?} must be rejected");
        };
        assert!(message.contains(&format!("{name:?}")), "{message}");
    }
}
