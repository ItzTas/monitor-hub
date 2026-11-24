use super::{FieldType, Settings};

impl Settings {
    /// Whether path names a value of a document that follows these settings, such as
    /// `rules[2].process`. The empty path names the whole document.
    pub fn declares(&self, path: &str) -> bool {
        if path.is_empty() {
            return true;
        }
        let mut fields = self.fields.as_slice();
        let mut path = path;
        loop {
            let (name, mut rest) = path.split_at(path.find(['.', '[']).unwrap_or(path.len()));
            let Some(field) = fields.iter().find(|field| field.name == name) else {
                return false;
            };
            let mut children = field.fields.as_deref();
            if field.kind == FieldType::List && rest.starts_with('[') {
                let Some(after) = cut_index(rest) else {
                    return false;
                };
                rest = after;
                children = field.items.as_ref().and_then(|item| item.fields.as_deref());
            }
            if rest.is_empty() {
                return true;
            }
            // Only objects and items of objects have fields, so a name after anything else is not found.
            let Some(next) = rest.strip_prefix('.') else {
                return false;
            };
            fields = children.unwrap_or_default();
            path = next;
        }
    }
}

// Removes a leading [index] from path, where index is 0 or digits without a leading zero.
fn cut_index(path: &str) -> Option<&str> {
    let (digits, rest) = path.strip_prefix('[')?.split_once(']')?;
    let leading_zero = digits.len() > 1 && digits.starts_with('0');
    let number = !digits.is_empty() && digits.bytes().all(|b| b.is_ascii_digit());
    (number && !leading_zero).then_some(rest)
}
