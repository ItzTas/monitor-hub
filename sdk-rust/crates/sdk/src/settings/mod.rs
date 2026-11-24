use std::io::{self, Write};
use std::process::ExitCode;
use std::time::Duration;

use serde::de::{Error as _, Unexpected};
use serde::{Deserialize, Deserializer, Serialize};
use serde_json::Value;

mod duration;
mod path;
mod rules;

pub use duration::parse_duration;
pub use rules::FieldProblem;

pub const SETTINGS_FILE: &str = "settings.json";
pub const VALIDATE_SETTINGS_ARG: &str = "validate-settings";
pub const SETTINGS_VERSION: u64 = 2;
pub const MAX_DEPTH: usize = 4;
pub const VALIDATE_SETTINGS_LIMIT: Duration = Duration::from_secs(5);
pub const ANSWER_LIMIT: usize = 65536;

const BYTE_ORDER_MARK: &[u8] = b"\xef\xbb\xbf";

#[derive(Debug, Clone, PartialEq, Eq, Deserialize)]
#[serde(deny_unknown_fields)]
#[non_exhaustive]
pub struct Settings {
    pub settings_version: u64,
    pub fields: Vec<Field>,
}

#[derive(Debug, Clone, PartialEq, Eq, Deserialize)]
#[serde(deny_unknown_fields)]
#[non_exhaustive]
pub struct Field {
    pub name: String,
    pub label: String,
    pub help: Option<String>,
    #[serde(rename = "type")]
    pub kind: FieldType,
    #[serde(default)]
    pub required: bool,
    #[serde(default)]
    pub secret: bool,
    pub default: Option<Value>,
    pub options: Option<Vec<String>>,
    pub group: Option<String>,
    #[serde(default, deserialize_with = "flag")]
    pub editable: Option<bool>,
    pub reason: Option<String>,
    pub env: Option<String>,
    pub fields: Option<Vec<Field>>,
    pub items: Option<Box<Item>>,
}

impl Field {
    pub fn is_editable(&self) -> bool {
        self.editable != Some(false)
    }
}

// Absent is None, but null is an error, as in required and secret.
fn flag<'de, D: Deserializer<'de>>(deserializer: D) -> Result<Option<bool>, D::Error> {
    bool::deserialize(deserializer).map(Some)
}

#[derive(Debug, Clone, PartialEq, Eq, Deserialize)]
#[serde(deny_unknown_fields)]
#[non_exhaustive]
pub struct Item {
    #[serde(rename = "type")]
    pub kind: FieldType,
    pub options: Option<Vec<String>>,
    pub fields: Option<Vec<Field>>,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Deserialize)]
#[serde(rename_all = "lowercase")]
#[non_exhaustive]
pub enum FieldType {
    String,
    Integer,
    Number,
    Boolean,
    Duration,
    Choice,
    Object,
    List,
}

#[derive(Debug, thiserror::Error)]
#[non_exhaustive]
pub enum SettingsError {
    #[error("settings.json starts with a UTF-8 byte order mark")]
    ByteOrderMark,
    #[error("settings.json is not valid")]
    Format(#[source] serde_json::Error),
    #[error("settings.json: settings_version is {0}, want {want}", want = SETTINGS_VERSION)]
    UnsupportedVersion(u64),
    #[error("settings.json: {path}: {problem}")]
    #[non_exhaustive]
    Field { path: String, problem: FieldProblem },
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[non_exhaustive]
pub struct Problem {
    #[serde(default, deserialize_with = "null_as_empty")]
    pub path: String,
    pub message: String,
}

impl Problem {
    pub fn new(path: impl Into<String>, message: impl Into<String>) -> Problem {
        Problem {
            path: path.into(),
            message: message.into(),
        }
    }
}

// An answer may leave path out or write null; both name the whole document.
fn null_as_empty<'de, D: Deserializer<'de>>(deserializer: D) -> Result<String, D::Error> {
    Option::<String>::deserialize(deserializer).map(Option::unwrap_or_default)
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[non_exhaustive]
pub struct Answer {
    pub problems: Vec<Problem>,
}

#[derive(Serialize)]
struct AnswerRef<'a> {
    problems: &'a [Problem],
}

pub fn is_reserved(name: &str) -> bool {
    const NAMES: [&str; 3] = ["PATH", "TZ", "TMPDIR"];
    const PREFIXES: [&str; 3] = ["MONITOR_HUB_", "PLUGIN_", "LD_"];
    NAMES.contains(&name) || PREFIXES.iter().any(|prefix| name.starts_with(prefix))
}

pub fn parse(json: &[u8]) -> Result<Settings, SettingsError> {
    if json.starts_with(BYTE_ORDER_MARK) {
        return Err(SettingsError::ByteOrderMark);
    }
    let settings: Settings = serde_json::from_slice(json).map_err(SettingsError::Format)?;
    check_shape(json).map_err(SettingsError::Format)?;
    if settings.settings_version != SETTINGS_VERSION {
        return Err(SettingsError::UnsupportedVersion(settings.settings_version));
    }
    rules::check(&settings.fields)
        .map_err(|(path, problem)| SettingsError::Field { path, problem })?;
    Ok(settings)
}

// The derived Deserialize also takes a JSON list in place of a struct, which settings.json
// does not allow for the document, a field or an item.
fn check_shape(json: &[u8]) -> Result<(), serde_json::Error> {
    let value: Value = serde_json::from_slice(json)?;
    objects_only(&value, "struct Settings")
}

fn objects_only(value: &Value, expected: &str) -> Result<(), serde_json::Error> {
    if value.is_array() {
        return Err(serde_json::Error::invalid_type(Unexpected::Seq, &expected));
    }
    let fields = value.get("fields").and_then(Value::as_array);
    for field in fields.into_iter().flatten() {
        objects_only(field, "struct Field")?;
    }
    match value.get("items") {
        Some(items) => objects_only(items, "struct Item"),
        None => Ok(()),
    }
}

pub fn write_answer(out: &mut impl Write, problems: &[Problem]) -> u8 {
    if emit(out, problems).is_err() {
        return 2;
    }
    u8::from(!problems.is_empty())
}

fn emit(out: &mut impl Write, problems: &[Problem]) -> io::Result<()> {
    serde_json::to_writer(&mut *out, &AnswerRef { problems })?;
    out.write_all(b"\n")?;
    out.flush()
}

pub fn answer(problems: &[Problem]) -> ExitCode {
    ExitCode::from(write_answer(&mut io::stdout().lock(), problems))
}
