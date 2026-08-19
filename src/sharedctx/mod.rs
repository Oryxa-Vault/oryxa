//! Shared room state, folded from events.

use std::{collections::BTreeMap, sync::RwLock};

use chrono::{DateTime, Utc};
use serde::{Deserialize, Serialize};
use thiserror::Error;

pub const MAX_ITEMS_PER_ENTRY: usize = 20;

#[derive(Clone, Copy, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(rename_all = "lowercase")]
pub enum Kind {
    Append,
    Value,
}

#[derive(Clone, Debug, Deserialize, Serialize)]
pub struct Item {
    pub by: String,
    pub at: DateTime<Utc>,
    pub text: String,
    pub seq: i64,
}

#[derive(Clone, Debug, Deserialize, Serialize)]
pub struct Rollup {
    pub text: String,
    pub covers: usize,
    pub through: i64,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub by: String,
}

#[derive(Clone, Debug, Deserialize, Serialize)]
pub struct Entry {
    pub key: String,
    pub kind: Kind,
    pub pinned: bool,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub value: String,
    #[serde(default, skip_serializing_if = "is_zero")]
    pub version: i64,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub by: String,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub items: Vec<Item>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub rollup: Option<Rollup>,
}

fn is_zero(value: &i64) -> bool {
    *value == 0
}

#[derive(Clone, Debug, Error)]
pub enum ContextError {
    #[error("entry is a different kind: {0:?}")]
    WrongKind(String),
    #[error("no such entry: {0:?}")]
    NotFound(String),
    #[error("stale write to {key:?}: current version is {version}")]
    Conflict {
        key: String,
        current: String,
        version: i64,
        by: String,
    },
}

#[derive(Default)]
pub struct Store {
    entries: RwLock<BTreeMap<String, Entry>>,
}

impl Store {
    pub fn new() -> Self {
        Self::default()
    }

    pub fn append(
        &self,
        key: &str,
        by: &str,
        text: &str,
        seq: i64,
        at: DateTime<Utc>,
    ) -> Result<Entry, ContextError> {
        let mut entries = self.entries.write().expect("shared context poisoned");
        let entry = entries.entry(key.to_owned()).or_insert_with(|| Entry {
            key: key.to_owned(),
            kind: Kind::Append,
            pinned: false,
            value: String::new(),
            version: 0,
            by: String::new(),
            items: Vec::new(),
            rollup: None,
        });
        if entry.kind != Kind::Append {
            return Err(ContextError::WrongKind(key.to_owned()));
        }
        entry.items.push(Item {
            by: by.to_owned(),
            at,
            text: text.to_owned(),
            seq,
        });
        entry.version = seq;
        Ok(entry.clone())
    }

    pub fn set(
        &self,
        key: &str,
        by: &str,
        value: &str,
        if_match: i64,
        seq: i64,
    ) -> Result<Entry, ContextError> {
        let mut entries = self.entries.write().expect("shared context poisoned");
        if let Some(entry) = entries.get_mut(key) {
            if entry.kind != Kind::Value {
                return Err(ContextError::WrongKind(key.to_owned()));
            }
            if if_match != -1 && entry.version != if_match {
                return Err(ContextError::Conflict {
                    key: key.to_owned(),
                    current: entry.value.clone(),
                    version: entry.version,
                    by: entry.by.clone(),
                });
            }
            entry.value = value.to_owned();
            entry.version = seq;
            entry.by = by.to_owned();
            return Ok(entry.clone());
        }
        if if_match > 0 {
            return Err(ContextError::Conflict {
                key: key.to_owned(),
                current: String::new(),
                version: 0,
                by: String::new(),
            });
        }
        let entry = Entry {
            key: key.to_owned(),
            kind: Kind::Value,
            pinned: false,
            value: value.to_owned(),
            version: seq,
            by: by.to_owned(),
            items: Vec::new(),
            rollup: None,
        };
        entries.insert(key.to_owned(), entry.clone());
        Ok(entry)
    }

    pub fn pin(&self, key: &str, pinned: bool) -> Result<Entry, ContextError> {
        let mut entries = self.entries.write().expect("shared context poisoned");
        let entry = entries
            .get_mut(key)
            .ok_or_else(|| ContextError::NotFound(key.to_owned()))?;
        entry.pinned = pinned;
        Ok(entry.clone())
    }

    pub fn get(&self, key: &str) -> Option<Entry> {
        self.entries
            .read()
            .expect("shared context poisoned")
            .get(key)
            .cloned()
    }

    pub fn snapshot(&self) -> Vec<Entry> {
        self.entries
            .read()
            .expect("shared context poisoned")
            .values()
            .cloned()
            .collect()
    }

    pub fn pinned(&self) -> Vec<Entry> {
        self.snapshot()
            .into_iter()
            .filter(|entry| entry.pinned)
            .collect()
    }

