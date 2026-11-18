use std::error::Error;
use std::io::{self, Write};

use monitor_hub_plugin::settings::{
    Answer, FieldProblem, FieldType, Problem, Settings, SettingsError, is_reserved, parse,
    write_answer,
};

const DESIGN_EXAMPLE: &str = r#"{
  "settings_version": 1,
  "fields": [
    {
      "name": "WATCH_TERMS",
      "label": "Watch terms",
      "help": "Names or registration numbers to look for in each edition, separated by commas.",
      "type": "list",
      "required": true
    },
    {
      "name": "CHECK_INTERVAL_MINUTES",
      "label": "Check interval (minutes)",
      "type": "number",
      "default": "30"
    },
    {
      "name": "DATABASE_URL",
      "label": "Database address",
      "type": "string",
      "secret": true,
      "required": true
    }
  ]
}"#;

type StructuralCase = (&'static str, Vec<u8>, Box<dyn Fn(&SettingsError) -> bool>);

fn field(name: &str) -> String {
    format!(r#"{{"name":"{name}","label":"L","type":"string"}}"#)
}

fn document(fields: &[String]) -> String {
    format!(
        r#"{{"settings_version":1,"fields":[{}]}}"#,
        fields.join(",")
    )
}

fn with_bad_field(bad: &str) -> String {
    document(&[field("OK_FIELD"), bad.to_owned()])
}

fn parse_str(json: &str) -> Result<Settings, SettingsError> {
    parse(json.as_bytes())
}

fn source_text(err: &SettingsError) -> String {
    err.source().map(ToString::to_string).unwrap_or_default()
}

#[test]
fn the_design_example_parses() {
    let settings = parse_str(DESIGN_EXAMPLE).unwrap();
    assert_eq!(settings.settings_version, 1);
    assert_eq!(settings.fields.len(), 3);

    let terms = &settings.fields[0];
    assert_eq!(terms.name, "WATCH_TERMS");
    assert_eq!(terms.kind, FieldType::List);
    assert!(terms.required);

    let interval = &settings.fields[1];
    assert_eq!(interval.kind, FieldType::Number);
    assert_eq!(interval.default.as_deref(), Some("30"));

    let database = &settings.fields[2];
    assert!(database.secret);

    for each in &settings.fields {
        assert!(each.editable);
        assert_eq!(each.options, None);
    }
}

#[test]
fn every_attribute_is_read() {
    let json = document(&[r#"{
        "name": "MODE",
        "label": "Mode",
        "help": "Pick one.",
        "type": "choice",
        "options": ["a", "b"],
        "default": "b",
        "group": "G",
        "required": true,
        "editable": false,
        "reason": "R"
    }"#
    .to_owned()]);
    let settings = parse_str(&json).unwrap();
    let got = &settings.fields[0];
    assert_eq!(got.name, "MODE");
    assert_eq!(got.label, "Mode");
    assert_eq!(got.help.as_deref(), Some("Pick one."));
    assert_eq!(got.kind, FieldType::Choice);
    assert!(!got.secret);
    assert!(got.required);
    assert_eq!(got.default.as_deref(), Some("b"));
    assert_eq!(
        got.options.as_deref(),
        Some(&["a".to_owned(), "b".to_owned()][..])
    );
    assert_eq!(got.group.as_deref(), Some("G"));
    assert!(!got.editable);
    assert_eq!(got.reason.as_deref(), Some("R"));
}

#[test]
fn null_optional_attributes_count_as_absent() {
    let json = document(&[
        r#"{"name":"A","label":"L","type":"string","help":null,"default":null,"options":null,"group":null,"reason":null}"#
            .to_owned(),
    ]);
    let settings = parse_str(&json).unwrap();
    let got = &settings.fields[0];
    assert_eq!(got.help, None);
    assert_eq!(got.default, None);
    assert_eq!(got.options, None);
    assert_eq!(got.group, None);
    assert_eq!(got.reason, None);
}

#[test]
fn structural_errors_are_reported() {
    let valid = document(&[field("A")]);
    let cases: Vec<StructuralCase> = vec![
        (
            "bom",
            format!("\u{feff}{valid}").into_bytes(),
            Box::new(|e| matches!(e, SettingsError::ByteOrderMark)),
        ),
        (
            "trailing data",
            format!("{valid}{{}}").into_bytes(),
            Box::new(|e| matches!(e, SettingsError::Format(_))),
        ),
        (
            "unknown top-level key",
            r#"{"settings_version":1,"fields":[],"extra":1}"#.as_bytes().to_vec(),
            Box::new(|e| {
                matches!(e, SettingsError::Format(_)) && source_text(e).contains("`extra`")
            }),
        ),
        (
            "unknown field attribute",
            r#"{"settings_version":1,"fields":[{"name":"A","label":"L","type":"string","secert":true}]}"#
                .as_bytes()
                .to_vec(),
            Box::new(|e| {
                matches!(e, SettingsError::Format(_)) && source_text(e).contains("`secert`")
            }),
        ),
        (
            "unknown type",
            r#"{"settings_version":1,"fields":[{"name":"A","label":"L","type":"password"}]}"#
                .as_bytes()
                .to_vec(),
            Box::new(|e| {
                matches!(e, SettingsError::Format(_)) && source_text(e).contains("`password`")
            }),
        ),
        (
            "wrong json type",
            r#"{"settings_version":1,"fields":[{"name":"A","label":"L","type":"string","default":30}]}"#
                .as_bytes()
                .to_vec(),
            Box::new(|e| matches!(e, SettingsError::Format(_))),
        ),
        (
            "missing label",
            r#"{"settings_version":1,"fields":[{"name":"A","type":"string"}]}"#
                .as_bytes()
                .to_vec(),
            Box::new(|e| {
                matches!(e, SettingsError::Format(_)) && source_text(e).contains("`label`")
            }),
        ),
        (
            "missing settings_version",
            br#"{"fields":[]}"#.to_vec(),
            Box::new(|e| matches!(e, SettingsError::Format(_))),
        ),
        (
            "duplicate attribute",
            r#"{"settings_version":1,"fields":[{"name":"A","name":"B","label":"L","type":"string"}]}"#
                .as_bytes()
                .to_vec(),
            Box::new(|e| matches!(e, SettingsError::Format(_))),
        ),
        (
            "invalid utf-8",
            b"{\"settings_version\":1,\"fields\":[{\"name\":\"A\",\"label\":\"\xff\",\"type\":\"string\"}]}"
                .to_vec(),
            Box::new(|e| matches!(e, SettingsError::Format(_))),
        ),
        (
            "array document",
            b"[1,[]]".to_vec(),
            Box::new(|e| {
                matches!(e, SettingsError::Format(_))
                    && source_text(e).starts_with("invalid type: sequence, expected struct Settings")
            }),
        ),
        (
            "positional field",
            br#"{"settings_version":1,"fields":[["N","Label",null,"string",false,false,null,null,null,true,null]]}"#
                .to_vec(),
            Box::new(|e| {
                matches!(e, SettingsError::Format(_))
                    && source_text(e).starts_with("invalid type: sequence, expected struct Field")
            }),
        ),
        (
            "version 2",
            br#"{"settings_version":2,"fields":[]}"#.to_vec(),
            Box::new(|e| matches!(e, SettingsError::UnsupportedVersion(2))),
        ),
        (
            "version 0",
            br#"{"settings_version":0,"fields":[]}"#.to_vec(),
            Box::new(|e| matches!(e, SettingsError::UnsupportedVersion(0))),
        ),
    ];

    for (case, input, check) in cases {
        let err = parse(&input).expect_err(case);
        assert!(check(&err), "{case}: {err:?}");
    }
}

#[test]
fn empty_field_list_is_valid() {
    let settings = parse_str(r#"{"settings_version":1,"fields":[]}"#).unwrap();
    assert!(settings.fields.is_empty());
}

#[test]
fn error_texts_are_stable() {
    let err = parse(br#"{"settings_version":2,"fields":[]}"#).unwrap_err();
    assert_eq!(
        err.to_string(),
        "settings.json: settings_version is 2, want 1"
    );
    let err = parse(b"\xef\xbb\xbf{}").unwrap_err();
    assert_eq!(
        err.to_string(),
        "settings.json starts with a UTF-8 byte order mark"
    );
    let err = parse(b"{").unwrap_err();
    assert_eq!(err.to_string(), "settings.json is not valid");
    assert!(err.source().is_some());
}

#[test]
fn field_rules_are_enforced() {
    let two_watch = document(&[field("WATCH_TERMS"), field("WATCH_TERMS")]);
    let cases: Vec<(&str, String, FieldProblem, &str)> = vec![
        (
            "lowercase name",
            with_bad_field(&field("watch_terms")),
            FieldProblem::InvalidName("watch_terms".into()),
            r#"settings.json: fields[1]: name "watch_terms" does not match ^[A-Z][A-Z0-9_]*$"#,
        ),
        (
            "digit first",
            with_bad_field(&field("9X")),
            FieldProblem::InvalidName("9X".into()),
            r#"settings.json: fields[1]: name "9X" does not match ^[A-Z][A-Z0-9_]*$"#,
        ),
        (
            "empty name",
            with_bad_field(&field("")),
            FieldProblem::InvalidName(String::new()),
            r#"settings.json: fields[1]: name "" does not match ^[A-Z][A-Z0-9_]*$"#,
        ),
        (
            "trailing newline in name",
            with_bad_field(r#"{"name":"A\n","label":"L","type":"string"}"#),
            FieldProblem::InvalidName("A\n".into()),
            r#"settings.json: fields[1]: name "A\n" does not match ^[A-Z][A-Z0-9_]*$"#,
        ),
        (
            "dash in name",
            with_bad_field(&field("A-B")),
            FieldProblem::InvalidName("A-B".into()),
            r#"settings.json: fields[1]: name "A-B" does not match ^[A-Z][A-Z0-9_]*$"#,
        ),
        (
            "space in name",
            with_bad_field(&field("A B")),
            FieldProblem::InvalidName("A B".into()),
            r#"settings.json: fields[1]: name "A B" does not match ^[A-Z][A-Z0-9_]*$"#,
        ),
        (
            "reserved PATH",
            with_bad_field(&field("PATH")),
            FieldProblem::ReservedName("PATH".into()),
            r#"settings.json: fields[1]: name "PATH" is reserved by the hub"#,
        ),
        (
            "reserved LD_PRELOAD",
            with_bad_field(&field("LD_PRELOAD")),
            FieldProblem::ReservedName("LD_PRELOAD".into()),
            r#"settings.json: fields[1]: name "LD_PRELOAD" is reserved by the hub"#,
        ),
        (
            "reserved MONITOR_HUB_X",
            with_bad_field(&field("MONITOR_HUB_X")),
            FieldProblem::ReservedName("MONITOR_HUB_X".into()),
            r#"settings.json: fields[1]: name "MONITOR_HUB_X" is reserved by the hub"#,
        ),
        (
            "duplicate name",
            two_watch,
            FieldProblem::DuplicateName("WATCH_TERMS".into()),
            r#"settings.json: fields[1]: name "WATCH_TERMS" is declared more than once"#,
        ),
        (
            "blank label",
            with_bad_field(r#"{"name":"B","label":"  ","type":"string"}"#),
            FieldProblem::EmptyLabel,
            "settings.json: fields[1]: label is empty",
        ),
        (
            "choice without options",
            with_bad_field(r#"{"name":"B","label":"L","type":"choice"}"#),
            FieldProblem::MissingOptions,
            "settings.json: fields[1]: a choice field needs at least one option",
        ),
        (
            "choice with empty options",
            with_bad_field(r#"{"name":"B","label":"L","type":"choice","options":[]}"#),
            FieldProblem::MissingOptions,
            "settings.json: fields[1]: a choice field needs at least one option",
        ),
        (
            "options on a string",
            with_bad_field(r#"{"name":"B","label":"L","type":"string","options":["a"]}"#),
            FieldProblem::UnexpectedOptions,
            "settings.json: fields[1]: options are only for choice fields",
        ),
        (
            "blank option",
            with_bad_field(r#"{"name":"B","label":"L","type":"choice","options":["a"," "]}"#),
            FieldProblem::EmptyOption,
            "settings.json: fields[1]: an option is empty",
        ),
        (
            "duplicate option",
            with_bad_field(r#"{"name":"B","label":"L","type":"choice","options":["a","b","a"]}"#),
            FieldProblem::DuplicateOption("a".into()),
            r#"settings.json: fields[1]: option "a" is listed more than once"#,
        ),
        (
            "default not an option",
            with_bad_field(
                r#"{"name":"B","label":"L","type":"choice","options":["a"],"default":"z"}"#,
            ),
            FieldProblem::DefaultNotAnOption("z".into()),
            r#"settings.json: fields[1]: default "z" is not one of the options"#,
        ),
        (
            "secret with an empty default",
            with_bad_field(
                r#"{"name":"B","label":"L","type":"string","secret":true,"default":""}"#,
            ),
            FieldProblem::SecretDefault,
            "settings.json: fields[1]: a secret field cannot have a default",
        ),
        (
            "not editable without reason",
            with_bad_field(r#"{"name":"B","label":"L","type":"string","editable":false}"#),
            FieldProblem::MissingReason,
            "settings.json: fields[1]: editable: false needs a non-empty reason",
        ),
        (
            "not editable with blank reason",
            with_bad_field(
                r#"{"name":"B","label":"L","type":"string","editable":false,"reason":" "}"#,
            ),
            FieldProblem::MissingReason,
            "settings.json: fields[1]: editable: false needs a non-empty reason",
        ),
        (
            "reason on an editable field",
            with_bad_field(r#"{"name":"B","label":"L","type":"string","reason":"R"}"#),
            FieldProblem::UnexpectedReason,
            "settings.json: fields[1]: reason is only for fields with editable: false",
        ),
        (
            "reason on an explicitly editable field",
            with_bad_field(
                r#"{"name":"B","label":"L","type":"string","editable":true,"reason":"R"}"#,
            ),
            FieldProblem::UnexpectedReason,
            "settings.json: fields[1]: reason is only for fields with editable: false",
        ),
        (
            "blank group",
            with_bad_field(r#"{"name":"B","label":"L","type":"string","group":" "}"#),
            FieldProblem::EmptyGroup,
            "settings.json: fields[1]: group is empty",
        ),
    ];

    for (case, json, problem, text) in cases {
        let err = parse_str(&json).expect_err(case);
        match &err {
            SettingsError::Field {
                index,
                problem: got,
            } => {
                assert_eq!(*index, 1, "{case}");
                assert_eq!(got, &problem, "{case}");
            }
            other => panic!("{case}: {other:?}"),
        }
        assert_eq!(err.to_string(), text, "{case}");
    }
}

#[test]
fn the_first_problem_in_table_order_wins() {
    let json = with_bad_field(
        r#"{"name":"B","label":"L","type":"string","secret":true,"default":"x","options":["a"]}"#,
    );
    let err = parse_str(&json).unwrap_err();
    assert!(matches!(
        err,
        SettingsError::Field {
            problem: FieldProblem::UnexpectedOptions,
            ..
        }
    ));
}

#[test]
fn secret_choice_is_allowed() {
    let json = document(&[
        r#"{"name":"B","label":"L","type":"choice","secret":true,"options":["a"]}"#.to_owned(),
    ]);
    assert!(parse_str(&json).is_ok());
}

#[test]
fn reserved_names() {
    for name in [
        "PATH",
        "TZ",
        "TMPDIR",
        "PLUGIN_",
        "PLUGIN_X",
        "MONITOR_HUB_PLUGIN",
        "LD_PRELOAD",
    ] {
        assert!(is_reserved(name), "{name}");
    }
    for name in ["PATHS", "TZ_NAME", "LD", "DATABASE_URL"] {
        assert!(!is_reserved(name), "{name}");
    }
}

struct Failing;

impl Write for Failing {
    fn write(&mut self, _: &[u8]) -> io::Result<usize> {
        Err(io::Error::other("closed"))
    }

    fn flush(&mut self) -> io::Result<()> {
        Err(io::Error::other("closed"))
    }
}

struct FlushFails;

impl Write for FlushFails {
    fn write(&mut self, buf: &[u8]) -> io::Result<usize> {
        Ok(buf.len())
    }

    fn flush(&mut self) -> io::Result<()> {
        Err(io::Error::other("closed"))
    }
}

#[test]
fn answer_bytes_and_codes() {
    let mut out = Vec::new();
    assert_eq!(write_answer(&mut out, &[]), 0);
    assert_eq!(out, b"{\"problems\":[]}\n");

    let mut out = Vec::new();
    let problems = [
        Problem::new("WATCH_TERMS", "set at least one name"),
        Problem::new("", "pick \"a\" or é"),
    ];
    assert_eq!(write_answer(&mut out, &problems), 1);
    assert_eq!(
        String::from_utf8(out).unwrap(),
        "{\"problems\":[{\"name\":\"WATCH_TERMS\",\"message\":\"set at least one name\"},{\"name\":\"\",\"message\":\"pick \\\"a\\\" or é\"}]}\n"
    );

    assert_eq!(write_answer(&mut Failing, &[]), 2);
    assert_eq!(write_answer(&mut Failing, &problems), 2);
    assert_eq!(write_answer(&mut FlushFails, &[]), 2);
}

#[test]
fn answers_are_read_leniently() {
    let answer: Answer =
        serde_json::from_str(r#"{"problems":[{"message":"m","code":7}],"extra":true}"#).unwrap();
    assert_eq!(answer.problems, vec![Problem::new("", "m")]);

    assert!(serde_json::from_str::<Answer>(r#"{"problems":null}"#).is_err());
    assert!(serde_json::from_str::<Answer>("{}").is_err());
}
