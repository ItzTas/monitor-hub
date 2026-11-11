use std::error::Error as StdError;
use std::fmt::Debug;
use std::io::{self, Write};
use std::panic::{self, PanicHookInfo};

use chrono::{SecondsFormat, Utc};
use serde_json::{Map, Number, Value};
use thiserror::Error;
use tracing::field::{Field, Visit};
use tracing::level_filters::LevelFilter;
use tracing::{Event, Subscriber};
use tracing_subscriber::layer::{Context, Layer, SubscriberExt};
use tracing_subscriber::registry::Registry;

const MAX_LINE: usize = 64 * 1024;
const MODULE_LIMIT: usize = 1024;
const TRUNCATED: &str = " [truncated]";
const FIELDS_DROPPED: &str = " [truncated, fields dropped]";
const LEVEL: &str = "@level";
const MESSAGE: &str = "@message";
const MODULE: &str = "@module";
const TIMESTAMP: &str = "@timestamp";
const HEAD: [&str; 4] = [LEVEL, MESSAGE, MODULE, TIMESTAMP];
const RESERVED: [&str; 10] = [
    LEVEL, MESSAGE, MODULE, TIMESTAMP, "plugin", "module", "time", "level", "msg", "source",
];

#[derive(Debug, Error, PartialEq, Eq)]
pub enum LoggingError {
    #[error("unknown log level {0:?}: use trace, debug, info, warn or error")]
    UnknownLevel(String),
    #[error("a global tracing subscriber is already installed")]
    AlreadyInitialized,
}

pub fn init(level: &str) -> Result<(), LoggingError> {
    let filter = parse_level(level)?;
    tracing::subscriber::set_global_default(subscriber(filter, write_stderr))
        .map_err(|_| LoggingError::AlreadyInitialized)?;
    panic::set_hook(Box::new(report_panic));
    Ok(())
}

fn parse_level(level: &str) -> Result<LevelFilter, LoggingError> {
    let levels = [
        ("trace", LevelFilter::TRACE),
        ("debug", LevelFilter::DEBUG),
        ("info", LevelFilter::INFO),
        ("warn", LevelFilter::WARN),
        ("error", LevelFilter::ERROR),
    ];
    levels
        .into_iter()
        .find(|(name, _)| name.eq_ignore_ascii_case(level))
        .map(|(_, filter)| filter)
        .ok_or_else(|| LoggingError::UnknownLevel(level.to_owned()))
}

fn subscriber(
    filter: LevelFilter,
    sink: impl Fn(&[u8]) + Send + Sync + 'static,
) -> impl Subscriber + Send + Sync {
    Registry::default().with(filter).with(JsonLines { sink })
}

fn write_stderr(line: &[u8]) {
    let _ = io::stderr().lock().write_all(line);
}

fn report_panic(info: &PanicHookInfo<'_>) {
    let mut fields = Fields {
        message: "panic".into(),
        values: Map::new(),
    };
    let payload = info.payload_as_str().unwrap_or("Box<dyn Any>");
    fields.values.insert("panic".into(), payload.into());
    if let Some(location) = info.location() {
        fields
            .values
            .insert("location".into(), location.to_string().into());
    }
    write_stderr(&encode("error", "panic", fields));
}

struct JsonLines<F> {
    sink: F,
}

impl<S, F> Layer<S> for JsonLines<F>
where
    S: Subscriber,
    F: Fn(&[u8]) + Send + Sync + 'static,
{
    fn on_event(&self, event: &Event<'_>, _: Context<'_, S>) {
        let metadata = event.metadata();
        let mut fields = Fields::default();
        event.record(&mut fields);
        let level = metadata.level().as_str().to_ascii_lowercase();
        (self.sink)(&encode(&level, metadata.target(), fields));
    }
}

#[derive(Default)]
struct Fields {
    message: String,
    values: Map<String, Value>,
}

impl Fields {
    fn add(&mut self, field: &Field, value: Value) {
        if field.name() == "message" {
            self.message = match value {
                Value::String(text) => text,
                other => other.to_string(),
            };
            return;
        }
        let mut key = field.name().to_owned();
        while RESERVED.contains(&key.as_str()) || self.values.contains_key(&key) {
            key.insert_str(0, "field_");
        }
        self.values.insert(key, value);
    }
}

impl Visit for Fields {
    fn record_debug(&mut self, field: &Field, value: &dyn Debug) {
        self.add(field, format!("{value:?}").into());
    }

    fn record_str(&mut self, field: &Field, value: &str) {
        self.add(field, value.into());
    }

