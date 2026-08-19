use serde_json::Value;

#[derive(Clone, Debug, Default)]
struct Step {
    key: Option<String>,
    bracket: Bracket,
}

#[derive(Clone, Debug, Default)]
enum Bracket {
    #[default]
    None,
    All,
    Index(usize),
    Predicate {
        key: String,
        negated: bool,
    },
}

pub fn select(data: &Value, path: &str) -> Vec<String> {
    let steps = parse_path(path);
    if steps.is_empty() {
        return stringify(data);
    }
    let mut current = vec![data];
    for step in &steps {
        let mut next = Vec::new();
        for node in current {
            next.extend(apply(node, step));
        }
        if next.is_empty() {
            return Vec::new();
        }
        current = next;
    }
    current.into_iter().flat_map(stringify).collect()
}

pub fn select_first(data: &Value, paths: &[String]) -> Vec<String> {
    for path in paths {
        let selected = select(data, path);
        if !selected.is_empty() {
            return selected;
        }
    }
    Vec::new()
}

pub fn truthy(data: &Value, path: &str) -> bool {
    !path.is_empty()
        && select(data, path).into_iter().any(|value| {
            !matches!(
                value.to_ascii_lowercase().as_str(),
                "" | "false" | "0" | "null"
            )
        })
}

pub fn matches(data: &Value, condition: &str) -> bool {
    let condition = condition.trim();
    if condition.is_empty() {
        return true;
    }
    for operator in ["==", "!="] {
        if let Some(index) = condition.find(operator) {
            let path = condition[..index].trim();
            let wanted = condition[index + operator.len()..]
                .trim()
                .trim_matches(['\'', '"']);
            let found = select(data, path).iter().any(|value| value == wanted);
            return if operator == "!=" { !found } else { found };
        }
    }
    truthy(data, condition)
}

fn apply<'a>(node: &'a Value, step: &Step) -> Vec<&'a Value> {
    let node = match &step.key {
        Some(key) => match node.get(key) {
            Some(value) => value,
            None => return Vec::new(),
        },
        None => node,
    };
    match &step.bracket {
        Bracket::None => vec![node],
        Bracket::All => node
            .as_array()
            .map_or_else(Vec::new, |items| items.iter().collect()),
        Bracket::Index(index) => node.get(*index).into_iter().collect(),
        Bracket::Predicate { key, negated } => node.as_array().map_or_else(Vec::new, |items| {
            items
                .iter()
                .filter(|item| field_truthy(item, key) != *negated)
                .collect()
        }),
    }
}

fn field_truthy(node: &Value, key: &str) -> bool {
    node.get(key).is_some_and(value_truthy)
}

fn value_truthy(value: &Value) -> bool {
    match value {
        Value::Null => false,
        Value::Bool(value) => *value,
        Value::String(value) => !value.is_empty() && value != "false",
        Value::Number(value) => value.as_f64() != Some(0.0),
        _ => true,
    }
}

fn parse_path(path: &str) -> Vec<Step> {
    let path = path.trim().strip_prefix('$').unwrap_or(path.trim());
    let path = path.strip_prefix('.').unwrap_or(path);
    if path.is_empty() {
        return Vec::new();
    }
    let mut output = Vec::new();
    for segment in path.split('.').filter(|segment| !segment.is_empty()) {
        let mut name = segment.to_owned();
        let mut brackets = Vec::new();
        while let Some(start) = name.find('[') {
            let Some(relative_end) = name[start..].find(']') else {
                break;
            };
            let end = start + relative_end;
            brackets.push(name[start + 1..end].to_owned());
            name.replace_range(start..=end, "");
        }
        if brackets.is_empty() {
            output.push(Step {
                key: (!name.is_empty()).then_some(name),
                bracket: Bracket::None,
            });
            continue;
        }
        for (index, bracket) in brackets.into_iter().enumerate() {
            let key = (index == 0 && !name.is_empty()).then(|| name.clone());
            let bracket = match bracket.as_str() {
                "*" => Bracket::All,
                value if value.starts_with('!') => Bracket::Predicate {
                    key: value[1..].to_owned(),
                    negated: true,
                },
                value => match value.parse::<usize>() {
                    Ok(index) => Bracket::Index(index),
                    Err(_) => Bracket::Predicate {
                        key: value.to_owned(),
                        negated: false,
                    },
                },
            };
            output.push(Step { key, bracket });
        }
    }
    output
}

fn stringify(value: &Value) -> Vec<String> {
    match value {
        Value::Null => Vec::new(),
        Value::String(value) if value.is_empty() => Vec::new(),
        Value::String(value) => vec![value.clone()],
        Value::Number(value) => vec![value.to_string()],
        Value::Bool(value) => vec![value.to_string()],
        Value::Array(values) => values.iter().flat_map(stringify).collect(),
        value => serde_json::to_string(value).into_iter().collect(),
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    #[test]
    fn selector_parity() {
        let document = json!({
            "content": {"parts": [
                {"text": "thinking", "thought": true},
                {"text": "answer"}
            ]},
            "choices": [{"delta": {"content": "hi"}}],
            "n": 42
        });
        assert_eq!(
            select(&document, "$.content.parts[*].text"),
            ["thinking", "answer"]
        );
        assert_eq!(
            select(&document, "$.content.parts[!thought].text"),
            ["answer"]
        );
        assert_eq!(
            select(&document, "$.content.parts[thought].text"),
            ["thinking"]
        );
        assert_eq!(select(&document, "$.choices[0].delta.content"), ["hi"]);
        assert_eq!(select(&document, "$.n"), ["42"]);
        assert!(select(&document, "$.missing").is_empty());
    }

    #[test]
    fn condition_parity() {
        let text = json!({"type": "TEXT_MESSAGE_CONTENT", "partial": true});
        assert!(matches(&text, "$.type == TEXT_MESSAGE_CONTENT"));
        assert!(matches(&text, "$.type != REASONING_MESSAGE_CONTENT"));
        assert!(matches(&text, "$.partial"));
        assert!(!matches(&text, "$.missing"));
    }
}
