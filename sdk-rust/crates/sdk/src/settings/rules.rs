use serde_json::Value;

use super::{Field, FieldType, MAX_DEPTH, is_reserved, parse_duration};

#[derive(Debug, Clone, PartialEq, Eq, thiserror::Error)]
#[non_exhaustive]
pub enum FieldProblem {
    #[error("name {0:?} does not match ^[a-z][a-z0-9_]*$")]
    InvalidName(String),
    #[error("name {0:?} is declared more than once")]
    DuplicateName(String),
    #[error("label is empty")]
    EmptyLabel,
    #[error("a choice needs at least one option")]
    MissingOptions,
    #[error("options are only for choice")]
    UnexpectedOptions,
    #[error("an option is empty")]
    EmptyOption,
    #[error("option {0:?} is listed more than once")]
    DuplicateOption(String),
    #[error("an object needs at least one field")]
    MissingFields,
    #[error("fields are only for object")]
    UnexpectedFields,
    #[error("a list needs items")]
    MissingItems,
    #[error("items are only for list")]
    UnexpectedItems,
    #[error("a list cannot hold lists")]
    ListOfLists,
    #[error("fields nest more than {max} levels deep", max = MAX_DEPTH)]
    TooDeep,
    #[error("an object or a list of objects cannot have a default")]
    DefaultOnContainer,
    #[error("default does not fit the field type")]
    DefaultType,
    #[error("default {0:?} is not one of the options")]
    DefaultNotAnOption(String),
    #[error("default {0:?} is not a duration such as 15m")]
    DefaultNotADuration(String),
    #[error("a secret field cannot have a default")]
    SecretDefault,
    #[error("an object or a list cannot be secret")]
    SecretContainer,
    #[error("a field inside a list item cannot be secret")]
    SecretInList,
    #[error("editable: false needs a non-empty reason")]
    MissingReason,
    #[error("reason is only for fields with editable: false")]
    UnexpectedReason,
    #[error("editable: false needs env")]
    NotEditableWithoutEnv,
    #[error("{0} is only for top-level fields")]
    TopLevelOnly(&'static str),
    #[error("env {0:?} does not match ^[A-Z][A-Z0-9_]*$")]
    EnvName(String),
    #[error("env {0:?} is reserved by the hub")]
    EnvReserved(String),
    #[error("env {0:?} is used by more than one field")]
    EnvDuplicate(String),
    #[error("env is only for fields of a simple type, choice, or a list of those")]
    EnvType,
    #[error("group is empty")]
    EmptyGroup,
}

type Located = (String, FieldProblem);

// The fields depth first, in the rule order of sdk-go/settings, up to the first problem.
pub(super) fn check(fields: &[Field]) -> Result<(), Located> {
    walk(fields, "", 1, false)
}

fn walk(fields: &[Field], parent: &str, level: usize, in_list: bool) -> Result<(), Located> {
    for (index, field) in fields.iter().enumerate() {
        let place = Place {
            field,
            earlier: &fields[..index],
            path: join(parent, &format!("fields[{index}]")),
            level,
            in_list,
        };
        place.check()?;
        // The field passed its rules, so only an object has fields and only a list has items.
        if let Some(children) = field.fields.as_deref() {
            walk(children, &place.path, level + 1, in_list)?;
        }
        if let Some(children) = field.items.as_ref().and_then(|item| item.fields.as_deref()) {
            walk(children, &join(&place.path, "items"), level + 1, true)?;
        }
    }
    Ok(())
}

fn join(parent: &str, child: &str) -> String {
    if parent.is_empty() {
        return child.to_owned();
    }
    format!("{parent}.{child}")
}

fn refuse_at(
    path: &str,
    broken: bool,
    problem: impl FnOnce() -> FieldProblem,
) -> Result<(), Located> {
    if broken {
        return Err((path.to_owned(), problem()));
    }
    Ok(())
}

struct Place<'a> {
    field: &'a Field,
    earlier: &'a [Field],
    path: String,
    level: usize,
    in_list: bool,
}

