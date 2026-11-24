use std::path::{Path, PathBuf};
use std::process::{Command, Output, Stdio};
use std::sync::mpsc;
use std::thread;
use std::time::Duration;

use monitor_hub_sdk::settings::VALIDATE_SETTINGS_ARG;
use monitor_hub_sdk_testkit::settings::command_with_document;

#[path = "support/example.rs"]
mod example;

const EXIT_TIMEOUT: Duration = Duration::from_secs(10);

fn monitor_dir() -> PathBuf {
    Path::new(env!("CARGO_MANIFEST_DIR")).join("examples/fixed-monitor")
}

fn validate(vars: &[(&str, &str)]) -> Output {
    let mut command = Command::new(example::fixed());
    command
        .arg(VALIDATE_SETTINGS_ARG)
        .env_clear()
        .envs(vars.iter().copied());
    run(command)
}

fn run(mut command: Command) -> Output {
    let child = command
        .current_dir(monitor_dir())
        .stdin(Stdio::null())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .spawn()
        .expect("start example monitor");
    let id = child.id();
    let (sender, receiver) = mpsc::channel();
    thread::spawn(move || {
        let _ = sender.send(child.wait_with_output());
    });
    if let Ok(output) = receiver.recv_timeout(EXIT_TIMEOUT) {
        return output.expect("wait for example monitor");
    }
    let _ = Command::new("kill")
        .args(["-s", "KILL", "--", &id.to_string()])
        .status();
    let _ = receiver.recv();
    panic!("validate-settings did not exit within {EXIT_TIMEOUT:?}");
}

#[test]
fn an_empty_environment_has_no_problems() {
    let output = validate(&[]);

    assert_eq!(
        String::from_utf8_lossy(&output.stdout),
        "{\"problems\":[]}\n"
    );
    assert_eq!(output.status.code(), Some(0));
}

#[test]
fn every_problem_is_listed_at_once() {
    let output = validate(&[("EVENTS", "x"), ("LOG_DEMO", "2")]);

    assert_eq!(
        String::from_utf8_lossy(&output.stdout),
        concat!(
            r#"{"problems":[{"path":"events","message":"EVENTS is not an integer (\"x\")"}]}"#,
            "\n"
        )
    );
    assert_eq!(output.status.code(), Some(1));

    let output = validate(&[("EVENTS", "-1"), ("LOG_DEMO", "2")]);

    assert_eq!(
        String::from_utf8_lossy(&output.stdout),
        concat!(
            r#"{"problems":[{"path":"events","message":"use a whole number of 0 or more"},"#,
            r#"{"path":"log_demo","message":"use 0 or 1"}]}"#,
            "\n"
        )
    );
    assert_eq!(output.status.code(), Some(1));
}

#[test]
fn each_setting_accepts_only_its_values() {
    let events = "use a whole number of 0 or more";
    let padded = r#"EVENTS is not an integer (\" 5\")"#;
    let fraction = r#"EVENTS is not an integer (\"1.5\")"#;
    let switch = "use 0 or 1";
    let cases = [
        ("EVENTS", "events", "", None),
        ("EVENTS", "events", "0", None),
        ("EVENTS", "events", "42", None),
        ("EVENTS", "events", "-1", Some(events)),
        ("EVENTS", "events", " 5", Some(padded)),
        ("EVENTS", "events", "1.5", Some(fraction)),
        ("LOG_DEMO", "log_demo", "", None),
        ("LOG_DEMO", "log_demo", "0", None),
        ("LOG_DEMO", "log_demo", "1", None),
        ("LOG_DEMO", "log_demo", "yes", Some(switch)),
        ("PANIC_DEMO", "panic_demo", "", None),
        ("PANIC_DEMO", "panic_demo", "0", None),
        ("PANIC_DEMO", "panic_demo", "1", None),
        ("PANIC_DEMO", "panic_demo", "2", Some(switch)),
    ];

    for (variable, path, value, want) in cases {
        let (stdout, code) = match want {
            None => ("{\"problems\":[]}\n".to_owned(), 0),
            Some(message) => (
                format!("{{\"problems\":[{{\"path\":\"{path}\",\"message\":\"{message}\"}}]}}\n"),
                1,
            ),
        };

        let output = validate(&[(variable, value)]);

        assert_eq!(
            String::from_utf8_lossy(&output.stdout),
            stdout,
            "{variable}={value:?}"
        );
        assert_eq!(output.status.code(), Some(code), "{variable}={value:?}");
    }
}

#[test]
fn a_document_on_the_descriptor_wins_over_the_environment() {
    let mut document = command_with_document(
        example::fixed(),
        &[VALIDATE_SETTINGS_ARG],
        br#"{"events": -1}"#,
    )
    .expect("write the document");
    document.command.env("EVENTS", "5");

    let output = run(document.command);

    assert_eq!(
        String::from_utf8_lossy(&output.stdout),
        concat!(
            r#"{"problems":[{"path":"events","message":"use a whole number of 0 or more"}]}"#,
            "\n"
        )
    );
    assert_eq!(output.status.code(), Some(1));
}

#[test]
fn a_value_the_configuration_type_refuses_gets_a_fixed_problem() {
    let document = command_with_document(
        example::fixed(),
        &[VALIDATE_SETTINGS_ARG],
        br#"{"events": "70123"}"#,
    )
    .expect("write the document");

    let output = run(document.command);

    assert_eq!(
        String::from_utf8_lossy(&output.stdout),
        concat!(
            r#"{"problems":[{"path":"","message":"the configuration does not fit the monitor's configuration type"}]}"#,
            "\n"
        )
    );
    assert!(!String::from_utf8_lossy(&output.stderr).contains("70123"));
    assert_eq!(output.status.code(), Some(1));
}

#[test]
fn starting_with_a_value_the_configuration_type_refuses_logs_a_fixed_problem() {
    let document = command_with_document(example::fixed(), &[], br#"{"events": "70123"}"#)
        .expect("write the document");

    let output = run(document.command);

    let stderr = String::from_utf8_lossy(&output.stderr);
    assert!(
        stderr.contains("the configuration does not fit the monitor's configuration type"),
        "{stderr}"
    );
    assert!(!stderr.contains("70123"), "{stderr}");
    assert!(!String::from_utf8_lossy(&output.stdout).contains("70123"));
    assert_eq!(output.status.code(), Some(1));
}
