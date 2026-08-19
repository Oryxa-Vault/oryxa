use std::collections::{BTreeMap, BTreeSet};

use serde_json::{Map, Value};

use super::Step;

#[derive(Clone, Debug, Default)]
pub struct ContextView {
    pub all: String,
    pub pinned: String,
    pub keys: BTreeMap<String, String>,
}

#[derive(Clone, Debug, Default)]
pub struct RenderContext {
    pub input: String,
    pub turn: String,
    pub conversation: String,
    pub handle: String,
    pub agent: String,
    pub workspace: String,
    pub callback_url: String,
    pub callback_token: String,
    pub vars: BTreeMap<String, String>,
    pub captures: BTreeMap<String, String>,
    pub context: ContextView,
}

impl RenderContext {
    fn lookup(&self, name: &str) -> Option<String> {
        match name {
            "input" => Some(self.input.clone()),
            "turn" => Some(self.turn.clone()),
            "conversation" => Some(self.conversation.clone()),
            "agent" => Some(self.agent.clone()),
            "workspace" => Some(if self.workspace.is_empty() {
                std::env::var("ORYXA_WORKSPACE")
                    .ok()
                    .filter(|value| !value.trim().is_empty())
                    .or_else(|| {
                        std::env::current_dir()
                            .ok()
                            .map(|path| path.to_string_lossy().into_owned())
                    })
                    .unwrap_or_default()
            } else {
                self.workspace.clone()
            }),
            "handle" => Some(
                (!self.handle.is_empty())
                    .then(|| self.handle.clone())
                    .or_else(|| {
                        self.captures
                            .get("handle")
                            .filter(|value| !value.is_empty())
                            .cloned()
                    })
                    .unwrap_or_else(|| self.conversation.clone()),
            ),
            "callback_url" => Some(self.callback_url.clone()),
            "callback_token" => Some(self.callback_token.clone()),
            "context" => Some(self.context.all.clone()),
            "context.pinned" => Some(self.context.pinned.clone()),
            name if name.starts_with("context.") => Some(
                self.context
                    .keys
                    .get(&name["context.".len()..])
                    .cloned()
                    .unwrap_or_default(),
            ),
            name if name.starts_with("vars.") => self.vars.get(&name["vars.".len()..]).cloned(),
            name if name.starts_with("env.") => {
                Some(std::env::var(&name["env.".len()..]).unwrap_or_else(|_| {
                    match &name["env.".len()..] {
                        "ORYXA_AGENT_HOST" | "ORYXA_SHIM_HOST" => "localhost".into(),
                        _ => String::new(),
                    }
                }))
            }
            name => self
                .captures
                .get(name)
                .or_else(|| self.vars.get(name))
                .cloned(),
        }
    }

    pub fn render_string(&self, mut input: &str) -> String {
        let mut output = String::new();
        loop {
            let Some(start) = input.find("{{") else {
                output.push_str(input);
                return output;
            };
            let Some(relative_end) = input[start..].find("}}") else {
                output.push_str(input);
                return output;
            };
            let end = start + relative_end;
            output.push_str(&input[..start]);
            let name = input[start + 2..end].trim();
            match self.lookup(name) {
                Some(value) => output.push_str(&value),
                None => output.push_str(&input[start..end + 2]),
            }
            input = &input[end + 2..];
        }
    }

    pub fn render(&self, value: &Value) -> Value {
        match value {
            Value::String(value) => Value::String(self.render_string(value)),
            Value::Array(values) => {
                Value::Array(values.iter().map(|value| self.render(value)).collect())
            }
            Value::Object(values) => Value::Object(
                values
                    .iter()
                    .map(|(key, value)| (self.render_string(key), self.render(value)))
                    .collect::<Map<_, _>>(),
            ),
            value => value.clone(),
        }
    }

    pub fn render_headers(&self, headers: &BTreeMap<String, String>) -> BTreeMap<String, String> {
        headers
            .iter()
            .map(|(key, value)| (self.render_string(key), self.render_string(value)))
            .collect()
    }
}

impl Step {
    pub fn context_refs(&self) -> Vec<String> {
        let mut refs = Vec::new();
        collect_refs(&self.path, &mut refs);
        for (key, value) in &self.headers {
            collect_refs(key, &mut refs);
            collect_refs(value, &mut refs);
        }
        if let Some(body) = &self.body {
            walk_strings(body, &mut |value| collect_refs(value, &mut refs));
        }
        refs
    }
}

fn collect_refs(input: &str, output: &mut Vec<String>) {
    let mut remaining = input;
    loop {
        let Some(start) = remaining.find("{{") else {
            return;
        };
        let Some(relative_end) = remaining[start..].find("}}") else {
            return;
        };
        let end = start + relative_end;
        let name = remaining[start + 2..end].trim();
        if name == "context" || name.starts_with("context.") {
            output.push(name.to_owned());
        }
        remaining = &remaining[end + 2..];
    }
}

fn walk_strings(value: &Value, visit: &mut impl FnMut(&str)) {
    match value {
        Value::String(value) => visit(value),
        Value::Array(values) => values.iter().for_each(|value| walk_strings(value, visit)),
        Value::Object(values) => values.iter().for_each(|(key, value)| {
            visit(key);
            walk_strings(value, visit);
        }),
        _ => {}
    }
}

#[allow(dead_code)]
fn distinct(values: impl IntoIterator<Item = String>) -> Vec<String> {
    let mut seen = BTreeSet::new();
    values
        .into_iter()
        .filter(|value| seen.insert(value.clone()))
        .collect()
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    #[test]
    fn renders_nested_values_and_keeps_unknowns() {
        let context = RenderContext {
            input: "hello".into(),
            conversation: "s_1".into(),
            vars: BTreeMap::from([("app".into(), "research".into())]),
            ..Default::default()
        };
        assert_eq!(
            context.render_string("{{input}} in {{app}}"),
            "hello in research"
        );
        assert_eq!(context.render_string("{{handle}}"), "s_1");
        assert_eq!(context.render_string("{{typo}}"), "{{typo}}");
        assert_eq!(
            context.render(&json!({"message": {"text": "{{input}}"}})),
            json!({"message": {"text": "hello"}})
        );
    }

    #[test]
    fn context_references_match_render_walk() {
        let step = Step {
            path: "/{{context.plan}}".into(),
            body: Some(json!({"all": "{{context}}", "pinned": "{{context.pinned}}"})),
            ..Default::default()
        };
        assert_eq!(
            step.context_refs(),
            ["context.plan", "context", "context.pinned"]
        );
    }
}
