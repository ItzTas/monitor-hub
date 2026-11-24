use std::error::Error as _;
use std::ffi::{OsStr, OsString};
use std::fs::File;
use std::io::{self, Read};
use std::iter;
use std::os::fd::{FromRawFd, OwnedFd, RawFd};
use std::sync::atomic::{AtomicBool, Ordering};

use serde_json::{Map, Value};

use crate::settings::{self, Field, FieldType, Problem, Settings, SettingsError};

pub const CONFIG_FD_ENV: &str = "MONITOR_HUB_CONFIG_FD";
pub const DOCUMENT_LIMIT: usize = 65536;

pub type Document = Map<String, Value>;

static DESCRIPTOR_TAKEN: AtomicBool = AtomicBool::new(false);

#[derive(Debug, thiserror::Error)]
#[non_exhaustive]
pub enum LoadError {
    #[error(transparent)]
    Settings(#[from] SettingsError),
    #[error("{CONFIG_FD_ENV} is {0:?}; it must be a descriptor number of 3 or more")]
    Descriptor(String),
    #[error(
        "{CONFIG_FD_ENV} was already read; the configuration document is read once per process"
    )]
    AlreadyTaken,
    #[error("cannot read the configuration document")]
    Read(#[source] io::Error),
    #[error("the configuration document is larger than {DOCUMENT_LIMIT} bytes")]
    TooLarge,
    #[error("the configuration document is not a JSON object")]
    NotAnObject,
    #[error("{}", joined(.0))]
    Environment(Vec<Problem>),
}

fn joined(problems: &[Problem]) -> String {
    problems
        .iter()
        .map(|problem| format!("{}: {}", problem.path, problem.message))
        .collect::<Vec<_>>()
        .join("; ")
}

impl LoadError {
    pub fn problems(&self) -> Vec<Problem> {
        if let LoadError::Environment(problems) = self {
            return problems.clone();
        }
        let message = iter::successors(self.source(), |&cause| cause.source())
            .fold(self.to_string(), |text, cause| format!("{text}: {cause}"));
        vec![Problem::new("", message)]
    }
}

/// Reads the document from the descriptor named by `MONITOR_HUB_CONFIG_FD`, once per process,
/// or else builds it from the environment.
pub fn load(settings_json: &str) -> Result<Document, LoadError> {
    let settings = settings::parse(settings_json.as_bytes())?;
    let Some(text) = std::env::var_os(CONFIG_FD_ENV).filter(|text| !text.is_empty()) else {
        return from_lookup(&settings, |name| std::env::var_os(name));
    };
    let number = descriptor_number(&text)?;
    if DESCRIPTOR_TAKEN.swap(true, Ordering::SeqCst) {
        return Err(LoadError::AlreadyTaken);
    }
    // SAFETY: the hub hands the monitor this descriptor and names it in CONFIG_FD_ENV, and
    // DESCRIPTOR_TAKEN lets it be taken once per process; nothing else owns it, so it is ours to
    // read and close.
    let fd = unsafe { OwnedFd::from_raw_fd(number) };
    from_descriptor(&settings, fd)
}

pub fn descriptor_number(text: &OsStr) -> Result<RawFd, LoadError> {
    // Digits only, since parse also takes a sign.
    text.to_str()
        .filter(|text| text.bytes().all(|b| b.is_ascii_digit()))
        .and_then(|digits| digits.parse::<RawFd>().ok())
        .filter(|&number| number >= 3)
        .ok_or_else(|| LoadError::Descriptor(text.to_string_lossy().into_owned()))
}

pub fn from_descriptor(settings: &Settings, fd: OwnedFd) -> Result<Document, LoadError> {
    let mut bytes = Vec::new();
    // Dropping the reader closes the descriptor, so a program the monitor runs does not get it.
    File::from(fd)
        .take(DOCUMENT_LIMIT as u64 + 1)
        .read_to_end(&mut bytes)
        .map_err(LoadError::Read)?;
    if bytes.len() > DOCUMENT_LIMIT {
        return Err(LoadError::TooLarge);
    }
    let Ok(Value::Object(document)) = serde_json::from_slice(&bytes) else {
        return Err(LoadError::NotAnObject);
    };
    Ok(with_defaults(settings, document))
}