impl Place<'_> {
    // Each rule counts on the ones before it having passed.
    fn check(&self) -> Result<(), Located> {
        let rules = [
            Self::name,
            Self::label,
            Self::type_attributes,
            Self::depth,
            Self::default,
            Self::secret,
            Self::editable,
            Self::top_level_only,
            Self::env,
            Self::group,
        ];
        rules.iter().try_for_each(|rule| rule(self))
    }

    fn refuse(&self, broken: bool, problem: impl FnOnce() -> FieldProblem) -> Result<(), Located> {
        refuse_at(&self.path, broken, problem)
    }

    fn fail(&self, problem: FieldProblem) -> Result<(), Located> {
        Err((self.path.clone(), problem))
    }

    fn name(&self) -> Result<(), Located> {
        let name = &self.field.name;
        self.refuse(!is_identifier(name, u8::is_ascii_lowercase), || {
            FieldProblem::InvalidName(name.clone())
        })?;
        self.refuse(
            self.earlier.iter().any(|earlier| earlier.name == *name),
            || FieldProblem::DuplicateName(name.clone()),
        )
    }

    fn label(&self) -> Result<(), Located> {
        self.refuse(self.field.label.trim().is_empty(), || {
            FieldProblem::EmptyLabel
        })
    }

    fn type_attributes(&self) -> Result<(), Located> {
        let field = self.field;
        members(
            &self.path,
            field.kind,
            field.options.as_deref(),
            field.fields.as_deref(),
        )?;
        let item = match (field.kind, field.items.as_deref()) {
            (FieldType::List, Some(item)) => item,
            (FieldType::List, None) => return self.fail(FieldProblem::MissingItems),
            (_, items) => return self.refuse(items.is_some(), || FieldProblem::UnexpectedItems),
        };
        let path = join(&self.path, "items");
        refuse_at(&path, item.kind == FieldType::List, || {
            FieldProblem::ListOfLists
        })?;
        members(
            &path,
            item.kind,
            item.options.as_deref(),
            item.fields.as_deref(),
        )
    }

    fn depth(&self) -> Result<(), Located> {
        self.refuse(self.level > MAX_DEPTH, || FieldProblem::TooDeep)
    }

    fn default(&self) -> Result<(), Located> {
        let field = self.field;
        let Some(default) = &field.default else {
            return Ok(());
        };
        self.refuse(holds_objects(field), || FieldProblem::DefaultOnContainer)?;
        let Some(item) = &field.items else {
            return self.value(field.kind, field.options.as_deref(), default);
        };
        let Some(values) = default.as_array() else {
            return self.fail(FieldProblem::DefaultType);
        };
        values
            .iter()
            .try_for_each(|value| self.value(item.kind, item.options.as_deref(), value))
    }

    // One default, or one value of a list default, against the type kind.
    fn value(
        &self,
        kind: FieldType,
        options: Option<&[String]>,
        value: &Value,
    ) -> Result<(), Located> {
        let fits = match kind {
            // serde_json reads -0 as a float, so it is refused here as in sdk-go/settings.
            FieldType::Integer => value.as_i64().is_some(),
            FieldType::Number => value.is_number(),
            FieldType::Boolean => value.is_boolean(),
            _ => value.is_string(),
        };
        self.refuse(!fits, || FieldProblem::DefaultType)?;
        let Some(text) = value.as_str() else {
            return Ok(());
        };
        let option = options
            .unwrap_or_default()
            .iter()
            .any(|option| option == text);
        self.refuse(kind == FieldType::Choice && !option, || {
            FieldProblem::DefaultNotAnOption(text.to_owned())
        })?;
        self.refuse(
            kind == FieldType::Duration && parse_duration(text).is_none(),
            || FieldProblem::DefaultNotADuration(text.to_owned()),
        )
    }

    fn secret(&self) -> Result<(), Located> {
        let field = self.field;
        if !field.secret {
            return Ok(());
        }
        self.refuse(field.default.is_some(), || FieldProblem::SecretDefault)?;
        self.refuse(
            matches!(field.kind, FieldType::Object | FieldType::List),
            || FieldProblem::SecretContainer,
        )?;
        self.refuse(self.in_list, || FieldProblem::SecretInList)
    }

    fn editable(&self) -> Result<(), Located> {
        let field = self.field;
        if field.is_editable() {
            return self.refuse(field.reason.is_some(), || FieldProblem::UnexpectedReason);
        }
        let blank = field
            .reason
            .as_deref()
            .is_none_or(|reason| reason.trim().is_empty());
        self.refuse(blank, || FieldProblem::MissingReason)?;
        self.refuse(field.env.is_none(), || FieldProblem::NotEditableWithoutEnv)
    }

    fn top_level_only(&self) -> Result<(), Located> {
        if self.level == 1 {
            return Ok(());
        }
        let field = self.field;
        let written = [
            ("env", field.env.is_some()),
            ("group", field.group.is_some()),
            ("editable", field.editable.is_some()),
        ];
        written.into_iter().try_for_each(|(attribute, present)| {
            self.refuse(present, || FieldProblem::TopLevelOnly(attribute))
        })
    }

    fn env(&self) -> Result<(), Located> {
        let Some(env) = self.field.env.as_deref() else {
            return Ok(());
        };
        self.refuse(!is_identifier(env, u8::is_ascii_uppercase), || {
            FieldProblem::EnvName(env.to_owned())
        })?;
        self.refuse(is_reserved(env), || {
            FieldProblem::EnvReserved(env.to_owned())
        })?;
        self.refuse(
            self.earlier
                .iter()
                .any(|earlier| earlier.env.as_deref() == Some(env)),
            || FieldProblem::EnvDuplicate(env.to_owned()),
        )?;
        self.refuse(holds_objects(self.field), || FieldProblem::EnvType)
    }

    fn group(&self) -> Result<(), Located> {
        let blank = self
            .field
            .group
            .as_deref()
            .is_some_and(|group| group.trim().is_empty());
        self.refuse(blank, || FieldProblem::EmptyGroup)
    }
}

