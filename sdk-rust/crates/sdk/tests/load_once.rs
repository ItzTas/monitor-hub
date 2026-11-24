// A test binary of its own: it sets MONITOR_HUB_CONFIG_FD for the whole process, and a new file
// must get the number the first load closed.

use std::fs::File;
use std::io::{Read, Seek, Write};
use std::os::fd::{AsRawFd, IntoRawFd};

use monitor_hub_sdk::config::{CONFIG_FD_ENV, LoadError, load};
use serde_json::{Value, json};

const SETTINGS: &str =
    r#"{"settings_version": 2, "fields": [{"name": "s", "label": "S", "type": "string"}]}"#;

fn file_with(content: &[u8]) -> File {
    let mut file = tempfile::tempfile().expect("create a temporary file");
    file.write_all(content).expect("write the file");
    file.rewind().expect("rewind the file");
    file
}

#[test]
fn the_descriptor_is_read_once_per_process() {
    let number = file_with(br#"{"s": "x"}"#).into_raw_fd();
    // SAFETY: this binary has a single test, so no other thread reads the environment.
    unsafe { std::env::set_var(CONFIG_FD_ENV, number.to_string()) };

    let document = load(SETTINGS).expect("the first load reads the descriptor");
    assert_eq!(Value::Object(document), json!({"s": "x"}));

    let mut reopened = file_with(b"kept");
    assert_eq!(
        reopened.as_raw_fd(),
        number,
        "the new file got another number"
    );

    let err = load(SETTINGS).unwrap_err();
    assert!(matches!(err, LoadError::AlreadyTaken), "{err:?}");
    assert_eq!(
        err.to_string(),
        "MONITOR_HUB_CONFIG_FD was already read; the configuration document is read once per process"
    );
    let mut content = String::new();
    reopened
        .read_to_string(&mut content)
        .expect("the file at that number is still open");
    assert_eq!(content, "kept");
}
