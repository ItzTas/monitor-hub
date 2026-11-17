use std::fs;
use std::path::Path;
use std::process::Command;

pub fn kill_where(matches: impl Fn(&Path) -> bool) {
    let Ok(entries) = fs::read_dir("/proc") else {
        return;
    };
    for entry in entries.flatten() {
        let name = entry.file_name();
        let Some(pid) = name
            .to_str()
            .filter(|name| name.bytes().all(|b| b.is_ascii_digit()))
        else {
            continue;
        };
        if matches(&entry.path()) {
            let _ = Command::new("kill")
                .args(["-s", "KILL", "--", pid])
                .status();
        }
    }
}

pub fn runs(process: &Path, argv: &[&str]) -> bool {
    let want: Vec<u8> = argv.iter().flat_map(|arg| arg.bytes().chain([0])).collect();
    fs::read(process.join("cmdline")).is_ok_and(|cmdline| cmdline == want)
}