    fn record_i64(&mut self, field: &Field, value: i64) {
        self.add(field, value.into());
    }

    fn record_u64(&mut self, field: &Field, value: u64) {
        self.add(field, value.into());
    }

    fn record_i128(&mut self, field: &Field, value: i128) {
        self.add(field, number_or_text(Number::from_i128(value), value));
    }

    fn record_u128(&mut self, field: &Field, value: u128) {
        self.add(field, number_or_text(Number::from_u128(value), value));
    }

    fn record_f64(&mut self, field: &Field, value: f64) {
        self.add(field, number_or_text(Number::from_f64(value), value));
    }

    fn record_bool(&mut self, field: &Field, value: bool) {
        self.add(field, value.into());
    }

    fn record_error(&mut self, field: &Field, value: &(dyn StdError + 'static)) {
        self.add(field, value.to_string().into());
    }
}

fn number_or_text(number: Option<Number>, value: impl ToString) -> Value {
    number.map_or_else(|| value.to_string().into(), Value::Number)
}

fn encode(level: &str, module: &str, fields: Fields) -> Vec<u8> {
    let Fields {
        message,
        values: mut object,
    } = fields;
    let timestamp = Utc::now().to_rfc3339_opts(SecondsFormat::Micros, true);
    object.insert(LEVEL.into(), level.into());
    object.insert(MODULE.into(), module.into());
    object.insert(TIMESTAMP.into(), timestamp.into());
    object.insert(MESSAGE.into(), message.as_str().into());
    let line = json(&object);
    if line.len() < MAX_LINE {
        return terminated(line);
    }
    if let Some(line) = with_cut_message(&mut object, &message, TRUNCATED) {
        return terminated(line);
    }
    object.retain(|key, _| HEAD.contains(&key.as_str()));
    let module = &module[..module.floor_char_boundary(MODULE_LIMIT)];
    object.insert(MODULE.into(), module.into());
    terminated(with_cut_message(&mut object, &message, FIELDS_DROPPED).unwrap_or_default())
}

fn with_cut_message(
    object: &mut Map<String, Value>,
    message: &str,
    suffix: &str,
) -> Option<Vec<u8>> {
    object.insert(MESSAGE.into(), "".into());
    let rest = json(object).len().saturating_sub(token_len(""));
    let cut = cut_to_fit(message, suffix, (MAX_LINE - 1).checked_sub(rest)?)?;
    object.insert(MESSAGE.into(), cut.into());
    Some(json(object))
}

fn cut_to_fit(text: &str, suffix: &str, budget: usize) -> Option<String> {
    let candidate = |end: usize| format!("{}{suffix}", &text[..text.floor_char_boundary(end)]);
    let fits = |end: usize| token_len(&candidate(end)) <= budget;
    if !fits(0) {
        return None;
    }
    let (mut low, mut high) = (0, text.len());
    while low < high {
        let middle = high - (high - low) / 2;
        if fits(middle) {
            low = middle;
        } else {
            high = middle - 1;
        }
    }
    Some(candidate(low))
}

fn token_len(text: &str) -> usize {
    serde_json::to_string(text).map_or(usize::MAX, |token| token.len())
}

fn json(object: &Map<String, Value>) -> Vec<u8> {
    serde_json::to_vec(object).unwrap_or_default()
}

fn terminated(mut line: Vec<u8>) -> Vec<u8> {
    line.push(b'\n');
    line
}

#[cfg(test)]
mod tests {
    use std::sync::{Arc, Mutex};

    use serde_json::{Map, Value, json};
    use tracing::level_filters::LevelFilter;

    use super::{MAX_LINE, parse_level, subscriber};

    fn capture(filter: LevelFilter, emit: impl FnOnce()) -> Vec<String> {
        let sink = Arc::new(Mutex::new(Vec::new()));
        let writer = Arc::clone(&sink);
        let subscriber = subscriber(filter, move |line: &[u8]| {
            writer.lock().expect("sink lock").extend_from_slice(line)
        });
        tracing::subscriber::with_default(subscriber, emit);
        let bytes = sink.lock().expect("sink lock").clone();
        let text = String::from_utf8(bytes).expect("UTF-8 output");
        assert!(text.ends_with('\n'), "output does not end a line: {text:?}");
        text.lines().map(str::to_owned).collect()
    }

    fn object(line: &str) -> Map<String, Value> {
        match serde_json::from_str(line) {
            Ok(Value::Object(object)) => object,
            _ => panic!("not a JSON object: {line:?}"),
        }
    }

