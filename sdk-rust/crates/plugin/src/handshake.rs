use std::env;
use std::fs;
use std::io::{self, ErrorKind, Write};
use std::path::{self, PathBuf};
use std::process;
use std::time::{SystemTime, UNIX_EPOCH};

use tokio::net::UnixListener;
use tracing::Level;

use crate::logging;

pub const MAGIC_COOKIE_KEY: &str = "MONITOR_HUB_PLUGIN";
pub const MAGIC_COOKIE_VALUE: &str = "7c0e5a52-monitor-hub";
pub const DATA_DIR_ENV: &str = "MONITOR_HUB_DATA_DIR";
pub const NOT_LAUNCHED_BY_HUB: &str =
    "this binary is a monitor-hub plugin and must be started by the hub";

const SOCKET_DIR_ENVS: [&str; 2] = ["PLUGIN_UNIX_SOCKET_DIR", "TMPDIR"];
const DEFAULT_SOCKET_DIR: &str = "/tmp";
const MAX_SOCKET_PATH: usize = 100;

pub fn launched_by_hub() -> bool {
    env::var_os(MAGIC_COOKIE_KEY).is_some_and(|value| value == MAGIC_COOKIE_VALUE)
}

pub(crate) struct Socket {
    path: String,
}

impl Socket {
    pub(crate) fn bind() -> io::Result<(Self, UnixListener)> {
        let path = socket_path()?;
        let listener = UnixListener::bind(&path)
            .map_err(|err| io::Error::new(err.kind(), format!("listen on {path}: {err}")))?;
        Ok((Self { path }, listener))
    }

    pub(crate) fn announce(&self) -> io::Result<()> {
        let mut stdout = io::stdout().lock();
        writeln!(stdout, "1|1|unix|{}|grpc", self.path)?;
        stdout.flush()
    }
}

impl Drop for Socket {
    fn drop(&mut self) {
        if let Err(err) = fs::remove_file(&self.path)
            && err.kind() != ErrorKind::NotFound
        {
            logging::write_event(
                Level::WARN,
                module_path!(),
                "cannot remove the plugin socket",
                &[
                    ("socket", self.path.as_str().into()),
                    ("error", err.to_string().into()),
                ],
            );
        }
    }
}

fn socket_path() -> io::Result<String> {
    let dir = SOCKET_DIR_ENVS
        .into_iter()
        .filter_map(env::var_os)
        .find(|dir| !dir.is_empty())
        .map_or_else(|| PathBuf::from(DEFAULT_SOCKET_DIR), PathBuf::from);
    let path = path::absolute(dir.join(socket_name()))?;
    let Some(text) = path.to_str() else {
        return Err(invalid(format!(
            "socket path {} is not valid UTF-8",
            path.display()
        )));
    };
    if text.contains('|') {
        return Err(invalid(format!("socket path {text} contains '|'")));
    }
    if text.len() > MAX_SOCKET_PATH {
        return Err(invalid(format!(
            "socket path {text} is too long: {} bytes, the limit is {MAX_SOCKET_PATH}",
            text.len()
        )));
    }
    Ok(text.to_owned())
}

fn socket_name() -> String {
    let nanos = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map_or(0, |since| since.subsec_nanos());
    format!("plugin-{}-{nanos}", process::id())
}

fn invalid(message: String) -> io::Error {
    io::Error::new(ErrorKind::InvalidInput, message)
}