    pub fn set_rollup(
        &self,
        key: &str,
        text: &str,
        by: &str,
        covers: usize,
        through: i64,
    ) -> Result<Entry, ContextError> {
        let mut entries = self.entries.write().expect("shared context poisoned");
        let entry = entries
            .get_mut(key)
            .ok_or_else(|| ContextError::NotFound(key.to_owned()))?;
        if entry.kind != Kind::Append {
            return Err(ContextError::WrongKind(key.to_owned()));
        }
        entry.rollup = Some(Rollup {
            text: text.to_owned(),
            covers,
            through,
            by: by.to_owned(),
        });
        Ok(entry.clone())
    }

    pub fn needs_rollup(&self, key: &str, wanted: usize) -> Option<(Vec<Item>, i64)> {
        let entry = self.get(key)?;
        if entry.kind == Kind::Value {
            return None;
        }
        let dropped = entry.items.len().saturating_sub(MAX_ITEMS_PER_ENTRY);
        if dropped == 0 {
            return None;
        }
        let covered = entry.rollup.as_ref().map_or(0, |rollup| {
            entry.items[..dropped]
                .iter()
                .filter(|item| item.seq <= rollup.through)
                .count()
        });
        if dropped.saturating_sub(covered) < wanted {
            return None;
        }
        let items = entry.items[..dropped].to_vec();
        let through = items.last()?.seq;
        Some((items, through))
    }
}

pub fn render(entries: &[Entry]) -> String {
    entries
        .iter()
        .filter_map(|entry| {
            let body = render_entry(entry);
            (!body.is_empty()).then(|| {
                if body.contains('\n') {
                    format!("{}:\n{body}", entry.key)
                } else {
                    format!("{}: {body}", entry.key)
                }
            })
        })
        .collect::<Vec<_>>()
        .join("\n\n")
}

pub fn render_entry(entry: &Entry) -> String {
    if entry.kind == Kind::Value {
        return entry.value.clone();
    }
    let dropped = entry.items.len().saturating_sub(MAX_ITEMS_PER_ENTRY);
    let mut lines = Vec::new();
    let covered = entry.rollup.as_ref().map_or(0, |rollup| {
        entry.items[..dropped]
            .iter()
            .filter(|item| item.seq <= rollup.through)
            .count()
    });
    if covered > 0 {
        let rollup = entry.rollup.as_ref().expect("covered requires rollup");
        lines.push(format!(
            "- ({covered} earlier items, summarised) {}",
            rollup.text
        ));
    }
    if dropped > covered {
        lines.push(format!("- ({} earlier items not shown)", dropped - covered));
    }
    lines.extend(
        entry.items[dropped..]
            .iter()
            .map(|item| format!("- {}", item.text)),
    );
    lines.join("\n")
}

pub fn elided(entries: &[Entry]) -> usize {
    entries
        .iter()
        .filter(|entry| entry.kind == Kind::Append)
        .map(|entry| {
            let dropped = entry.items.len().saturating_sub(MAX_ITEMS_PER_ENTRY);
            let covered = entry.rollup.as_ref().map_or(0, |rollup| {
                entry.items[..dropped]
                    .iter()
                    .filter(|item| item.seq <= rollup.through)
                    .count()
            });
            dropped.saturating_sub(covered)
        })
        .sum()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn append_and_values_keep_their_semantics() {
        let store = Store::new();
        store.append("findings", "a", "one", 1, Utc::now()).unwrap();
        store.append("findings", "b", "two", 2, Utc::now()).unwrap();
        let findings = store.get("findings").unwrap();
        assert_eq!(findings.items.len(), 2);
        assert!(store.set("findings", "a", "wrong", -1, 3).is_err());

        let plan = store.set("plan", "a", "first", -1, 4).unwrap();
        let conflict = store.set("plan", "b", "stale", plan.version - 1, 5);
        assert!(matches!(conflict, Err(ContextError::Conflict { .. })));
        assert_eq!(
            store
                .set("plan", "b", "next", plan.version, 6)
                .unwrap()
                .value,
            "next"
        );
    }

    #[test]
    fn rendering_is_bounded_without_trimming_storage() {
        let store = Store::new();
        for index in 0..MAX_ITEMS_PER_ENTRY + 4 {
            store
                .append(
                    "findings",
                    "a",
                    &format!("item {index}"),
                    index as i64 + 1,
                    Utc::now(),
                )
                .unwrap();
        }
        let entry = store.get("findings").unwrap();
        assert_eq!(entry.items.len(), MAX_ITEMS_PER_ENTRY + 4);
        assert!(render_entry(&entry).contains("4 earlier items not shown"));
        assert_eq!(elided(&[entry]), 4);
    }
}
