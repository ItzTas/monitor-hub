use std::error::Error;
use std::fs;
use std::iter;
use std::os::unix::process::ExitStatusExt;
use std::path::{self, Path};
use std::process::{Command, ExitStatus};

use monitor_hub_sdk::settings::{
    ANSWER_LIMIT, Answer, SETTINGS_FILE, Settings, VALIDATE_SETTINGS_ARG, VALIDATE_SETTINGS_LIMIT,
    parse,
};
use serde_json::Value;

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
