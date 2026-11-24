use std::env;
use std::fs::{self, Permissions};
use std::os::unix::fs::PermissionsExt;
use std::path::{Path, PathBuf};
use std::sync::mpsc::{self, RecvTimeoutError};
use std::thread;
use std::time::Duration;

use monitor_hub_sdk_testkit::settings::check;
use tempfile::TempDir;

#[path = "../../sdk/tests/support/example.rs"]
mod example;
#[path = "support/leftovers.rs"]
mod leftovers;

const EMPTY_SETTINGS: &str = r#"{"settings_version":2,"fields":[]}"#;
const BAD_SETTINGS: &str = r#"{"settings_version":2,"fields":[{"name":"Events","label":"Events","type":"integer","env":"EVENTS"}]}"#;
const API_SETTINGS: &str =
    r#"{"settings_version":2,"fields":[{"name":"api_url","label":"API","type":"string"}]}"#;
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
            bound: BOUND,
            detached: None,
            want,
        }
    }
}

struct FakeMonitor {
    dir: TempDir,
    detached: Option<&'static str>,
}

impl FakeMonitor {
    fn new(case: &Case) -> FakeMonitor {
        let dir = TempDir::new().expect("create temp dir");
        if let Some(settings) = case.settings {
            fs::write(dir.path().join("settings.json"), settings).expect("write settings.json");
        }
        let validator = dir.path().join(VALIDATOR);
        fs::write(&validator, format!("#!/bin/sh\n{}\n", case.script))
            .expect("write the validator");
        fs::set_permissions(&validator, Permissions::from_mode(0o755)).expect("chmod");
        FakeMonitor {
            dir,
            detached: case.detached,
        }
    }