    fn one_event(emit: impl FnOnce()) -> (String, Map<String, Value>) {
        let lines = capture(LevelFilter::TRACE, emit);
        assert_eq!(lines.len(), 1, "lines: {lines:?}");
        let line = lines[0].clone();
        let parsed = object(&line);
        (line, parsed)
    }

    #[test]
    fn a_message_with_a_newline_and_a_quote_is_one_json_line() {
        let message = "first line\nsaid \"hi\"\r\t\u{0}\u{1b}[31m done";

        let (_, line) = one_event(|| tracing::warn!(target: "demo", "{message}"));

        assert_eq!(line["@level"], "warn");
        assert_eq!(line["@message"], message);
        assert_eq!(line["@module"], "demo");
    }

    #[test]
    fn fields_keep_their_json_types() {
        let (_, line) = one_event(|| {
            tracing::info!(
                count = 3_i64,
                big = u64::MAX,
                wide = i128::from(i64::MIN) - 1,
                ratio = 0.5,
                not_a_number = f64::NAN,
                ready = true,
                name = "rust",
                list = ?vec![1, 2],
                shown = %"displayed",
                "typed"
            )
        });

        assert_eq!(line["count"], json!(3));
        assert_eq!(line["big"], json!(u64::MAX));
        assert_eq!(line["wide"], json!("-9223372036854775809"));
        assert_eq!(line["ratio"], json!(0.5));
        assert_eq!(line["not_a_number"], json!("NaN"));
        assert_eq!(line["ready"], json!(true));
        assert_eq!(line["name"], json!("rust"));
        assert_eq!(line["list"], json!("[1, 2]"));
        assert_eq!(line["shown"], json!("displayed"));
    }

    #[test]
    fn fields_named_like_reserved_keys_are_prefixed() {
        let (_, line) = one_event(|| {
            tracing::error!(
                target: "demo",
                {
                    "@level" = "fake",
                    "@message" = 1,
                    "@module" = 2,
                    "@timestamp" = 3,
                    plugin = "other",
                    module = 4,
                    time = 5,
                    level = 6,
                    msg = 7,
                    source = 8,
                    field_plugin = 9,
                },
                "real"
            )
        });

        let want = json!({
            "@level": "error",
            "@message": "real",
            "@module": "demo",
            "@timestamp": line["@timestamp"],
            "field_@level": "fake",
            "field_@message": 1,
            "field_@module": 2,
            "field_@timestamp": 3,
            "field_plugin": "other",
            "field_field_plugin": 9,
            "field_module": 4,
            "field_time": 5,
            "field_level": 6,
            "field_msg": 7,
            "field_source": 8,
        });
        assert_eq!(Value::Object(line), want);
    }

    #[test]
    fn an_over_long_message_is_cut_to_fit_the_line_limit() {
        let message = "\"é\n".repeat(MAX_LINE);

        let (raw, line) = one_event(|| tracing::info!(answer = 42, "{message}"));

        assert!(raw.len() < MAX_LINE, "line of {} bytes", raw.len());
        assert_eq!(line["@level"], "info");
        assert_eq!(line["answer"], 42);
        let cut = line["@message"].as_str().expect("message");
        let kept = cut.strip_suffix(" [truncated]").expect("truncation mark");
        assert!(message.starts_with(kept));
        assert!(
            raw.len() > MAX_LINE - 16,
            "cut too much: {} bytes",
            raw.len()
        );
    }

    #[test]
    fn over_long_fields_are_dropped_to_fit_the_line_limit() {
        let huge = "x".repeat(2 * MAX_LINE);

        let (raw, line) = one_event(|| tracing::warn!(target: "demo", huge, small = 1, "short"));

        assert!(raw.len() < MAX_LINE, "line of {} bytes", raw.len());
        let want = json!({
            "@level": "warn",
            "@message": "short [truncated, fields dropped]",
            "@module": "demo",
            "@timestamp": line["@timestamp"],
        });
        assert_eq!(Value::Object(line), want);
    }

    #[test]
    fn events_below_the_level_are_not_emitted() {
        let lines = capture(LevelFilter::WARN, || {
            tracing::info!("hidden");
            tracing::warn!("shown");
            tracing::error!("shown too");
        });

        let messages: Vec<Value> = lines
            .iter()
            .map(|line| object(line)["@message"].clone())
            .collect();
        assert_eq!(messages, [json!("shown"), json!("shown too")]);
    }

    #[test]
    fn level_names_ignore_case() {
        assert_eq!(parse_level("WaRn"), Ok(LevelFilter::WARN));
        assert_eq!(parse_level("TRACE"), Ok(LevelFilter::TRACE));
    }
}
