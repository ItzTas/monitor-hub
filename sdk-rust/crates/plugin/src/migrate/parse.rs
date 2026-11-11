#[derive(Debug, PartialEq, Eq)]
pub(super) enum Problem {
    Unsupported(String),
    Invalid(String),
}

#[derive(Clone, Copy, PartialEq, Eq)]
enum Direction {
    Up,
    Down,
}

#[derive(Clone, Copy, PartialEq, Eq)]
enum State {
    Start,
    Up,
    BeginUp,
    EndUp,
    Down,
    BeginDown,
    EndDown,
}

#[derive(Clone, Copy)]
enum Annotation {
    Up,
    Down,
    StatementBegin,
    StatementEnd,
}

pub(super) fn up_statements(sql: &str) -> Result<Vec<String>, Problem> {
    let up = statements(sql, Direction::Up)?;
    statements(sql, Direction::Down)?;
    Ok(up)
}

fn statements(sql: &str, direction: Direction) -> Result<Vec<String>, Problem> {
    let mut state = State::Start;
    let mut buf = String::new();
    let mut found = Vec::new();
    for line in sql.lines() {
        if state == State::Start && line.trim().is_empty() {
            continue;
        }
        if line.trim_start().starts_with("--") && line.contains("+goose") {
            let annotation = annotation(line)?;
            state = transition(state, annotation, &buf)?;
            if !matches!(annotation, Annotation::StatementEnd) {
                continue;
            }
        }
        if buf.is_empty() && (line.trim_start().starts_with("--") || line.is_empty()) {
            continue;
        }
        if !matches!(state, State::EndUp | State::EndDown) {
            buf.push_str(line);
            buf.push('\n');
        }
        let section = match state {
            State::Up | State::BeginUp | State::EndUp => Direction::Up,
            State::Down | State::BeginDown | State::EndDown => Direction::Down,
            State::Start => return Err(must_start_with_up()),
        };
        if section != direction {
            buf.clear();
            continue;
        }
        let complete = match state {
            State::EndUp => {
                state = State::Up;
                true
            }
            State::EndDown => {
                state = State::Down;
                true
            }
            State::Up | State::Down => ends_with_semicolon(line),
            _ => false,
        };
        if complete {
            found.push(buf.trim().to_owned());
            buf.clear();
        }
    }
    match state {
        State::Start => Err(must_start_with_up()),
        State::BeginUp | State::BeginDown => Err(Problem::Invalid(
            "missing '-- +goose StatementEnd' annotation".to_owned(),
        )),
        _ if !buf.trim().is_empty() => Err(missing_semicolon(&buf)),
        _ => Ok(found),
    }
}

fn transition(state: State, annotation: Annotation, buf: &str) -> Result<State, Problem> {
    let invalid = |reason: &str| Err(Problem::Invalid(reason.to_owned()));
    match (annotation, state) {
        (Annotation::Up, State::Start) => Ok(State::Up),
        (Annotation::Up, _) => invalid("duplicate '-- +goose Up' annotations"),
        (Annotation::Down, State::Up | State::EndUp) if !buf.trim().is_empty() => {
            Err(missing_semicolon(buf))
        }
        (Annotation::Down, State::Up | State::EndUp) => Ok(State::Down),
        (Annotation::Down, _) => Err(must_start_with_up()),
        (Annotation::StatementBegin, State::Up | State::EndUp) => Ok(State::BeginUp),
        (Annotation::StatementBegin, State::Down | State::EndDown) => Ok(State::BeginDown),
        (Annotation::StatementBegin, _) => {
            invalid("'-- +goose StatementBegin' must come after '-- +goose Up' or '-- +goose Down'")
        }
        (Annotation::StatementEnd, State::BeginUp) => Ok(State::EndUp),
        (Annotation::StatementEnd, State::BeginDown) => Ok(State::EndDown),
        (Annotation::StatementEnd, _) => {
            invalid("'-- +goose StatementEnd' must come after '-- +goose StatementBegin'")
        }
    }
}

