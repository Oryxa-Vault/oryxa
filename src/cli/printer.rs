//! Renders a room's event stream as text.
//!
//! It keeps the little state needed to read well. One input fans out to a turn
//! per agent, so the question is printed once per group rather than once per
//! agent.
//!
//! The other piece of state is who is speaking, and that one exists because
//! lanes run in parallel. Announcing an agent when its turn starts reads
//! correctly only if turns take it in turns, which is exactly what this project
//! went out of its way not to do: two agents starting together printed both
//! headers back to back and then concatenated both answers into one paragraph.
//!
//! So the header is printed when an agent first says something, and again
//! whenever the speaker changes. A single agent is unaffected — one header, one
//! continuous stream, still live. Several agents read as blocks, attributed, in
//! the order the room actually heard them.

use std::{
    collections::BTreeSet,
    io::{Write, stdout},
};

use serde::Deserialize;

use crate::events::Event;

#[derive(Default, Deserialize)]
struct Data {
    #[serde(default)]
    text: String,
    #[serde(default)]
    kind: String,
    #[serde(default)]
    error: String,
    #[serde(default)]
    key: String,
    #[serde(default)]
    value: String,
    #[serde(default)]
    group: String,
}

/// A turn that worked and said nothing.
///
/// Decoded separately because `text` is a string on every other event and a
/// list of selectors on this one. Sharing a struct means the whole decode fails
/// on the type mismatch and every field silently reads as empty.
#[derive(Default, Deserialize)]
struct Empty {
    #[serde(default)]
    reason: String,
    #[serde(default)]
    text: Vec<String>,
    #[serde(default)]
    when: String,
}

pub struct Printer {
    raw: bool,
    groups: BTreeSet<String>,
    /// Whose text is mid-line right now.
    speaking: String,
}

impl Printer {
    pub fn new(raw: bool) -> Self {
        Self {
            raw,
            groups: BTreeSet::new(),
            speaking: String::new(),
        }
    }

    pub fn print(&mut self, event: &Event) {
        let data: Data = event
            .data
            .clone()
            .and_then(|value| serde_json::from_value(value).ok())
            .unwrap_or_default();

        match event.kind.as_str() {
            "output.part" if data.kind == "text" => self.say(&event.actor, &data.text),
            "output.part" if self.raw => {
                let raw = event
                    .data
                    .as_ref()
                    .map(ToString::to_string)
                    .unwrap_or_default();
                self.line(format_args!("  · {} {raw}", event.actor));
            }
            "output.part" => {}
            "input.submitted" => {
                if !data.group.is_empty() && !self.groups.insert(data.group) {
                    return; // already shown; this copy belongs to another agent
                }
                self.end_line();
                self.line(format_args!("\n  {} › {}", event.actor, data.text));
            }
            // Nothing is printed when a turn starts. An agent is announced by
            // speaking.
            "turn.started" => {}
            "turn.finished" => {
                if self.speaking == event.actor {
                    self.end_line();
                }
            }
            // The server already worked out which half a silent turn came from
            // and put the selectors on the event, so the terminal says what the
            // viewer says rather than inventing a thinner version of it — a
            // silent agent is the failure people blame the framework for.
            "turn.empty" => {
                let empty: Empty = event
                    .data
                    .clone()
                    .and_then(|value| serde_json::from_value(value).ok())
                    .unwrap_or_default();
                self.end_line();
                self.line(format_args!(
                    "  {} ‹ finished without saying anything — {}",
                    event.actor, empty.reason
                ));
                let mut selectors = Vec::new();
                if !empty.text.is_empty() {
                    selectors.push(format!("text: {}", empty.text.join(" | ")));
                }
                if !empty.when.is_empty() {
                    selectors.push(format!("when: {}", empty.when));
                }
                if !selectors.is_empty() {
                    self.line(format_args!("      [{}]", selectors.join("  ")));
                }
            }
            "turn.failed" => {
                self.end_line();
                self.line(format_args!("  {} failed: {}", event.actor, data.error));
            }
            "turn.cancelled" => {
                self.end_line();
                self.line(format_args!("  {} cancelled", event.actor));
            }
            "interaction.requested" => {
                self.end_line();
                self.line(format_args!(
                    "  · {} is waiting for a decision — oryxa approve {}",
                    event.actor, event.session
                ));
            }
            "context.appended" => {
                self.end_line();
                self.line(format_args!("  · {} appended to {}", event.actor, data.key));
            }
            "context.set" => {
                self.end_line();
                self.line(format_args!(
                    "  · {} set {} = {}",
                    event.actor, data.key, data.value
                ));
            }
            "conflict.rejected" => {
                self.end_line();
                self.line(format_args!("  · conflict on {} (rejected)", data.key));
            }
            _ if self.raw => {
                self.end_line();
                self.line(format_args!(
                    "  · seq={} {} {}",
                    event.seq, event.kind, event.actor
                ));
            }
            _ => {}
        }
        // Deltas arrive as separate events and the line is left open between
        // them, so nothing reaches the terminal until the buffer happens to
        // fill. Flushing here is what makes a streaming agent read like one.
        let _ = stdout().flush();
    }

    /// Prints text as coming from `actor`, opening a new block if the speaker
    /// changed since the last thing printed.
    fn say(&mut self, actor: &str, text: &str) {
        if self.speaking != actor {
            self.end_line();
            print!("  {actor} ‹ ");
            self.speaking = actor.to_string();
        }
        print!("{text}"); // no newline: deltas concatenate into one answer
    }

    pub fn end_line(&mut self) {
        if !self.speaking.is_empty() {
            println!();
            self.speaking.clear();
        }
    }

    fn line(&mut self, args: std::fmt::Arguments<'_>) {
        println!("{args}");
    }
}
