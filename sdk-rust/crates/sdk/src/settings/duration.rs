const LIMIT: u64 = 1 << 63;
const SECOND: u64 = 1_000_000_000;

/// Nanoseconds from Go's time.ParseDuration syntax, such as 15m or 1h30m; None when the text
/// is not in that syntax or does not fit in i64.
pub fn parse_duration(text: &str) -> Option<i64> {
    let (negative, mut rest) = match text.as_bytes().first() {
        Some(b'-') => (true, &text[1..]),
        Some(b'+') => (false, &text[1..]),
        _ => (false, text),
    };
    if rest == "0" {
        return Some(0);
    }
    if rest.is_empty() {
        return None;
    }
    let mut total: u64 = 0;
    while !rest.is_empty() {
        let (value, remaining) = component(rest)?;
        total = total.wrapping_add(value);
        if total > LIMIT {
            return None;
        }
        rest = remaining;
    }
    if negative {
        return Some((total as i64).wrapping_neg());
    }
    i64::try_from(total).ok()
}

fn component(text: &str) -> Option<(u64, &str)> {
    let first = text.as_bytes()[0];
    if first != b'.' && !first.is_ascii_digit() {
        return None;
    }
    let (mut value, rest) = leading_int(text)?;
    let whole = rest.len() != text.len();
    let (fraction, scale, decimals, rest) = match rest.strip_prefix('.') {
        Some(digits) => leading_fraction(digits),
        None => (0, 1.0, false, rest),
    };
    if !whole && !decimals {
        return None;
    }
    let length = rest
        .bytes()
        .position(|c| c == b'.' || c.is_ascii_digit())
        .unwrap_or(rest.len());
    if length == 0 {
        return None;
    }
    let (unit, rest) = rest.split_at(length);
    let unit = unit_nanos(unit)?;
    if value > LIMIT / unit {
        return None;
    }
    value *= unit;
    if fraction > 0 {
        value += (fraction as f64 * (unit as f64 / scale)) as u64;
        if value > LIMIT {
            return None;
        }
    }
    Some((value, rest))
}

fn unit_nanos(unit: &str) -> Option<u64> {
    const UNITS: [(&str, u64); 8] = [
        ("ns", 1),
        ("us", 1_000),
        ("µs", 1_000),
        ("μs", 1_000),
        ("ms", 1_000_000),
        ("s", SECOND),
        ("m", 60 * SECOND),
        ("h", 3_600 * SECOND),
    ];
    UNITS
        .iter()
        .find(|(name, _)| *name == unit)
        .map(|(_, nanos)| *nanos)
}

fn leading_int(text: &str) -> Option<(u64, &str)> {
    let digits = text.bytes().take_while(u8::is_ascii_digit).count();
    let mut value: u64 = 0;
    for c in text[..digits].bytes() {
        if value > LIMIT / 10 {
            return None;
        }
        value = value * 10 + u64::from(c - b'0');
        if value > LIMIT {
            return None;
        }
    }
    Some((value, &text[digits..]))
}

fn leading_fraction(text: &str) -> (u64, f64, bool, &str) {
    let digits = text.bytes().take_while(u8::is_ascii_digit).count();
    let mut value: u64 = 0;
    let mut scale = 1.0;
    for c in text[..digits].bytes() {
        if value > (LIMIT - 1) / 10 {
            break;
        }
        let next = value * 10 + u64::from(c - b'0');
        if next > LIMIT {
            break;
        }
        value = next;
        scale *= 10.0;
    }
    (value, scale, digits > 0, &text[digits..])
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parse_wraps_the_sum_like_go() {
        assert_eq!(
            parse_duration("9223372036854775808ns9223372036854775808ns"),
            Some(0)
        );
    }
}
