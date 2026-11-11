use std::env;
use std::ffi::OsString;
use std::fs;
use std::path::Path;
use std::process::Command;

#[test]
fn generated_code_is_current() {
    if !protoc_available() {
        assert!(env::var_os("CI").is_none(), "protoc is required in CI");
        eprintln!("skipped: protoc is not available");
        return;
    }
    let out = tempfile::tempdir().expect("create temp dir");
    let status = Command::new(env!("CARGO"))
        .args(["run", "--quiet", "-p", "monitor-hub-codegen", "--"])
        .arg(out.path())
        .status()
        .expect("run codegen");
    assert!(status.success(), "codegen failed: {status}");

    let committed = files(&Path::new(env!("CARGO_MANIFEST_DIR")).join("src/generated"));
    let fresh = files(out.path());
    assert_eq!(
        names(&committed),
        names(&fresh),
        "src/generated is stale: run make generate-rust"
    );
    for ((name, want), (_, got)) in fresh.iter().zip(&committed) {
        assert!(
            want == got,
            "src/generated/{} is stale: run make generate-rust",
            name.to_string_lossy()
        );
    }
}

fn protoc_available() -> bool {
    let protoc = env::var_os("PROTOC").unwrap_or_else(|| "protoc".into());
    Command::new(protoc)
        .arg("--version")
        .output()
        .is_ok_and(|out| out.status.success())
}

fn files(dir: &Path) -> Vec<(OsString, String)> {
    let mut files: Vec<_> = fs::read_dir(dir)
        .expect("read generated dir")
        .map(|entry| {
            let path = entry.expect("read dir entry").path();
            let name = path.file_name().expect("file name").to_owned();
            (
                name,
                fs::read_to_string(&path).expect("read generated file"),
            )
        })
        .collect();
    files.sort();
    files
}

fn names(files: &[(OsString, String)]) -> Vec<&OsString> {
    files.iter().map(|(name, _)| name).collect()
}
