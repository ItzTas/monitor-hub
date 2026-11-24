use std::error::Error;
use std::fs;
use std::io::{self, Write};
use std::iter;
use std::os::unix::process::ExitStatusExt;
use std::path::{self, Path};
use std::process::{Command, ExitStatus};

use monitor_hub_sdk::config::CONFIG_FD_ENV;
use monitor_hub_sdk::settings::{
    ANSWER_LIMIT, Answer, Field, FieldType, Item, SETTINGS_FILE, Settings, VALIDATE_SETTINGS_ARG,
    VALIDATE_SETTINGS_LIMIT, parse,
};
use serde::de::DeserializeOwned;
use serde_json::{Map, Value};
use tempfile::{NamedTempFile, TempPath};

use crate::process::{Failure, Finished, Keep, execute};

pub fn check(dir: impl AsRef<Path>, program: impl AsRef<Path>) -> Vec<String> {
    let dir = dir.as_ref();
    let settings = read_settings(dir);
    let validator = validator_violation(dir, program.as_ref(), settings.as_ref().ok());
    settings.err().into_iter().chain(validator).collect()
}

fn chain(err: &(dyn Error + 'static)) -> String {
    iter::successors(Some(err), |&err| err.source())
        .map(ToString::to_string)
        .collect::<Vec<_>>()
        .join(": ")
}

fn read_settings(dir: &Path) -> Result<Settings, String> {
    let path = dir.join(SETTINGS_FILE);
    let json = fs::read(&path)
        .map_err(|err| format!("{SETTINGS_FILE}: cannot read {}: {err}", path.display()))?;
    parse(&json).map_err(|err| chain(&err))
}

fn validator_violation(dir: &Path, program: &Path, settings: Option<&Settings>) -> Option<String> {
    let not_started = "validate-settings could not be started";
    let program = match path::absolute(program) {
        Ok(program) => program,
        Err(err) => return Some(format!("{not_started}: {}: {err}", program.display())),
    };
    if !dir.is_dir() {
        return Some(format!(
            "{not_started}: {} is not a directory",
            dir.display()
        ));
    }
    let mut command = Command::new(&program);
    command
        .arg(VALIDATE_SETTINGS_ARG)
        .env_clear()
        .current_dir(dir);
    match execute(&mut command, VALIDATE_SETTINGS_LIMIT, Keep::Nothing) {
        Ok(finished) => answer_violation(&finished, settings).err(),
        Err(Failure::Timeout(_)) => Some(format!(
            "validate-settings did not finish within {}s",
            VALIDATE_SETTINGS_LIMIT.as_secs()
        )),
        Err(Failure::Start(err)) => Some(format!("{not_started}: {}: {err}", program.display())),
    }
}

fn answer_violation(finished: &Finished, settings: Option<&Settings>) -> Result<(), String> {
    let code = exit_code(finished.status)?;
    let stdout = finished.stdout.as_slice();
    if stdout.len() > ANSWER_LIMIT {
        return Err(format!(
            "validate-settings wrote more than {ANSWER_LIMIT} bytes on stdout"
        ));
    }
    if stdout.trim_ascii().is_empty() {
        return Err("validate-settings printed nothing on stdout; it runs with an empty environment and without MONITOR_HUB_MONITOR".to_owned());
    }
    let answer = read_answer(stdout)?;
    if let Some(index) = answer
        .problems
        .iter()
        .position(|problem| problem.message.trim().is_empty())
    {
        return Err(format!(
            "validate-settings answer is invalid: problem {index} has an empty message"
        ));
    }
    match (code, answer.problems.is_empty()) {
        (0, false) => Err("validate-settings exited with code 0 but listed problems".to_owned()),
        (1, true) => Err("validate-settings exited with code 1 but listed no problems".to_owned()),
        _ => undeclared_violation(&answer, settings),
    }
}

fn undeclared_violation(answer: &Answer, settings: Option<&Settings>) -> Result<(), String> {
    let Some(settings) = settings else {
        return Ok(());
    };
    let Some((index, problem)) = answer
        .problems
        .iter()
        .enumerate()
        .find(|(_, problem)| !settings.declares(&problem.path))
    else {
        return Ok(());
    };
    Err(format!(
        "validate-settings answer is invalid: problem {index} names the path {:?}, which {SETTINGS_FILE} does not declare",
        problem.path
    ))
}

fn exit_code(status: ExitStatus) -> Result<i32, String> {
    match (status.code(), status.signal()) {
        (Some(code @ (0 | 1)), _) => Ok(code),
        (Some(code), _) => Err(format!(
            "validate-settings exited with code {code}, want 0 or 1"
        )),
        (None, Some(signal)) => Err(format!("validate-settings was killed by signal {signal}")),
        (None, None) => Err(format!("validate-settings ended with {status}")),
    }
}

fn read_answer(stdout: &[u8]) -> Result<Answer, String> {
    let value: Value = serde_json::from_slice(stdout).map_err(|err| {
        format!("validate-settings did not print a single JSON object on stdout: {err}")
    })?;
    if !value.is_object() {
        return Err(
            "validate-settings did not print a single JSON object on stdout: the answer is not an object"
                .to_owned(),
        );
    }
    let problems = value.get("problems").and_then(Value::as_array);
    if let Some(index) = problems.and_then(|problems| problems.iter().position(Value::is_array)) {
        return Err(format!(
            "validate-settings answer is invalid: problem {index} is not an object"
        ));
    }
    serde_json::from_value(value)
        .map_err(|err| format!("validate-settings answer is invalid: {err}"))
}

/// A document with a value for every declared field, at every level: its default, or a value of
/// its type.
pub fn full_document(settings_json: &str) -> Value {
    let settings = parse(settings_json.as_bytes()).unwrap_or_else(|err| panic!("{}", chain(&err)));
    object_of(&settings.fields)
}

fn object_of(fields: &[Field]) -> Value {
    let object: Map<String, Value> = fields
        .iter()
        .map(|field| (field.name.clone(), value_of(field)))
        .collect();
    Value::Object(object)
}

fn value_of(field: &Field) -> Value {
    if let Some(default) = &field.default {
        return default.clone();
    }
    if let Some(item) = field.items.as_deref() {
        return Value::Array(vec![item_of(item)]);
    }
    sample(
        field.kind,
        field.options.as_deref(),
        field.fields.as_deref(),
    )
}

fn item_of(item: &Item) -> Value {
    sample(item.kind, item.options.as_deref(), item.fields.as_deref())
}

// A value of a simple type, a choice or an object; settings.json has no list of lists.
fn sample(kind: FieldType, options: Option<&[String]>, fields: Option<&[Field]>) -> Value {
    match kind {
        FieldType::Integer => Value::from(1),
        FieldType::Number => Value::from(1.5),
        FieldType::Boolean => Value::from(true),
        FieldType::Duration => Value::from("1m"),
        FieldType::Choice => Value::from(options.unwrap_or_default()[0].as_str()),
        FieldType::Object => object_of(fields.unwrap_or_default()),
        _ => Value::from("x"),
    }
}

/// The full document with one key it does not declare, once for each of its objects, with the
/// path of that key.
pub fn undeclared_documents(settings_json: &str) -> Vec<(String, Value)> {
    undeclared(&full_document(settings_json))
}

fn undeclared(full: &Value) -> Vec<(String, Value)> {
    let mut objects = Vec::new();
    find_objects(full, "", "", &mut objects);
    objects
        .into_iter()
        .map(|(path, pointer)| {
            let mut document = full.clone();
            let object = document
                .pointer_mut(&pointer)
                .and_then(Value::as_object_mut)
                .expect("the pointer names an object of the document");
            let mut key = "undeclared".to_owned();
            while object.contains_key(&key) {
                key.push('_');
            }
            let path = join(&path, &key);
            object.insert(key, Value::from("x"));
            (path, document)
        })
        .collect()
}

// The path and JSON pointer of each object in value, parents first.
fn find_objects(value: &Value, path: &str, pointer: &str, found: &mut Vec<(String, String)>) {
    match value {
        Value::Object(object) => {
            found.push((path.to_owned(), pointer.to_owned()));
            for (name, child) in object {
                find_objects(
                    child,
                    &join(path, name),
                    &format!("{pointer}/{name}"),
                    found,
                );
            }
        }
        Value::Array(items) => {
            for (index, item) in items.iter().enumerate() {
                let path = format!("{path}[{index}]");
                find_objects(item, &path, &format!("{pointer}/{index}"), found);
            }
        }
        _ => {}
    }
}

fn join(parent: &str, name: &str) -> String {
    if parent.is_empty() {
        return name.to_owned();
    }
    format!("{parent}.{name}")
}

/// Panics unless T reads the full document and refuses each undeclared document.
#[track_caller]
pub fn assert_no_drift<T: DeserializeOwned>(settings_json: &str) {
    let full = full_document(settings_json);
    if let Err(err) = serde_json::from_value::<T>(full.clone()) {
        panic!("the configuration refuses a document with every declared field: {err}");
    }
    for (path, document) in undeclared(&full) {
        if serde_json::from_value::<T>(document).is_ok() {
            panic!("the configuration accepts an undeclared field at {path}");
        }
    }
}

pub struct DocumentCommand {
    pub command: Command,
    // Removes the file when dropped; it outlives a command moved out of the struct.
    _file: TempPath,
}

/// A command that runs program with args and the document on descriptor 3, named by
/// MONITOR_HUB_CONFIG_FD, in an empty environment. Add variables with env or envs; env_clear
/// would remove MONITOR_HUB_CONFIG_FD.
pub fn command_with_document(
    program: impl AsRef<Path>,
    args: &[&str],
    document: &[u8],
) -> io::Result<DocumentCommand> {
    let mut file = NamedTempFile::new()?;
    file.write_all(document)?;
    let file = file.into_temp_path();
    let mut command = Command::new("/bin/sh");
    command
        .args(["-c", r#"exec "$@" 3<"$0""#])
        .arg(&file)
        .arg(program.as_ref())
        .args(args)
        .env_clear()
        .env(CONFIG_FD_ENV, "3");
    Ok(DocumentCommand {
        command,
        _file: file,
    })
}