    fn check(&self, case: &Case) -> Vec<String> {
        let dir = self.dir.path().to_owned();
        let program = dir.join(VALIDATOR);
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

impl Drop for FakeMonitor {
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

fn fixed_monitor_dir() -> PathBuf {
    Path::new(env!("CARGO_MANIFEST_DIR")).join("../sdk/examples/fixed-monitor")
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
        check(fixed_monitor_dir(), example::fixed()),
        Vec::<String>::new()
    );
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
            "echo 'this binary is a monitor for monitor-hub' >&2; exit 1",
            vec![Want::Exact(
                "validate-settings printed nothing on stdout; it runs with an empty environment and without MONITOR_HUB_MONITOR",
            )],
        ),
        Case::new(
            "only whitespace",
            "printf ' \\n\\t\\n'; exit 1",
            vec![Want::Exact(
                "validate-settings printed nothing on stdout; it runs with an empty environment and without MONITOR_HUB_MONITOR",
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
        Case::new(
            "loud stderr",
            r#"head -c 50000000 /dev/zero >&2; echo '{"problems":[]}'"#,
            vec![],
        ),
        Case {
            bound: Duration::from_secs(12),
            ..Case::new(
                "hangs while logging",
                "exec yes 'validating the settings, still validating the settings' >&2",
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
            r#"echo '{"problems":[{"path":"X"}]}'; exit 1"#,
            vec![Want::Prefix("validate-settings answer is invalid: ")],
        ),
        Case::new(
            "positional problem",
            r#"echo '{"problems":[["API_URL","set it"]]}'; exit 1"#,
            vec![Want::Exact(
                "validate-settings answer is invalid: problem 0 is not an object",
            )],
        ),
        Case::new(
            "empty message",
            r#"echo '{"problems":[{"path":"X","message":""}]}'; exit 1"#,
            vec![Want::Exact(
                "validate-settings answer is invalid: problem 0 has an empty message",
            )],
        ),
        Case::new(
            "blank message",
            r#"printf '{"problems":[{"path":"X","message":" \\t"}]}'; exit 1"#,
            vec![Want::Exact(
                "validate-settings answer is invalid: problem 0 has an empty message",
            )],
        ),
        Case::new(
            "empty message with code 0",
            r#"echo '{"problems":[{"path":"X","message":""}]}'"#,
            vec![Want::Exact(
                "validate-settings answer is invalid: problem 0 has an empty message",
            )],
        ),
        Case::new(
            "problems with code 0",
            r#"echo '{"problems":[{"path":"X","message":"wrong"}]}'"#,
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
            settings: Some(API_SETTINGS),
            ..Case::new(
                "declared names",
                r#"echo '{"problems":[{"path":"api_url","message":"set it"},{"path":"","message":"together"},{"message":"no path"}]}'; exit 1"#,
                vec![],
            )
        },
        Case {
            settings: Some(API_SETTINGS),
            ..Case::new(
                "undeclared name",
                r#"echo '{"problems":[{"path":"api_url","message":"set it"},{"path":"","message":"together"},{"path":"not_declared","message":"m"}]}'; exit 1"#,
                vec![Want::Exact(
                    r#"validate-settings answer is invalid: problem 2 names the path "not_declared", which settings.json does not declare"#,
                )],
            )
        },
        Case {
            settings: Some(API_SETTINGS),
            ..Case::new(
                "undeclared name with code 0",
                r#"echo '{"problems":[{"path":"not_declared","message":"m"}]}'"#,
                vec![Want::Exact(
                    "validate-settings exited with code 0 but listed problems",
                )],
            )
        },
        Case {
            settings: Some(API_SETTINGS),
            ..Case::new(
                "undeclared name after an empty message",
                r#"echo '{"problems":[{"path":"not_declared","message":"m"},{"path":"api_url","message":""}]}'; exit 1"#,
                vec![Want::Exact(
                    "validate-settings answer is invalid: problem 1 has an empty message",
                )],
            )
        },
        Case::new(
            "undeclared path",
            r#"echo '{"problems":[{"path":"rules[0].nope","message":"m"}]}'; exit 1"#,
            vec![Want::Exact(
                r#"validate-settings answer is invalid: problem 0 names the path "rules[0].nope", which settings.json does not declare"#,
            )],
        ),
        Case::new(
            "version 1 answer",
            r#"echo '{"problems":[{"name":"X","message":"m"}]}'; exit 1"#,
            vec![],
        ),
        Case {
            settings: Some(BAD_SETTINGS),
            ..Case::new(
                "undeclared name with a bad file",
                r#"echo '{"problems":[{"path":"not_declared","message":"m"}]}'; exit 1"#,
                vec![Want::Exact(
                    "settings.json: fields[0]: name \"Events\" does not match ^[a-z][a-z0-9_]*$",
                )],
            )
        },
        Case {
            settings: Some(BAD_SETTINGS),
            ..Case::new(
                "bad file",
                CLEAN_ENVIRONMENT,
                vec![Want::Exact(
                    "settings.json: fields[0]: name \"Events\" does not match ^[a-z][a-z0-9_]*$",
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
    let monitors: Vec<FakeMonitor> = cases.iter().map(FakeMonitor::new).collect();

    for (case, monitor) in cases.iter().zip(&monitors) {
        let got = monitor.check(case);

        assert!(matches(&got, &case.want), "{}: {got:?}", case.name);
    }
}

#[test]
fn a_validator_that_cannot_start_names_the_path_it_tried() {
    let dir = TempDir::new().expect("create temp dir");
    fs::write(dir.path().join("settings.json"), EMPTY_SETTINGS).expect("write settings.json");
    let absent = dir.path().join("absent");
    let relative = "absent-validator";
    let tried = env::current_dir()
        .expect("current directory")
        .join(relative);
    let missing = dir.path().join("missing");
    let not_found = "No such file or directory (os error 2)";

    assert_eq!(
        check(dir.path(), &absent),
        [format!(
            "validate-settings could not be started: {}: {not_found}",
            absent.display()
        )]
    );
    assert_eq!(
        check(dir.path(), relative),
        [format!(
            "validate-settings could not be started: {}: {not_found}",
            tried.display()
        )]
    );
    assert_eq!(
        check(&missing, &absent),
        [
            format!(
                "settings.json: cannot read {}: {not_found}",
                missing.join("settings.json").display()
            ),
            format!(
                "validate-settings could not be started: {} is not a directory",
                missing.display()
            ),
        ]
    );
}