// The options and fields of the field or item at path, against its type kind.
fn members(
    path: &str,
    kind: FieldType,
    options: Option<&[String]>,
    fields: Option<&[Field]>,
) -> Result<(), Located> {
    check_options(path, kind, options)?;
    if kind == FieldType::Object {
        return refuse_at(path, fields.is_none_or(<[Field]>::is_empty), || {
            FieldProblem::MissingFields
        });
    }
    refuse_at(path, fields.is_some(), || FieldProblem::UnexpectedFields)
}

fn check_options(path: &str, kind: FieldType, options: Option<&[String]>) -> Result<(), Located> {
    if kind != FieldType::Choice {
        return refuse_at(path, options.is_some(), || FieldProblem::UnexpectedOptions);
    }
    let options = options.unwrap_or_default();
    refuse_at(path, options.is_empty(), || FieldProblem::MissingOptions)?;
    for (index, option) in options.iter().enumerate() {
        refuse_at(path, option.trim().is_empty(), || FieldProblem::EmptyOption)?;
        refuse_at(path, options[..index].contains(option), || {
            FieldProblem::DuplicateOption(option.clone())
        })?;
    }
    Ok(())
}

// The field passed type_attributes, so only a list has items.
fn holds_objects(field: &Field) -> bool {
    field.kind == FieldType::Object
        || field
            .items
            .as_ref()
            .is_some_and(|item| item.kind == FieldType::Object)
}

// A letter of the given case, then letters of that case, digits or underscores.
fn is_identifier(text: &str, letter: fn(&u8) -> bool) -> bool {
    let mut bytes = text.bytes();
    bytes.next().is_some_and(|first| letter(&first))
        && bytes.all(|b| letter(&b) || b.is_ascii_digit() || b == b'_')
}
