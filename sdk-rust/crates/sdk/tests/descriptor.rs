// A test binary of its own: a test running alongside could open a file that reuses the closed number.

use std::fs;
use std::io::{ErrorKind, Seek, Write};
use std::os::fd::{AsRawFd, OwnedFd};

use monitor_hub_sdk::config::from_descriptor;
use monitor_hub_sdk::settings::parse;

#[test]
fn the_descriptor_is_closed_after_reading() {
    let settings = parse(br#"{"settings_version": 2, "fields": []}"#).expect("valid settings");
    let mut file = tempfile::tempfile().expect("create a temporary file");
    file.write_all(b"{}").expect("write the document");
    file.rewind().expect("rewind the file");
    let number = file.as_raw_fd();

    from_descriptor(&settings, OwnedFd::from(file)).expect("read the document");

    let err = fs::read_link(format!("/proc/self/fd/{number}")).unwrap_err();
    assert_eq!(err.kind(), ErrorKind::NotFound, "{err}");
}
