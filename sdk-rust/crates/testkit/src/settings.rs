use std::collections::BTreeSet;
use std::error::Error;
use std::fs;
use std::iter;
use std::os::unix::process::ExitStatusExt;
use std::path::{self, Path};
use std::process::{Command, ExitStatus};

use monitor_hub_plugin::settings::{
    ANSWER_LIMIT, Answer, SETTINGS_FILE, Settings, VALIDATE_SETTINGS_ARG, VALIDATE_SETTINGS_LIMIT,
    is_reserved, parse,
};
use serde_json::Value;

use crate::process::{Failure, Finished, Keep, execute};

pub fn check(dir: impl AsRef<Path>, program: impl AsRef<Path>) -> Vec<String> {
    let dir = dir.as_ref();
    let settings = read_settings(dir);
    let declared: Option<BTreeSet<&str>> = settings.as_ref().ok().map(|settings| {
        settings
            .fields
            .iter()
            .map(|field| field.name.as_str())
            .collect()
    });
    let validator = validator_violation(dir, program.as_ref(), declared.as_ref());
    settings.err().into_iter().chain(validator).collect()
}

#[track_caller]
pub fn assert_no_drift(settings_json: &str, read: &[&str]) {
    let settings = match parse(settings_json.as_bytes()) {
        Ok(settings) => settings,
        Err(err) => panic!("{}", chain(&err)),
    };
    let declared: BTreeSet<&str> = settings
        .fields
        .iter()
        .map(|field| field.name.as_str())
        .collect();
    let read: BTreeSet<&str> = read
        .iter()
        .copied()
        .filter(|name| !is_reserved(name))
        .collect();
    let sides = [
        ("read but not declared", read.difference(&declared)),
        ("declared but not read", declared.difference(&read)),
    ];
    let lines: Vec<String> = sides
        .into_iter()
        .map(|(side, names)| (side, names.copied().collect::<Vec<_>>()))
        .filter(|(_, names)| !names.is_empty())
        .map(|(side, names)| format!("\n  {side}: {}", names.join(", ")))
        .collect();
    if lines.is_empty() {
        return;
    }
    panic!(
        "{SETTINGS_FILE} and the configuration disagree:{}",
        lines.concat()
    );
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

fn validator_violation(
    dir: &Path,
    program: &Path,
    declared: Option<&BTreeSet<&str>>,
) -> Option<String> {
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
        Ok(finished) => answer_violation(&finished, declared).err(),
        Err(Failure::Timeout(_)) => Some(format!(
            "validate-settings did not finish within {}s",
            VALIDATE_SETTINGS_LIMIT.as_secs()
        )),
        Err(Failure::Start(err)) => Some(format!("{not_started}: {}: {err}", program.display())),
    }
}

fn answer_violation(finished: &Finished, declared: Option<&BTreeSet<&str>>) -> Result<(), String> {
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
        _ => undeclared_violation(&answer, declared),
    }
}

fn undeclared_violation(answer: &Answer, declared: Option<&BTreeSet<&str>>) -> Result<(), String> {
    let Some(declared) = declared else {
        return Ok(());
    };
    let Some((index, problem)) =
        answer.problems.iter().enumerate().find(|(_, problem)| {
            !problem.name.is_empty() && !declared.contains(problem.name.as_str())
        })
    else {
        return Ok(());
    };
    Err(format!(
        "validate-settings answer is invalid: problem {index} names {:?}, which {SETTINGS_FILE} does not declare",
        problem.name
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
