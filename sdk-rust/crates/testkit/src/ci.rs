use std::env;

pub(crate) fn active() -> bool {
    env::var_os("CI").is_some_and(|value| !value.is_empty())
}
