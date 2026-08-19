use chrono::{DateTime, Utc};
use serde::{Deserialize, Serialize};

#[derive(Clone, Debug)]
pub struct Author {
    pub name: String,
    pub source: String,
}

impl Author {
    pub fn claimed(name: impl Into<String>) -> Self {
        Self {
            name: name.into(),
            source: "claimed".into(),
        }
    }
}

#[derive(Clone, Debug, Deserialize, Serialize)]
pub struct Input {
    pub id: String,
    pub seq: usize,
    pub author: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub source: String,
    pub text: String,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub to: Vec<String>,
    pub wake: Vec<String>,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub why: String,
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    pub withdrawn: bool,
}

#[derive(Clone, Copy, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(rename_all = "lowercase")]
pub enum TurnState {
    Queued,
    Running,
    Done,
    Failed,
    Cancelled,
}

#[derive(Clone, Debug, Deserialize, Serialize)]
pub struct Turn {
    pub id: String,
    pub agent: String,
    pub state: TurnState,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub inputs: Vec<Input>,
    pub author: String,
    pub text: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub error: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub output: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub group: String,
}

impl Turn {
    pub fn new(agent: &str, inputs: Vec<Input>) -> Self {
        let author = inputs
            .last()
            .map(|input| input.author.clone())
            .unwrap_or_default();
        let group = inputs
            .last()
            .map(|input| input.id.clone())
            .unwrap_or_default();
        Self {
            id: format!("t_{}", short_id(12)),
            agent: agent.to_owned(),
            state: TurnState::Running,
            text: render_inputs(&inputs),
            inputs,
            author,
            error: String::new(),
            output: String::new(),
            group,
        }
    }
}

pub fn render_inputs(inputs: &[Input]) -> String {
    match inputs {
        [] => String::new(),
        [one] => one.text.clone(),
        many => many
            .iter()
            .map(|input| format!("{}: {}", input.author, input.text))
            .collect::<Vec<_>>()
            .join("\n"),
    }
}

#[derive(Clone, Copy, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(rename_all = "lowercase")]
pub enum State {
    Idle,
    Running,
    Closed,
}

#[derive(Clone, Debug, Deserialize, Serialize)]
pub struct Summary {
    pub id: String,
    pub agent: String,
    pub agents: Vec<String>,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub workspace: String,
    pub state: State,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub handle: String,
    #[serde(default, skip_serializing_if = "std::collections::BTreeMap::is_empty")]
    pub handles: std::collections::BTreeMap<String, String>,
    pub created: DateTime<Utc>,
}

#[derive(Clone, Debug, Deserialize, Serialize)]
pub struct View {
    #[serde(flatten)]
    pub summary: Summary,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub waiting: Vec<Input>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub current: Option<Turn>,
    pub history: Vec<Turn>,
}

pub(crate) fn short_id(length: usize) -> String {
    uuid::Uuid::new_v4().simple().to_string()[..length].to_owned()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn one_message_is_unchanged_and_many_are_attributed() {
        let input = |author: &str, text: &str| Input {
            id: short_id(4),
            seq: 0,
            author: author.into(),
            source: "claimed".into(),
            text: text.into(),
            to: Vec::new(),
            wake: vec!["a".into()],
            why: String::new(),
            withdrawn: false,
        };
        assert_eq!(render_inputs(&[input("alice", "hello")]), "hello");
        assert_eq!(
            render_inputs(&[input("alice", "ship it"), input("bob", "wait")]),
            "alice: ship it\nbob: wait"
        );
    }
}
