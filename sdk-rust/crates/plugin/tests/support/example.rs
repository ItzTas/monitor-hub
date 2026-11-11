use std::path::{Path, PathBuf};
use std::process::Command;
use std::sync::OnceLock;

use serde_json::Value;

pub fn fixed() -> &'static Path {
    static PATH: OnceLock<PathBuf> = OnceLock::new();
    PATH.get_or_init(|| build("fixed"))
}

fn build(name: &str) -> PathBuf {
    let output = Command::new(env!("CARGO"))
        .args([
            "build",
            "--quiet",
            "--message-format=json-render-diagnostics",
            "-p",
            "monitor-hub-plugin",
            "--example",
            name,
        ])
        .output()
        .expect("run cargo build");
    assert!(
        output.status.success(),
        "cargo build --example {name} failed: {}\n{}",
        output.status,
        String::from_utf8_lossy(&output.stderr)
    );
    String::from_utf8_lossy(&output.stdout)
        .lines()
        .filter_map(|line| serde_json::from_str::<Value>(line).ok())
        .find(|message| is_example(message, name))
        .and_then(|message| message["executable"].as_str().map(PathBuf::from))
        .unwrap_or_else(|| panic!("cargo build did not report an executable for example {name}"))
}

fn is_example(message: &Value, name: &str) -> bool {
    message["reason"] == "compiler-artifact"
        && message["target"]["name"] == name
        && message["target"]["kind"]
            .as_array()
            .is_some_and(|kinds| kinds.iter().any(|kind| kind == "example"))
}
