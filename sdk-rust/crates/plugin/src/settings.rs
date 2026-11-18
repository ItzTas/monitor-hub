use std::collections::HashSet;
use std::io::{self, Write};
use std::process::ExitCode;
use std::time::Duration;

use serde::de::{Error as _, Unexpected};
use serde::{Deserialize, Serialize};
use serde_json::Value;

pub const SETTINGS_FILE: &str = "settings.json";
pub const VALIDATE_SETTINGS_ARG: &str = "validate-settings";
pub const SETTINGS_VERSION: u64 = 1;
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
    pub secret: bool,
    #[serde(default)]
    pub required: bool,
    pub default: Option<String>,
    pub options: Option<Vec<String>>,
    pub group: Option<String>,
    #[serde(default = "editable_by_default")]
    pub editable: bool,
    pub reason: Option<String>,
}

fn editable_by_default() -> bool {
    true
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Deserialize)]
#[serde(rename_all = "lowercase")]
#[non_exhaustive]
pub enum FieldType {
    String,
    List,
    Integer,
    Number,
    Duration,
    Choice,
}

#[derive(Debug, thiserror::Error)]
#[non_exhaustive]
pub enum SettingsError {
    #[error("settings.json starts with a UTF-8 byte order mark")]
    ByteOrderMark,
    #[error("settings.json is not valid")]
    Format(#[source] serde_json::Error),
    #[error("settings.json: settings_version is {0}, want 1")]
    UnsupportedVersion(u64),
    #[error("settings.json: fields[{index}]: {problem}")]
    Field { index: usize, problem: FieldProblem },
}

#[derive(Debug, Clone, PartialEq, Eq, thiserror::Error)]
#[non_exhaustive]
pub enum FieldProblem {
    #[error("name {0:?} does not match ^[A-Z][A-Z0-9_]*$")]
    InvalidName(String),
    #[error("name {0:?} is reserved by the hub")]
    ReservedName(String),
    #[error("name {0:?} is declared more than once")]
    DuplicateName(String),
    #[error("label is empty")]
    EmptyLabel,
    #[error("a choice field needs at least one option")]
    MissingOptions,
    #[error("options are only for choice fields")]
    UnexpectedOptions,
    #[error("an option is empty")]
    EmptyOption,
    #[error("option {0:?} is listed more than once")]
    DuplicateOption(String),
    #[error("default {0:?} is not one of the options")]
    DefaultNotAnOption(String),
    #[error("a secret field cannot have a default")]
    SecretDefault,
    #[error("editable: false needs a non-empty reason")]
    MissingReason,
    #[error("reason is only for fields with editable: false")]
    UnexpectedReason,
    #[error("group is empty")]
    EmptyGroup,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[non_exhaustive]
pub struct Problem {
    #[serde(default)]
    pub name: String,
    pub message: String,
}

impl Problem {
    pub fn new(name: impl Into<String>, message: impl Into<String>) -> Problem {
        Problem {
            name: name.into(),
            message: message.into(),
        }
    }
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
    let mut seen = HashSet::new();
    for (index, field) in settings.fields.iter().enumerate() {
        if let Some(problem) = check_field(field, &mut seen) {
            return Err(SettingsError::Field { index, problem });
        }
    }
    Ok(settings)
}

fn check_shape(json: &[u8]) -> Result<(), serde_json::Error> {
    let value: Value = serde_json::from_slice(json)?;
    if value.is_array() {
        return Err(sequence("struct Settings"));
    }
    let fields = value.get("fields").and_then(Value::as_array);
    if fields.is_some_and(|fields| fields.iter().any(Value::is_array)) {
        return Err(sequence("struct Field"));
    }
    Ok(())
}

fn sequence(expected: &str) -> serde_json::Error {
    serde_json::Error::invalid_type(Unexpected::Seq, &expected)
}

fn check_field<'a>(field: &'a Field, seen: &mut HashSet<&'a str>) -> Option<FieldProblem> {
    check_name(field, seen)
        .or_else(|| check_label(field))
        .or_else(|| check_options(field))
        .or_else(|| check_secret(field))
        .or_else(|| check_reason(field))
        .or_else(|| check_group(field))
}

fn check_name<'a>(field: &'a Field, seen: &mut HashSet<&'a str>) -> Option<FieldProblem> {
    let name = field.name.as_str();
    if !is_valid_name(name) {
        return Some(FieldProblem::InvalidName(name.to_owned()));
    }
    if is_reserved(name) {
        return Some(FieldProblem::ReservedName(name.to_owned()));
    }
    if !seen.insert(name) {
        return Some(FieldProblem::DuplicateName(name.to_owned()));
    }
    None
}

fn is_valid_name(name: &str) -> bool {
    let mut bytes = name.bytes();
    bytes.next().is_some_and(|first| first.is_ascii_uppercase())
        && bytes.all(|b| b.is_ascii_uppercase() || b.is_ascii_digit() || b == b'_')
}

fn check_label(field: &Field) -> Option<FieldProblem> {
    field
        .label
        .trim()
        .is_empty()
        .then_some(FieldProblem::EmptyLabel)
}

fn check_options(field: &Field) -> Option<FieldProblem> {
    if field.kind != FieldType::Choice {
        return field
            .options
            .is_some()
            .then_some(FieldProblem::UnexpectedOptions);
    }
    let options = match field.options.as_deref() {
        Some(options) if !options.is_empty() => options,
        _ => return Some(FieldProblem::MissingOptions),
    };
    let mut seen = HashSet::new();
    for option in options {
        if option.trim().is_empty() {
            return Some(FieldProblem::EmptyOption);
        }
        if !seen.insert(option.as_str()) {
            return Some(FieldProblem::DuplicateOption(option.clone()));
        }
    }
    let default = field.default.as_ref()?;
    (!seen.contains(default.as_str())).then(|| FieldProblem::DefaultNotAnOption(default.clone()))
}

fn check_secret(field: &Field) -> Option<FieldProblem> {
    (field.secret && field.default.is_some()).then_some(FieldProblem::SecretDefault)
}

fn check_reason(field: &Field) -> Option<FieldProblem> {
    if field.editable {
        return field
            .reason
            .is_some()
            .then_some(FieldProblem::UnexpectedReason);
    }
    let has_reason = field
        .reason
        .as_deref()
        .is_some_and(|r| !r.trim().is_empty());
    (!has_reason).then_some(FieldProblem::MissingReason)
}

fn check_group(field: &Field) -> Option<FieldProblem> {
    let blank = field.group.as_deref().is_some_and(|g| g.trim().is_empty());
    blank.then_some(FieldProblem::EmptyGroup)
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
