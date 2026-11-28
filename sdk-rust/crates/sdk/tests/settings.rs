use std::error::Error;
use std::fs;
use std::io::{self, Write};
use std::path::Path;

use monitor_hub_sdk::settings::{
    Answer, FieldProblem, FieldType, Problem, SettingsError, is_reserved, parse, parse_duration,
    write_answer,
};
use serde::Deserialize;
use serde_json::{Value, json};

const DESIGN_EXAMPLE: &str = r#"{
  "settings_version": 2,
  "fields": [
    {"name": "sei_url", "label": "SEI address", "type": "string", "required": true, "env": "SEI_URL"},
    {"name": "password", "label": "SEI password", "type": "string", "secret": true, "env": "SEI_PASSWORD",
     "help": "Saving restarts the monitor, and with a valid password that is one login to SEI."},
    {"name": "units", "label": "Units", "type": "list", "items": {"type": "string"}, "env": "SEI_UNITS"},
    {"name": "poll_interval", "label": "Check interval", "type": "duration", "default": "15m", "env": "POLL_INTERVAL"},
    {"name": "password_command", "label": "Password command", "type": "string", "secret": true,
     "editable": false, "reason": "It makes the monitor run a program inside the hub's container.",
     "env": "SEI_PASSWORD_CMD"}
  ]
}"#;

const OK_FIELD: &str = r#"{"name":"ok","label":"OK","type":"string"}"#;

// A field written as a list, with every attribute in declaration order.
const LISTED_FIELD: &str =
    r#"["x","X",null,"string",false,false,null,null,null,true,null,null,null,null]"#;

fn document(fields: &[&str]) -> String {
    format!(
        r#"{{"settings_version":2,"fields":[{}]}}"#,
        fields.join(",")
    )
}

fn second(field: &str) -> String {
    document(&[OK_FIELD, field])
}

#[derive(Deserialize)]
struct Vectors {
    cases: Vec<Vector>,
}

#[derive(Deserialize)]
struct Vector {
    name: String,
    settings: Value,
    problem: Option<String>,
}

#[test]
fn the_rules_follow_the_shared_vectors() {
    let path =
        Path::new(env!("CARGO_MANIFEST_DIR")).join("../../../testdata/config/declarations.json");
    let vectors: Vectors =
        serde_json::from_slice(&fs::read(&path).expect("read the vectors")).expect("vectors");
    assert!(!vectors.cases.is_empty(), "{} has no cases", path.display());

    for case in vectors.cases {
        let json = serde_json::to_vec(&case.settings).expect("encode the case");
        let got = parse(&json).map(drop).map_err(|err| err.to_string());

        assert_eq!(got, case.problem.map_or(Ok(()), Err), "{}", case.name);
    }
}

