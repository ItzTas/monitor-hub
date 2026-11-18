use std::path::{Path, PathBuf};
use std::process::{Command, Output, Stdio};
use std::sync::mpsc;
use std::thread;
use std::time::Duration;

use monitor_hub_plugin::settings::VALIDATE_SETTINGS_ARG;

#[path = "support/example.rs"]
mod example;

const EXIT_TIMEOUT: Duration = Duration::from_secs(10);

fn monitor_dir() -> PathBuf {
    Path::new(env!("CARGO_MANIFEST_DIR")).join("examples/fixed-plugin")
}

fn validate(vars: &[(&str, &str)]) -> Output {
    let child = Command::new(example::fixed())
        .arg(VALIDATE_SETTINGS_ARG)
        .env_clear()
        .envs(vars.iter().copied())
        .current_dir(monitor_dir())
        .stdin(Stdio::null())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .spawn()
        .expect("start example plugin");
    let id = child.id();
    let (sender, receiver) = mpsc::channel();
    thread::spawn(move || {
        let _ = sender.send(child.wait_with_output());
    });
    let Ok(output) = receiver.recv_timeout(EXIT_TIMEOUT) else {
        let _ = Command::new("kill")
            .args(["-s", "KILL", "--", &id.to_string()])
            .status();
        panic!("validate-settings did not exit within {EXIT_TIMEOUT:?}");
    };
    output.expect("wait for example plugin")
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
            r#"{"problems":[{"name":"EVENTS","message":"use a whole number of 0 or more"},"#,
            r#"{"name":"LOG_DEMO","message":"use 0 or 1"}]}"#,
            "\n"
        )
    );
    assert_eq!(output.status.code(), Some(1));
}

#[test]
fn each_setting_accepts_only_its_values() {
    let events = "use a whole number of 0 or more";
    let switch = "use 0 or 1";
    let cases = [
        ("EVENTS", "", None),
        ("EVENTS", "0", None),
        ("EVENTS", "42", None),
        ("EVENTS", "-1", Some(events)),
        ("EVENTS", " 5", Some(events)),
        ("EVENTS", "1.5", Some(events)),
        ("LOG_DEMO", "", None),
        ("LOG_DEMO", "0", None),
        ("LOG_DEMO", "1", None),
        ("LOG_DEMO", "yes", Some(switch)),
        ("PANIC_DEMO", "", None),
        ("PANIC_DEMO", "0", None),
        ("PANIC_DEMO", "1", None),
        ("PANIC_DEMO", "2", Some(switch)),
    ];

    for (name, value, want) in cases {
        let (stdout, code) = match want {
            None => ("{\"problems\":[]}\n".to_owned(), 0),
            Some(message) => (
                format!("{{\"problems\":[{{\"name\":\"{name}\",\"message\":\"{message}\"}}]}}\n"),
                1,
            ),
        };

        let output = validate(&[(name, value)]);

        assert_eq!(
            String::from_utf8_lossy(&output.stdout),
            stdout,
            "{name}={value:?}"
        );
        assert_eq!(output.status.code(), Some(code), "{name}={value:?}");
    }
}
