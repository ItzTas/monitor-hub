use std::fs::{self, Permissions};
use std::os::unix::fs::PermissionsExt;
use std::path::{Path, PathBuf};
use std::sync::mpsc::{self, RecvTimeoutError};
use std::thread;
use std::time::Duration;

use monitor_hub_plugin_testkit::settings::{assert_no_drift, check};
use tempfile::TempDir;

#[path = "../../plugin/tests/support/example.rs"]
mod example;
#[path = "support/leftovers.rs"]
mod leftovers;

const FIXED_SETTINGS: &str = include_str!("../../plugin/examples/fixed-plugin/settings.json");
const EMPTY_SETTINGS: &str = r#"{"settings_version":1,"fields":[]}"#;
const BAD_SETTINGS: &str =
    r#"{"settings_version":1,"fields":[{"name":"events","label":"Events","type":"integer"}]}"#;
const CLEAN_ENVIRONMENT: &str =
    r#"[ -z "${CARGO_MANIFEST_DIR:-}" ] && [ -f settings.json ] || exit 3; echo '{"problems":[]}'"#;
const VALIDATOR: &str = "validator";
const BOUND: Duration = Duration::from_secs(10);

enum Want {
    Exact(&'static str),
    Prefix(&'static str),
}

struct Case {
    name: &'static str,
    settings: Option<&'static str>,
    script: &'static str,
    program: &'static str,
    bound: Duration,
    detached: Option<&'static str>,
    want: Vec<Want>,
}

impl Case {
    fn new(name: &'static str, script: &'static str, want: Vec<Want>) -> Case {
        Case {
            name,
            settings: Some(EMPTY_SETTINGS),
            script,
            program: VALIDATOR,
            bound: BOUND,
            detached: None,
            want,
        }
    }
}

struct Plugin {
    dir: TempDir,
    detached: Option<&'static str>,
}

impl Plugin {
    fn new(case: &Case) -> Plugin {
        let dir = TempDir::new().expect("create temp dir");
        if let Some(settings) = case.settings {
            fs::write(dir.path().join("settings.json"), settings).expect("write settings.json");
        }
        let validator = dir.path().join(VALIDATOR);
        fs::write(&validator, format!("#!/bin/sh\n{}\n", case.script))
            .expect("write the validator");
        fs::set_permissions(&validator, Permissions::from_mode(0o755)).expect("chmod");
        Plugin {
            dir,
            detached: case.detached,
        }
    }

    fn check(&self, case: &Case) -> Vec<String> {
        let dir = self.dir.path().to_owned();
        let program = dir.join(case.program);
        let (sender, receiver) = mpsc::channel();
        thread::spawn(move || {
            let _ = sender.send(check(dir, program));
        });
        match receiver.recv_timeout(case.bound) {
            Ok(violations) => violations,
            Err(RecvTimeoutError::Timeout) => {
                panic!(
                    "{}: check did not return within {:?}",
                    case.name, case.bound
                )
            }
            Err(RecvTimeoutError::Disconnected) => panic!("{}: check panicked", case.name),
        }
    }
}

impl Drop for Plugin {
    fn drop(&mut self) {
        let Ok(dir) = fs::canonicalize(self.dir.path()) else {
            return;
        };
        leftovers::kill_where(|process| {
            fs::read_link(process.join("cwd")).is_ok_and(|cwd| cwd.starts_with(&dir))
                || self
                    .detached
                    .is_some_and(|argument| leftovers::runs(process, &["sleep", argument]))
        });
    }
}

fn fixed_plugin_dir() -> PathBuf {
    Path::new(env!("CARGO_MANIFEST_DIR")).join("../plugin/examples/fixed-plugin")
}

fn matches(got: &[String], want: &[Want]) -> bool {
    got.len() == want.len()
        && got.iter().zip(want).all(|(got, want)| match want {
            Want::Exact(text) => got == text,
            Want::Prefix(text) => got.starts_with(text),
        })
}

#[test]
fn the_fixed_example_passes() {
    assert_eq!(
        check(fixed_plugin_dir(), example::fixed()),
        Vec::<String>::new()
    );
}

#[test]
fn the_fixed_example_declares_what_it_reads() {
    assert_no_drift(
        FIXED_SETTINGS,
        &["EVENTS", "LOG_DEMO", "PANIC_DEMO", "PATH"],
    );
}

#[test]
#[should_panic(
    expected = "settings.json and the configuration disagree:\n  read but not declared: EXTRA, OTHER\n  declared but not read: PANIC_DEMO"
)]
fn drift_names_both_sides() {
    assert_no_drift(
        FIXED_SETTINGS,
        &["OTHER", "EVENTS", "LOG_DEMO", "EXTRA", "EXTRA"],
    );
}

#[test]
#[should_panic(expected = "settings.json is not valid")]
fn drift_on_an_invalid_file_says_so() {
    assert_no_drift("{}", &[]);
}

