use std::collections::HashMap;
use std::ffi::{OsStr, OsString};
use std::fs;
use std::io::{self, Seek, Write};
use std::os::fd::OwnedFd;
use std::os::unix::ffi::OsStringExt;
use std::path::Path;

use monitor_hub_sdk::config::{
    DOCUMENT_LIMIT, Document, LoadError, descriptor_number, from_descriptor, from_lookup,
    with_defaults,
};
use monitor_hub_sdk::settings::{Problem, Settings, parse};
use serde::Deserialize;
use serde::de::DeserializeOwned;
use serde_json::{Value, json};

fn settings(fields: Value) -> Settings {
    let json = json!({"settings_version": 2, "fields": fields}).to_string();
    parse(json.as_bytes()).expect("valid settings")
}

fn object(value: Value) -> Document {
    let Value::Object(document) = value else {
        panic!("{value} is not an object");
    };
    document
}

fn environment_problems(err: LoadError) -> Vec<Problem> {
    let LoadError::Environment(problems) = err else {
        panic!("not a problem of the environment: {err}");
    };
    problems
}

#[derive(Deserialize)]
struct Vectors<T> {
    cases: Vec<T>,
}

fn vectors<T: DeserializeOwned>(file: &str) -> Vec<T> {
    let path = Path::new(env!("CARGO_MANIFEST_DIR"))
        .join("../../../testdata/config")
        .join(file);
    let vectors: Vectors<T> =
        serde_json::from_slice(&fs::read(&path).expect("read the vectors")).expect("vectors");
    assert!(!vectors.cases.is_empty(), "{} has no cases", path.display());
    vectors.cases
}

#[derive(Deserialize)]
struct EnvCase {
    name: String,
    fields: Value,
    env: HashMap<String, String>,
    document: Option<Document>,
    problems: Option<Vec<Problem>>,
}

#[test]
fn conversion_follows_the_shared_vectors() {
    for case in vectors::<EnvCase>("env.json") {
        let want = match (case.document, case.problems) {
            (Some(document), None) => Ok(document),
            (None, Some(problems)) => Err(problems),
            _ => panic!("{}: want exactly one of document and problems", case.name),
        };
        let lookup = |name: &str| case.env.get(name).map(OsString::from);

        let got = from_lookup(&settings(case.fields), lookup).map_err(environment_problems);

        assert_eq!(got, want, "{}", case.name);
    }
}

#[test]
fn text_that_is_not_utf8() {
    let field = |secret: bool| {
        settings(
            json!([{"name": "x", "label": "X", "type": "string", "secret": secret, "env": "X"}]),
        )
    };
    let lookup = |bytes: &[u8]| {
        let text = OsString::from_vec(bytes.to_vec());
        move |_: &str| Some(text.clone())
    };

    let err = from_lookup(&field(true), lookup(b"a\xffb")).unwrap_err();
    assert_eq!(
        environment_problems(err),
        [Problem::new("x", "X is not valid UTF-8")]
    );

    let cases: [(&[u8], &str); 3] = [
        (b"a\xffb", "a\u{fffd}b"),
        (b"a\xe2\x82b", "a\u{fffd}b"),
        (b"a\xff\xffb", "a\u{fffd}\u{fffd}b"),
    ];
    for (bytes, text) in cases {
        let got = from_lookup(&field(false), lookup(bytes)).expect("a document");
        assert_eq!(got, object(json!({"x": text})), "{}", bytes.escape_ascii());
    }
}

#[derive(Deserialize)]
struct DefaultsCase {
    name: String,
    fields: Value,
    document: Document,
    want: Document,
}

#[test]
fn defaults_follow_the_shared_vectors() {
    for case in vectors::<DefaultsCase>("defaults.json") {
        let got = with_defaults(&settings(case.fields), case.document);

        assert_eq!(got, case.want, "{}", case.name);
    }
}

fn descriptor(content: &[u8]) -> OwnedFd {
    let mut file = tempfile::tempfile().expect("create a temporary file");
    file.write_all(content).expect("write the document");
    file.rewind().expect("rewind the file");
    file.into()
}

#[test]
fn a_document_is_read_from_a_descriptor() {
    let settings = settings(json!([
        {"name": "n", "label": "N", "type": "integer"},
        {"name": "s", "label": "S", "type": "string", "default": "d"}
    ]));
    let read = |content: &[u8]| from_descriptor(&settings, descriptor(content));

    let mut full = br#"{"n": 7}"#.to_vec();
    full.resize(DOCUMENT_LIMIT, b' ');
    assert_eq!(read(&full).unwrap(), object(json!({"n": 7, "s": "d"})));

    full.push(b' ');
    let err = read(&full).unwrap_err();
    assert!(matches!(err, LoadError::TooLarge), "{err:?}");

    for content in ["[]", "{"] {
        let err = read(content.as_bytes()).unwrap_err();
        assert!(matches!(err, LoadError::NotAnObject), "{content}: {err:?}");
    }
}

#[test]
fn descriptor_numbers() {
    for (text, number) in [("3", 3), ("17", 17)] {
        assert_eq!(descriptor_number(OsStr::new(text)).unwrap(), number);
    }

    for text in ["", "0", "2", "-3", "+3", " 3", "3x", "99999999999"] {
        let err = descriptor_number(OsStr::new(text)).unwrap_err();
        assert!(
            matches!(&err, LoadError::Descriptor(got) if got == text),
            "{text:?}: {err:?}"
        );
    }

    let err = descriptor_number(OsStr::new("3x")).unwrap_err();
    assert_eq!(
        err.to_string(),
        r#"MONITOR_HUB_CONFIG_FD is "3x"; it must be a descriptor number of 3 or more"#
    );
}

#[test]
fn problems_name_their_paths() {
    let problems = vec![
        Problem::new("n[1]", r#"N is not an integer ("x")"#),
        Problem::new("pin", "PIN is not an integer"),
    ];
    let err = LoadError::Environment(problems.clone());
    assert_eq!(err.problems(), problems);
    assert_eq!(
        err.to_string(),
        r#"n[1]: N is not an integer ("x"); pin: PIN is not an integer"#
    );

    assert_eq!(
        LoadError::TooLarge.problems(),
        [Problem::new(
            "",
            "the configuration document is larger than 65536 bytes"
        )]
    );

    assert_eq!(
        LoadError::Read(io::Error::other("bad descriptor")).problems(),
        [Problem::new(
            "",
            "cannot read the configuration document: bad descriptor"
        )]
    );
}
