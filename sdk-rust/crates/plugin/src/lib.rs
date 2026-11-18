#[rustfmt::skip]
#[allow(clippy::all)]
#[path = "generated/monitorhub.plugin.v1.rs"]
pub mod proto;

#[rustfmt::skip]
#[allow(clippy::all)]
#[path = "generated/plugin.rs"]
mod controller;

mod handshake;
pub mod logging;
mod serve;
pub mod settings;
mod watch;

pub use handshake::{
    DATA_DIR_ENV, MAGIC_COOKIE_KEY, MAGIC_COOKIE_VALUE, NOT_LAUNCHED_BY_HUB, launched_by_hub,
};
pub use serve::{ServeError, serve, serve_with_notifier};
pub use watch::{Notifier, Signals};