#[test]
fn structural_errors_are_reported() {
    let valid = document(&[OK_FIELD]);
    let err = parse(format!("\u{feff}{valid}").as_bytes()).unwrap_err();
    assert!(matches!(err, SettingsError::ByteOrderMark), "{err:?}");

    let format: Vec<(&str, Vec<u8>)> = vec![
        ("trailing data", format!("{valid}{{}}").into()),
        (
            "misspelled attribute",
            second(r#"{"name":"b","label":"L","type":"string","secert":true}"#).into(),
        ),
        (
            "unknown type",
            second(r#"{"name":"b","label":"L","type":"password"}"#).into(),
        ),
        (
            "label that is a number",
            second(r#"{"name":"b","label":1,"type":"string"}"#).into(),
        ),
        (
            "null required",
            second(r#"{"name":"b","label":"L","type":"string","required":null}"#).into(),
        ),
        (
            "null editable",
            second(r#"{"name":"b","label":"L","type":"string","editable":null}"#).into(),
        ),
        (
            "repeated name",
            second(r#"{"name":"b","name":"c","label":"L","type":"string"}"#).into(),
        ),
        (
            "invalid UTF-8",
            b"{\"settings_version\":2,\"fields\":[{\"name\":\"b\",\"label\":\"\xff\",\"type\":\"string\"}]}"
                .into(),
        ),
        (
            "label in a list item",
            second(r#"{"name":"b","label":"L","type":"list","items":{"type":"string","label":"L"}}"#)
                .into(),
        ),
        (
            "unknown top-level attribute",
            r#"{"settings_version":2,"fields":[],"extra":1}"#.into(),
        ),
        ("no settings_version", r#"{"fields":[]}"#.into()),
        ("a list as the document", "[]".into()),
        ("a list with the document's values", "[2,[]]".into()),
        (
            "a list as a field",
            r#"{"settings_version":2,"fields":[["a","A",null,"string"]]}"#.into(),
        ),
        ("a list with a field's values", document(&[LISTED_FIELD]).into()),
        (
            "a list as items",
            second(r#"{"name":"b","label":"L","type":"list","items":["string"]}"#).into(),
        ),
        (
            "a list with an item's values",
            second(r#"{"name":"b","label":"L","type":"list","items":["string",null,null]}"#).into(),
        ),
        (
            "a list as a field of an object",
            second(&format!(
                r#"{{"name":"b","label":"L","type":"object","fields":[{LISTED_FIELD}]}}"#
            ))
            .into(),
        ),
        (
            "a list as a field of a list item",
            second(&format!(
                r#"{{"name":"b","label":"L","type":"list","items":{{"type":"object","fields":[{LISTED_FIELD}]}}}}"#
            ))
            .into(),
        ),
    ];
    for (case, json) in format {
        let err = parse(&json).expect_err(case);
        assert!(matches!(err, SettingsError::Format(_)), "{case}: {err:?}");
    }

    for version in [0, 3] {
        let json = format!(r#"{{"settings_version":{version},"fields":[]}}"#);
        let err = parse(json.as_bytes()).unwrap_err();
        assert!(
            matches!(err, SettingsError::UnsupportedVersion(got) if got == version),
            "{err:?}"
        );
    }
}

#[test]
fn the_design_example_reads_as_declared() {
    let settings = parse(DESIGN_EXAMPLE.as_bytes()).unwrap();
    assert_eq!(settings.fields.len(), 5);
    let field = |name: &str| {
        settings
            .fields
            .iter()
            .find(|field| field.name == name)
            .unwrap_or_else(|| panic!("{name} is declared"))
    };

    let poll_interval = field("poll_interval");
    assert_eq!(poll_interval.default, Some(json!("15m")));
    assert_eq!(poll_interval.env.as_deref(), Some("POLL_INTERVAL"));
    assert!(!field("password_command").is_editable());
}

#[test]
fn every_attribute_is_read() {
    let json = document(&[
        r#"{"name":"mode","label":"Mode","help":"Pick one.","type":"choice","required":true,"secret":false,
            "default":"b","options":["a","b"],"group":"G","editable":false,"reason":"R","env":"MODE"}"#,
        r#"{"name":"login","label":"Login","type":"object",
            "fields":[{"name":"password","label":"Password","type":"string","secret":true}]}"#,
        r#"{"name":"rules","label":"Rules","type":"list","items":{"type":"object",
            "fields":[{"name":"actions","label":"Actions","type":"list","items":{"type":"choice","options":["urgent"]}}]}}"#,
    ]);
    let settings = parse(json.as_bytes()).unwrap();
    assert_eq!(settings.settings_version, 2);
    let [mode, login, rules] = &settings.fields[..] else {
        panic!("{settings:?}");
    };

    assert_eq!(mode.name, "mode");
    assert_eq!(mode.label, "Mode");
    assert_eq!(mode.help.as_deref(), Some("Pick one."));
    assert_eq!(mode.kind, FieldType::Choice);
    assert!(mode.required);
    assert!(!mode.secret);
    assert_eq!(mode.default, Some(json!("b")));
    assert_eq!(mode.options, Some(vec!["a".to_owned(), "b".to_owned()]));
    assert_eq!(mode.group.as_deref(), Some("G"));
    assert_eq!(mode.editable, Some(false));
    assert!(!mode.is_editable());
    assert_eq!(mode.reason.as_deref(), Some("R"));
    assert_eq!(mode.env.as_deref(), Some("MODE"));

    assert_eq!(login.kind, FieldType::Object);
    let [password] = login.fields.as_deref().unwrap() else {
        panic!("{login:?}");
    };
    assert_eq!(password.name, "password");
    assert!(password.secret);
    assert!(!password.required);
    assert_eq!(password.editable, None);
    assert!(password.is_editable());

    assert_eq!(rules.kind, FieldType::List);
    let item = rules.items.as_deref().unwrap();
    assert_eq!(item.kind, FieldType::Object);
    let [actions] = item.fields.as_deref().unwrap() else {
        panic!("{item:?}");
    };
    assert_eq!(actions.name, "actions");
    let choice = actions.items.as_deref().unwrap();
    assert_eq!(choice.kind, FieldType::Choice);
    assert_eq!(choice.options, Some(vec!["urgent".to_owned()]));
    assert_eq!(choice.fields, None);
}

#[test]
fn null_optional_attributes_count_as_absent() {
    let json = document(&[
        r#"{"name":"a","label":"L","type":"string","help":null,"default":null,"options":null,"group":null,"reason":null,"env":null,"fields":null,"items":null}"#,
    ]);
    let settings = parse(json.as_bytes()).unwrap();
    let got = &settings.fields[0];
    assert_eq!(got.help, None);
    assert_eq!(got.default, None);
    assert_eq!(got.options, None);
    assert_eq!(got.group, None);
    assert_eq!(got.reason, None);
    assert_eq!(got.env, None);
    assert_eq!(got.fields, None);
    assert_eq!(got.items, None);
}

#[test]
fn error_texts_are_stable() {
    let err = parse(br#"{"settings_version":3,"fields":[]}"#).unwrap_err();
    assert_eq!(
        err.to_string(),
        "settings.json: settings_version is 3, want 2"
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
fn the_first_problem_in_table_order_wins() {
    let json = second(
        r#"{"name":"b","label":"L","type":"string","secret":true,"default":"x","options":["a"]}"#,
    );
    let err = parse(json.as_bytes()).unwrap_err();
    assert!(
        matches!(
            &err,
            SettingsError::Field {
                path,
                problem: FieldProblem::UnexpectedOptions,
                ..
            } if path == "fields[1]"
        ),
        "{err:?}"
    );
}

#[test]
fn reserved_names() {
    for name in [
        "PATH",
        "TZ",
        "TMPDIR",
        "PLUGIN_",
        "PLUGIN_X",
        "MONITOR_HUB_MONITOR",
        "LD_PRELOAD",
    ] {
        assert!(is_reserved(name), "{name}");
    }
    for name in ["PATHS", "TZ_NAME", "LD", "DATABASE_URL"] {
        assert!(!is_reserved(name), "{name}");
    }
}

#[test]
fn paths_are_resolved_against_the_declaration() {
    let settings = parse(
        document(&[
            r#"{"name":"sei_url","label":"SEI address","type":"string","required":true,"env":"SEI_URL"}"#,
            r#"{"name": "rules", "label": "Notification rules", "type": "list",
               "items": {"type": "object", "fields": [
                 {"name": "process", "label": "Process number (regular expression)", "type": "string", "required": true},
                 {"name": "actions", "label": "Actions", "type": "list", "items": {"type": "choice", "options": ["urgent", "ignore"]}}
               ]}}"#,
        ])
        .as_bytes(),
    )
    .unwrap();
    let cases = [
        ("", true),
        ("sei_url", true),
        ("rules", true),
        ("rules[0]", true),
        ("rules[12].process", true),
        ("rules[99999999999999999999].process", true),
        ("rules[3].actions", true),
        ("rules[3].actions[0]", true),
        ("rules.process", false),
        ("rules[01].process", false),
        ("rules[].process", false),
        ("rules[0].", false),
        ("rules[-1]", false),
        ("sei_url[0]", false),
        ("rules[0].other", false),
        ("SEI_URL", false),
        ("[0]", false),
        ("sei_url.", false),
    ];

    for (path, want) in cases {
        assert_eq!(settings.declares(path), want, "{path:?}");
    }
}

#[derive(Deserialize)]
struct DurationVectors {
    parse: Vec<DurationCase>,
}

#[derive(Deserialize)]
struct DurationCase {
    input: String,
    nanoseconds: Option<i64>,
}

#[test]
fn durations_follow_go() {
    let cases = [
        ("15m", Some(900_000_000_000)),
        ("1h30m", Some(5_400_000_000_000)),
        ("-5m", Some(-300_000_000_000)),
        (".5h", Some(1_800_000_000_000)),
        ("300ms", Some(300_000_000)),
        ("1µs", Some(1_000)),
        ("0", Some(0)),
        ("15", None),
        ("", None),
        ("1m-", None),
        ("5 m", None),
        ("9223372036h", None),
    ];
    for (input, want) in cases {
        assert_eq!(parse_duration(input), want, "{input:?}");
    }

    let vectors: DurationVectors =
        serde_json::from_str(include_str!("duration_parse.json")).expect("duration vectors");
    assert!(!vectors.parse.is_empty());
    for case in vectors.parse {
        assert_eq!(
            parse_duration(&case.input),
            case.nanoseconds,
            "{:?}",
            case.input
        );
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
        Problem::new("rules[2].process", "not a valid regular expression"),
        Problem::new("", "pick \"a\" or é"),
    ];
    assert_eq!(write_answer(&mut out, &problems), 1);
    assert_eq!(
        String::from_utf8(out).unwrap(),
        "{\"problems\":[{\"path\":\"rules[2].process\",\"message\":\"not a valid regular expression\"},{\"path\":\"\",\"message\":\"pick \\\"a\\\" or é\"}]}\n"
    );

    assert_eq!(write_answer(&mut Failing, &[]), 2);
    assert_eq!(write_answer(&mut Failing, &problems), 2);
    assert_eq!(write_answer(&mut FlushFails, &[]), 2);
}

#[test]
fn answers_are_read_leniently() {
    for json in [
        r#"{"problems":[{"message":"m","code":7}],"extra":true}"#,
        r#"{"problems":[{"message":"m","name":"x"}]}"#,
        r#"{"problems":[{"path":null,"message":"m"}]}"#,
    ] {
        let answer: Answer = serde_json::from_str(json).unwrap();
        assert_eq!(answer.problems, vec![Problem::new("", "m")], "{json}");
    }

    assert!(serde_json::from_str::<Answer>(r#"{"problems":null}"#).is_err());
    assert!(serde_json::from_str::<Answer>("{}").is_err());
}