fn annotation(line: &str) -> Result<Annotation, Problem> {
    if line.starts_with([' ', '\t']) {
        return Err(Problem::Invalid(format!(
            "annotation {line:?} starts with whitespace"
        )));
    }
    let command = line.replace("--", "").replacen("+goose", "", 1);
    if command.contains("+goose") {
        return Err(Problem::Invalid(format!(
            "line {line:?} holds more than one '+goose' annotation"
        )));
    }
    let command = command.trim();
    if command.is_empty() {
        return Err(Problem::Invalid(format!(
            "line {line:?} is an empty annotation"
        )));
    }
    let known = [
        ("Up", Some(Annotation::Up)),
        ("Down", Some(Annotation::Down)),
        ("StatementBegin", Some(Annotation::StatementBegin)),
        ("StatementEnd", Some(Annotation::StatementEnd)),
        ("NO TRANSACTION", None),
        ("ENVSUB ON", None),
        ("ENVSUB OFF", None),
    ];
    let found = known
        .iter()
        .find(|(name, _)| name.eq_ignore_ascii_case(command));
    match found {
        Some((_, Some(annotation))) => Ok(*annotation),
        Some((name, None)) => Err(Problem::Unsupported((*name).to_owned())),
        None => Err(Problem::Unsupported(command.to_owned())),
    }
}

fn ends_with_semicolon(line: &str) -> bool {
    line.split_whitespace()
        .take_while(|word| !word.starts_with("--"))
        .last()
        .is_some_and(|word| word.ends_with(';'))
}

fn must_start_with_up() -> Problem {
    Problem::Invalid("the file must start with a '-- +goose Up' annotation".to_owned())
}

fn missing_semicolon(buf: &str) -> Problem {
    Problem::Invalid(format!(
        "unfinished statement {:?}: missing semicolon?",
        buf.trim()
    ))
}

#[cfg(test)]
mod tests {
    use super::{Problem, up_statements};

    #[test]
    fn statements_are_split_the_way_goose_splits_them() {
        let sql = "-- leading comment\n\
                   \n\
                   -- +goose Up\n\
                   -- a comment before the statement\n\
                   CREATE TABLE a (\n\
                   \x20   id integer -- trailing comment;\n\
                   );\n\
                   \n\
                   INSERT INTO a VALUES (1); -- done\n\
                   -- +goose StatementBegin\n\
                   CREATE FUNCTION f() RETURNS void AS $$ BEGIN PERFORM 1; END; $$ LANGUAGE plpgsql;\n\
                   SELECT 2;\n\
                   -- +goose StatementEnd\n\
                   -- +goose down\n\
                   DROP TABLE a;\n";

        assert_eq!(
            up_statements(sql),
            Ok(vec![
                "CREATE TABLE a (\n    id integer -- trailing comment;\n);".to_owned(),
                "INSERT INTO a VALUES (1); -- done".to_owned(),
                "CREATE FUNCTION f() RETURNS void AS $$ BEGIN PERFORM 1; END; $$ LANGUAGE plpgsql;\nSELECT 2;".to_owned(),
            ])
        );
    }

    #[test]
    fn an_up_section_with_no_statements_is_empty() {
        assert_eq!(
            up_statements("-- +goose Up\n-- +goose Down\nDROP TABLE a;\n"),
            Ok(vec![])
        );
    }

    #[test]
    fn annotations_other_than_the_supported_ones_are_unsupported() {
        for (sql, annotation) in [
            ("-- +goose Up\n-- +goose no transaction\n", "NO TRANSACTION"),
            ("-- +goose ENVSUB ON\n-- +goose Up\n", "ENVSUB ON"),
            (
                "-- +goose Up\n-- +goose Down\n-- +goose ENVSUB OFF\n",
                "ENVSUB OFF",
            ),
            ("-- +goose Up\n-- +goose Later\n", "Later"),
        ] {
            assert_eq!(
                up_statements(sql),
                Err(Problem::Unsupported(annotation.to_owned())),
                "{sql}"
            );
        }
    }

    #[test]
    fn malformed_files_are_invalid() {
        for sql in [
            "CREATE TABLE a (id integer);\n",
            "-- just a comment\n",
            "SELECT 1;\n-- +goose Up\n",
            "-- +goose Up\n-- +goose Up\n",
            "-- +goose Up\nSELECT 1\n",
            "-- +goose Up\nSELECT 1\n-- +goose Down\n",
            "-- +goose Up\n-- +goose Down\nSELECT 1\n",
            "-- +goose Up\n-- +goose StatementBegin\nSELECT 1;\n",
            "-- +goose Up\n-- +goose StatementEnd\n",
            "-- +goose Down\n",
            " -- +goose Up\n",
            "-- +goose Up -- +goose Down\n",
            "-- +goose\n",
        ] {
            assert!(
                matches!(up_statements(sql), Err(Problem::Invalid(_))),
                "{sql:?} gave {:?}",
                up_statements(sql)
            );
        }
    }
}