#[test]
fn misbehaving_validators_are_reported() {
    let cases = [
        Case::new("clean environment", CLEAN_ENVIRONMENT, vec![]),
        Case::new(
            "surrounding whitespace",
            r#"printf ' \n{"problems":[]}\n\n'"#,
            vec![],
        ),
        Case::new(
            "log first",
            r#"echo starting; echo '{"problems":[]}'"#,
            vec![Want::Prefix(
                "validate-settings did not print a single JSON object on stdout: ",
            )],
        ),
        Case::new(
            "needs the cookie",
            "echo 'this binary is a monitor-hub plugin' >&2; exit 1",
            vec![Want::Exact(
                "validate-settings printed nothing on stdout; it runs with an empty environment and without MONITOR_HUB_PLUGIN",
            )],
        ),
        Case::new(
            "only whitespace",
            "printf ' \\n\\t\\n'; exit 1",
            vec![Want::Exact(
                "validate-settings printed nothing on stdout; it runs with an empty environment and without MONITOR_HUB_PLUGIN",
            )],
        ),
        Case {
            bound: Duration::from_secs(12),
            ..Case::new(
                "hangs",
                "exec sleep 30",
                vec![Want::Exact("validate-settings did not finish within 5s")],
            )
        },
        Case {
            bound: Duration::from_secs(3),
            ..Case::new(
                "background child",
                r#"sleep 30 & echo '{"problems":[]}'"#,
                vec![],
            )
        },
        Case {
            bound: Duration::from_secs(3),
            detached: Some("31.1"),
            ..Case::new(
                "detached child",
                r#"setsid sleep 31.1 & echo '{"problems":[]}'"#,
                vec![],
            )
        },
        Case {
            bound: Duration::from_secs(9),
            detached: Some("31.2"),
            ..Case::new(
                "detached child while hanging",
                "setsid sleep 31.2 & exec sleep 30",
                vec![Want::Exact("validate-settings did not finish within 5s")],
            )
        },
        Case::new(
            "exit 2",
            r#"echo '{"problems":[]}'; exit 2"#,
            vec![Want::Exact(
                "validate-settings exited with code 2, want 0 or 1",
            )],
        ),
        Case::new(
            "killed",
            "kill -9 $$",
            vec![Want::Exact("validate-settings was killed by signal 9")],
        ),
        Case {
            program: "absent",
            ..Case::new(
                "missing program",
                CLEAN_ENVIRONMENT,
                vec![Want::Prefix("validate-settings could not be started: ")],
            )
        },
        Case::new(
            "too much output",
            r#"head -c 65537 /dev/zero | tr '\0' ' '; echo '{"problems":[]}'"#,
            vec![Want::Exact(
                "validate-settings wrote more than 65536 bytes on stdout",
            )],
        ),
        Case::new(
            "not an object",
            "echo '[]'",
            vec![Want::Prefix(
                "validate-settings did not print a single JSON object on stdout: ",
            )],
        ),
        Case::new(
            "no message",
            r#"echo '{"problems":[{"name":"X"}]}'; exit 1"#,
            vec![Want::Prefix("validate-settings answer is invalid: ")],
        ),
        Case::new(
            "empty message",
            r#"echo '{"problems":[{"name":"X","message":""}]}'; exit 1"#,
            vec![Want::Exact(
                "validate-settings answer is invalid: problem 0 has an empty message",
            )],
        ),
        Case::new(
            "empty message with code 0",
            r#"echo '{"problems":[{"name":"X","message":""}]}'"#,
            vec![Want::Exact(
                "validate-settings answer is invalid: problem 0 has an empty message",
            )],
        ),
        Case::new(
            "problems with code 0",
            r#"echo '{"problems":[{"name":"X","message":"wrong"}]}'"#,
            vec![Want::Exact(
                "validate-settings exited with code 0 but listed problems",
            )],
        ),
        Case::new(
            "no problems with code 1",
            r#"echo '{"problems":[]}'; exit 1"#,
            vec![Want::Exact(
                "validate-settings exited with code 1 but listed no problems",
            )],
        ),
        Case {
            settings: Some(BAD_SETTINGS),
            ..Case::new(
                "bad file",
                CLEAN_ENVIRONMENT,
                vec![Want::Exact(
                    "settings.json: fields[0]: name \"events\" does not match ^[A-Z][A-Z0-9_]*$",
                )],
            )
        },
        Case {
            settings: None,
            ..Case::new(
                "no file",
                CLEAN_ENVIRONMENT,
                vec![
                    Want::Prefix("settings.json: cannot read "),
                    Want::Exact("validate-settings exited with code 3, want 0 or 1"),
                ],
            )
        },
    ];
    let plugins: Vec<Plugin> = cases.iter().map(Plugin::new).collect();

    for (case, plugin) in cases.iter().zip(&plugins) {
        let got = plugin.check(case);

        assert!(matches(&got, &case.want), "{}: {got:?}", case.name);
    }
}