pub fn from_lookup(
    settings: &Settings,
    lookup: impl Fn(&str) -> Option<OsString>,
) -> Result<Document, LoadError> {
    let mut document = Document::new();
    let mut problems = Vec::new();
    for field in &settings.fields {
        let Some(name) = field.env.as_deref() else {
            continue;
        };
        let Some(text) = lookup(name).filter(|text| !text.is_empty()) else {
            continue;
        };
        let variable = Variable { name, field };
        match variable.convert(&text) {
            Ok(value) => {
                document.insert(field.name.clone(), value);
            }
            Err(found) => problems.extend(found),
        }
    }
    if !problems.is_empty() {
        return Err(LoadError::Environment(problems));
    }
    Ok(with_defaults(settings, document))
}

// The environment variable of a top-level field.
struct Variable<'a> {
    name: &'a str,
    field: &'a Field,
}

impl Variable<'_> {
    fn convert(&self, text: &OsStr) -> Result<Value, Vec<Problem>> {
        let field = self.field;
        if field.secret && text.to_str().is_none() {
            let message = format!("{} is not valid UTF-8", self.name);
            return Err(vec![Problem::new(&field.name, message)]);
        }
        let text = text.to_string_lossy();
        let Some(item) = field.items.as_deref() else {
            return simple(field.kind, &text)
                .map_err(|what| vec![self.problem(field.name.clone(), what, &text)]);
        };
        self.list(item.kind, &text)
    }

    // The index of a path counts only the pieces kept, as in the document.
    fn list(&self, kind: FieldType, text: &str) -> Result<Value, Vec<Problem>> {
        let pieces = text
            .split(',')
            .map(str::trim)
            .filter(|piece| !piece.is_empty());
        let mut values = Vec::new();
        let mut problems = Vec::new();
        for (index, piece) in pieces.enumerate() {
            match simple(kind, piece) {
                Ok(value) => values.push(value),
                Err(what) => {
                    let path = format!("{}[{index}]", self.field.name);
                    problems.push(self.problem(path, what, piece));
                }
            }
        }
        if !problems.is_empty() {
            return Err(problems);
        }
        Ok(Value::Array(values))
    }

    // The text of a secret stays out of the message.
    fn problem(&self, path: String, what: &str, text: &str) -> Problem {
        if self.field.secret {
            return Problem::new(path, format!("{} {what}", self.name));
        }
        let quoted = Value::from(text);
        Problem::new(path, format!("{} {what} ({quoted})", self.name))
    }
}

// The value of text for a field or item of type kind, or what text is not.
fn simple(kind: FieldType, text: &str) -> Result<Value, &'static str> {
    match kind {
        FieldType::Integer => text
            .parse::<i64>()
            .map(Value::from)
            .map_err(|_| "is not an integer"),
        // The grammar of f64 is ^[+-]?([0-9]+(\.[0-9]*)?|\.[0-9]+)([eE][+-]?[0-9]+)?$ plus inf,
        // infinity and NaN, which are not finite.
        FieldType::Number => text
            .parse::<f64>()
            .ok()
            .filter(|number| number.is_finite())
            .map(Value::from)
            .ok_or("is not a number"),
        FieldType::Boolean => text
            .parse::<bool>()
            .map(Value::from)
            .map_err(|_| "is not true or false"),
        _ => Ok(Value::from(text)),
    }
}

pub fn with_defaults(settings: &Settings, mut document: Document) -> Document {
    fill(&settings.fields, &mut document);
    document
}

fn fill(fields: &[Field], document: &mut Document) {
    for field in fields {
        let Some(value) = document.get_mut(&field.name) else {
            if let Some(value) = missing(field) {
                document.insert(field.name.clone(), value);
            }
            continue;
        };
        match (&field.default, value) {
            (Some(default), value) if *value == "" => *value = default.clone(),
            (_, Value::Object(object)) => fill(field.fields.as_deref().unwrap_or_default(), object),
            (_, Value::Array(items)) => {
                let children = field.items.as_ref().and_then(|item| item.fields.as_deref());
                for item in items.iter_mut().filter_map(Value::as_object_mut) {
                    fill(children.unwrap_or_default(), item);
                }
            }
            _ => {}
        }
    }
}

// The value of a field the document leaves out: its default, or for an object, one built from
// the defaults of its fields when that is not empty.
fn missing(field: &Field) -> Option<Value> {
    if field.default.is_some() {
        return field.default.clone();
    }
    let mut object = Document::new();
    fill(field.fields.as_deref()?, &mut object);
    (!object.is_empty()).then_some(Value::Object(object))
}
