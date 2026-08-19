use std::collections::BTreeSet;

use regex::Regex;

use crate::connector::Registry;

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct WakeDecision {
    pub agents: Vec<String>,
    pub why: String,
}

pub fn who_wakes(
    text: &str,
    to: &[String],
    agents: &[String],
    speakers: &[String],
    registry: &Registry,
) -> WakeDecision {
    let addressed = to
        .iter()
        .filter(|candidate| agents.contains(candidate))
        .cloned()
        .collect::<Vec<_>>();
    if !addressed.is_empty() {
        return WakeDecision {
            agents: addressed,
            why: "addressed".into(),
        };
    }

    let words = tokens(text);
    let mentioned = mentions(text);
    let mut by_mention = Vec::new();
    let mut by_name = Vec::new();
    let mut by_interest = Vec::new();
    let mut interest = String::new();
    for agent in agents {
        let lower = agent.to_ascii_lowercase();
        if mentioned.contains(&lower) {
            by_mention.push(agent.clone());
        } else if words.contains(&lower) {
            by_name.push(agent.clone());
        } else if let Some(spec) = registry.get(agent) {
            for wanted in &spec.interests {
                let wanted = wanted.trim().to_ascii_lowercase();
                if !wanted.is_empty() && words.contains(&wanted) {
                    by_interest.push(agent.clone());
                    interest = wanted;
                    break;
                }
            }
        }
    }
    if !by_mention.is_empty() {
        return WakeDecision {
            agents: by_mention,
            why: "mentioned".into(),
        };
    }
    if !by_name.is_empty() {
        return WakeDecision {
            agents: by_name,
            why: "named".into(),
        };
    }
    for speaker in speakers {
        let lower = speaker.to_ascii_lowercase();
        if !lower.is_empty() && (words.contains(&lower) || mentioned.contains(&lower)) {
            return WakeDecision {
                agents: Vec::new(),
                why: format!("for {speaker}"),
            };
        }
    }
    if !by_interest.is_empty() {
        return WakeDecision {
            agents: by_interest,
            why: format!("interest: {interest}"),
        };
    }
    if chatter(text) {
        return WakeDecision {
            agents: Vec::new(),
            why: "chatter".into(),
        };
    }
    WakeDecision {
        agents: agents.to_vec(),
        why: "open to the room".into(),
    }
}

fn tokens(text: &str) -> BTreeSet<String> {
    let address =
        Regex::new(r"(?i)\b\S+@\S+\.\S+|https?://\S+|\b\S+\.(com|io|net|org|co|dev|ai)\b\S*")
            .expect("address regex");
    address
        .replace_all(&text.to_ascii_lowercase(), " ")
        .split(|character: char| {
            !character.is_alphanumeric() && character != '-' && character != '_'
        })
        .filter(|word| !word.is_empty())
        .map(str::to_owned)
        .collect()
}

fn mentions(text: &str) -> BTreeSet<String> {
    Regex::new(r"@([A-Za-z0-9_-]+)")
        .expect("mention regex")
        .captures_iter(text)
        .filter_map(|capture| capture.get(1))
        .map(|value| value.as_str().to_ascii_lowercase())
        .collect()
}

fn chatter(text: &str) -> bool {
    if text.contains('?') {
        return false;
    }
    let acknowledgements = [
        "ok",
        "okay",
        "k",
        "kk",
        "yep",
        "yeah",
        "yes",
        "yup",
        "no",
        "nope",
        "nah",
        "thanks",
        "thank",
        "thx",
        "ty",
        "cheers",
        "lol",
        "haha",
        "np",
        "agreed",
        "agree",
        "noted",
        "cool",
        "nice",
        "great",
        "perfect",
        "hi",
        "hey",
        "hello",
        "morning",
        "afternoon",
        "everyone",
        "all",
        "folks",
        "team",
        "got",
        "it",
        "on",
        "same",
        "sure",
        "both",
        "you",
        "of",
        "guys",
        "much",
        "lot",
        "a",
        "very",
        "so",
        "too",
        "man",
        "mate",
        "welcome",
        "please",
        "pls",
        "for",
        "me",
        "us",
        "my",
        "your",
        "sounds",
        "good",
        "makes",
        "sense",
        "works",
        "fine",
        "done",
        "awesome",
        "brilliant",
        "excellent",
        "lovely",
        "super",
        "we",
        "i",
        "that",
        "this",
        "s",
        "re",
        "m",
        "ll",
        "ve",
        "d",
        "t",
    ]
    .into_iter()
    .collect::<BTreeSet<_>>();
    let words = text
        .to_ascii_lowercase()
        .split(|character: char| !character.is_alphanumeric())
        .filter(|word| !word.is_empty())
        .map(str::to_owned)
        .collect::<Vec<_>>();
    words.is_empty()
        || words
            .iter()
            .all(|word| acknowledgements.contains(word.as_str()))
}

#[cfg(test)]
mod tests {
    use std::collections::BTreeMap;

    use crate::connector::{Origin, Spec, Step};

    use super::*;

    fn registry() -> Registry {
        let registry = Registry::new();
        for (name, interests) in [
            ("code", vec!["repo".into()]),
            ("spend", vec!["budget".into()]),
        ] {
            registry
                .put(Spec {
                    name: name.into(),
                    base: "http://localhost".into(),
                    acp: None,
                    headers: BTreeMap::new(),
                    vars: BTreeMap::new(),
                    capabilities: Vec::new(),
                    interests,
                    timeout: String::new(),
                    open: None,
                    turn: Step::default(),
                    context: Vec::new(),
                    origin: Origin::File,
                })
                .unwrap();
        }
        registry
    }

    #[test]
    fn wake_ladder_preserves_precedence() {
        let registry = registry();
        let agents = vec!["code".into(), "spend".into()];
        assert_eq!(
            who_wakes("@code help", &[], &agents, &[], &registry).why,
            "mentioned"
        );
        assert_eq!(
            who_wakes("budget please", &[], &agents, &[], &registry).agents,
            ["spend"]
        );
        assert_eq!(
            who_wakes("thanks both of you", &[], &agents, &[], &registry).why,
            "chatter"
        );
        assert_eq!(
            who_wakes(
                "arsh, what happened with the budget",
                &[],
                &agents,
                &["arsh".into()],
                &registry
            )
            .why,
            "for arsh"
        );
        assert_eq!(
            who_wakes("what now?", &[], &agents, &[], &registry)
                .agents
                .len(),
            2
        );
    }
}
