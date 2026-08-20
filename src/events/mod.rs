//! Append-only event stores.

use std::{collections::BTreeMap, io::ErrorKind, path::PathBuf, sync::Arc};

use async_trait::async_trait;
use chrono::{DateTime, Utc};
use serde::{Deserialize, Serialize};
use serde_json::Value;
use thiserror::Error;
use tokio::{
    io::AsyncWriteExt,
    sync::{RwLock, broadcast},
};
use tokio_postgres::{Client, NoTls};

pub const SYSTEM_STREAM: &str = "_system";

pub fn reserved(id: &str) -> bool {
    id.starts_with('_')
}

#[derive(Clone, Debug, Deserialize, Serialize)]
pub struct Event {
    pub seq: i64,
    pub session: String,
    pub ts: DateTime<Utc>,
    pub kind: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub actor: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub turn: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub data: Option<Value>,
}

#[derive(Debug, Error)]
pub enum StoreError {
    #[error("{0}")]
    Message(String),
    #[error(transparent)]
    Io(#[from] std::io::Error),
    #[error(transparent)]
    Json(#[from] serde_json::Error),
    #[error(transparent)]
    Postgres(#[from] tokio_postgres::Error),
}

#[async_trait]
pub trait Store: Send + Sync {
    async fn append(
        &self,
        session: &str,
        kind: &str,
        actor: &str,
        turn: &str,
        data: Option<Value>,
    ) -> Result<Event, StoreError>;
    async fn since(&self, session: &str, since: i64) -> Result<Vec<Event>, StoreError>;
    fn subscribe(&self) -> broadcast::Receiver<Event>;
    async fn sessions(&self) -> Result<Vec<String>, StoreError>;
    async fn reset(&self) -> Result<usize, StoreError>;
}

#[derive(Default)]
struct MemoryState {
    by_session: BTreeMap<String, Vec<Event>>,
    order: Vec<String>,
}

pub struct MemoryStore {
    state: RwLock<MemoryState>,
    fanout: broadcast::Sender<Event>,
}

impl Default for MemoryStore {
    fn default() -> Self {
        Self::new()
    }
}

impl MemoryStore {
    pub fn new() -> Self {
        let (fanout, _) = broadcast::channel(64);
        Self {
            state: RwLock::new(MemoryState::default()),
            fanout,
        }
    }
}

#[async_trait]
impl Store for MemoryStore {
    async fn append(
        &self,
        session: &str,
        kind: &str,
        actor: &str,
        turn: &str,
        data: Option<Value>,
    ) -> Result<Event, StoreError> {
        let mut state = self.state.write().await;
        if !state.by_session.contains_key(session) {
            state.order.push(session.to_owned());
        }
        let events = state.by_session.entry(session.to_owned()).or_default();
        let event = Event {
            seq: events.len() as i64 + 1,
            session: session.to_owned(),
            ts: Utc::now(),
            kind: kind.to_owned(),
            actor: actor.to_owned(),
            turn: turn.to_owned(),
            data,
        };
        events.push(event.clone());
        drop(state);
        let _ = self.fanout.send(event.clone());
        Ok(event)
    }

    async fn since(&self, session: &str, since: i64) -> Result<Vec<Event>, StoreError> {
        let state = self.state.read().await;
        Ok(state
            .by_session
            .get(session)
            .into_iter()
            .flatten()
            .filter(|event| event.seq > since)
            .cloned()
            .collect())
    }

    fn subscribe(&self) -> broadcast::Receiver<Event> {
        self.fanout.subscribe()
    }

    async fn sessions(&self) -> Result<Vec<String>, StoreError> {
        Ok(self.state.read().await.order.clone())
    }

    async fn reset(&self) -> Result<usize, StoreError> {
        let mut state = self.state.write().await;
        let count = state.order.len();
        *state = MemoryState::default();
        Ok(count)
    }
}

/// A durable append-only JSONL store for a single-user local runtime.
///
/// The in-memory projection keeps reads cheap while every accepted event is
/// flushed to disk before it becomes visible to subscribers. PostgreSQL remains
/// the shared/server store; this file store is intentionally scoped to the
/// native desktop control plane.
pub struct FileStore {
    state: RwLock<MemoryState>,
    path: PathBuf,
    fanout: broadcast::Sender<Event>,
}

impl FileStore {
    pub async fn open(path: impl Into<PathBuf>) -> Result<Self, StoreError> {
        let path = path.into();
        if let Some(parent) = path.parent() {
            tokio::fs::create_dir_all(parent).await?;
        }
        let contents = match tokio::fs::read_to_string(&path).await {
            Ok(contents) => contents,
            Err(error) if error.kind() == ErrorKind::NotFound => String::new(),
            Err(error) => return Err(error.into()),
        };
        let mut state = MemoryState::default();
        for (index, line) in contents.lines().enumerate() {
            if line.trim().is_empty() {
                continue;
            }
            let event: Event = serde_json::from_str(line).map_err(|error| {
                StoreError::Message(format!(
                    "{}:{}: invalid event: {error}",
                    path.display(),
                    index + 1
                ))
            })?;
            if !state.by_session.contains_key(&event.session) {
                state.order.push(event.session.clone());
            }
            let events = state.by_session.entry(event.session.clone()).or_default();
            let expected = events.len() as i64 + 1;
            if event.seq != expected {
                return Err(StoreError::Message(format!(
                    "{}:{}: expected sequence {expected}, got {}",
                    path.display(),
                    index + 1,
                    event.seq
                )));
            }
            events.push(event);
        }
        let (fanout, _) = broadcast::channel(64);
        Ok(Self {
            state: RwLock::new(state),
            path,
            fanout,
        })
    }
}

#[async_trait]
impl Store for FileStore {
    async fn append(
        &self,
        session: &str,
        kind: &str,
        actor: &str,
        turn: &str,
        data: Option<Value>,
    ) -> Result<Event, StoreError> {
        let mut state = self.state.write().await;
        let is_new = !state.by_session.contains_key(session);
        let seq = state
            .by_session
            .get(session)
            .map_or(1, |events| events.len() as i64 + 1);
        let event = Event {
            seq,
            session: session.to_owned(),
            ts: Utc::now(),
            kind: kind.to_owned(),
            actor: actor.to_owned(),
            turn: turn.to_owned(),
            data,
        };
        let mut encoded = serde_json::to_vec(&event)?;
        encoded.push(b'\n');
        let mut file = tokio::fs::OpenOptions::new()
            .create(true)
            .append(true)
            .open(&self.path)
            .await?;
        file.write_all(&encoded).await?;
        file.flush().await?;
        file.sync_data().await?;
        if is_new {
            state.order.push(session.to_owned());
        }
        state
            .by_session
            .entry(session.to_owned())
            .or_default()
            .push(event.clone());
        drop(state);
        let _ = self.fanout.send(event.clone());
        Ok(event)
    }

    async fn since(&self, session: &str, since: i64) -> Result<Vec<Event>, StoreError> {
        let state = self.state.read().await;
        Ok(state
            .by_session
            .get(session)
            .into_iter()
            .flatten()
            .filter(|event| event.seq > since)
            .cloned()
            .collect())
    }

    fn subscribe(&self) -> broadcast::Receiver<Event> {
        self.fanout.subscribe()
    }

    async fn sessions(&self) -> Result<Vec<String>, StoreError> {
        Ok(self.state.read().await.order.clone())
    }

    async fn reset(&self) -> Result<usize, StoreError> {
        let mut state = self.state.write().await;
        let count = state.order.len();
        tokio::fs::write(&self.path, []).await?;
        *state = MemoryState::default();
        Ok(count)
    }
}

const SCHEMA: &str = r#"
CREATE TABLE IF NOT EXISTS oryxa_sessions (
  id          TEXT PRIMARY KEY,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  last_seq    BIGINT      NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS oryxa_events (
  session_id  TEXT        NOT NULL REFERENCES oryxa_sessions(id) ON DELETE CASCADE,
  seq         BIGINT      NOT NULL,
  ts          TIMESTAMPTZ NOT NULL DEFAULT now(),
  kind        TEXT        NOT NULL,
  actor       TEXT        NOT NULL DEFAULT '',
  turn        TEXT        NOT NULL DEFAULT '',
  data        JSONB,
  PRIMARY KEY (session_id, seq)
);
CREATE INDEX IF NOT EXISTS oryxa_events_session_seq
  ON oryxa_events (session_id, seq);
"#;

pub struct PostgresStore {
    client: Arc<Client>,
    fanout: broadcast::Sender<Event>,
}

impl PostgresStore {
    pub async fn connect(dsn: &str) -> Result<Self, StoreError> {
        let (client, connection) = tokio_postgres::connect(dsn, NoTls).await?;
        tokio::spawn(async move {
            if let Err(error) = connection.await {
                eprintln!("oryxa: postgres connection failed: {error}");
            }
        });
        client.batch_execute(SCHEMA).await?;
        let (fanout, _) = broadcast::channel(64);
        Ok(Self {
            client: Arc::new(client),
            fanout,
        })
    }
}

#[async_trait]
impl Store for PostgresStore {
    async fn append(
        &self,
        session: &str,
        kind: &str,
        actor: &str,
        turn: &str,
        data: Option<Value>,
    ) -> Result<Event, StoreError> {
        // One statement is one transaction. The upsert locks only this room's
        // counter row, preserving per-room ordering without serialising rooms.
        let row = self
            .client
            .query_one(
                r#"
WITH allocated AS (
  INSERT INTO oryxa_sessions (id, last_seq) VALUES ($1, 1)
  ON CONFLICT (id) DO UPDATE SET last_seq = oryxa_sessions.last_seq + 1
  RETURNING last_seq
)
INSERT INTO oryxa_events (session_id, seq, kind, actor, turn, data)
SELECT $1, last_seq, $2, $3, $4, $5 FROM allocated
RETURNING seq, ts
"#,
                &[&session, &kind, &actor, &turn, &data],
            )
            .await?;
        let event = Event {
            seq: row.get(0),
            session: session.to_owned(),
            ts: row.get::<_, DateTime<Utc>>(1),
            kind: kind.to_owned(),
            actor: actor.to_owned(),
            turn: turn.to_owned(),
            data,
        };
        let _ = self.fanout.send(event.clone());
        Ok(event)
    }

    async fn since(&self, session: &str, since: i64) -> Result<Vec<Event>, StoreError> {
        let rows = self
            .client
            .query(
                "SELECT seq, ts, kind, actor, turn, data FROM oryxa_events WHERE session_id=$1 AND seq>$2 ORDER BY seq",
                &[&session, &since],
            )
            .await?;
        Ok(rows
            .into_iter()
            .map(|row| Event {
                seq: row.get(0),
                session: session.to_owned(),
                ts: row.get(1),
                kind: row.get(2),
                actor: row.get(3),
                turn: row.get(4),
                data: row.get(5),
            })
            .collect())
    }

    fn subscribe(&self) -> broadcast::Receiver<Event> {
        self.fanout.subscribe()
    }

    async fn sessions(&self) -> Result<Vec<String>, StoreError> {
        Ok(self
            .client
            .query("SELECT id FROM oryxa_sessions ORDER BY created_at, id", &[])
            .await?
            .into_iter()
            .map(|row| row.get(0))
            .collect())
    }

    async fn reset(&self) -> Result<usize, StoreError> {
        let row = self
            .client
            .query_one("SELECT count(*) FROM oryxa_sessions", &[])
            .await?;
        let count: i64 = row.get(0);
        self.client
            .execute("DELETE FROM oryxa_sessions", &[])
            .await?;
        Ok(count as usize)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    #[tokio::test]
    async fn memory_is_ordered_and_replayable() {
        let store = MemoryStore::new();
        let mut subscriber = store.subscribe();
        let first = store
            .append(
                "s_1",
                "session.created",
                "",
                "",
                Some(json!({"agent": "a"})),
            )
            .await
            .unwrap();
        let second = store
            .append("s_1", "input.submitted", "alice", "in_1", None)
            .await
            .unwrap();
        assert_eq!((first.seq, second.seq), (1, 2));
        assert_eq!(subscriber.recv().await.unwrap().kind, "session.created");
        assert_eq!(store.since("s_1", 1).await.unwrap()[0].seq, 2);
        assert_eq!(store.sessions().await.unwrap(), ["s_1"]);
    }

    #[tokio::test]
    async fn reset_restarts_sequence_numbers() {
        let store = MemoryStore::new();
        store.append("s_1", "one", "", "", None).await.unwrap();
        assert_eq!(store.reset().await.unwrap(), 1);
        assert_eq!(
            store.append("s_1", "two", "", "", None).await.unwrap().seq,
            1
        );
    }

    #[tokio::test]
    async fn file_store_survives_reopen_and_keeps_order() {
        let directory = tempfile::tempdir().unwrap();
        let path = directory.path().join("events.jsonl");
        let store = FileStore::open(&path).await.unwrap();
        store
            .append("s_1", "session.created", "", "", Some(json!({"agent":"a"})))
            .await
            .unwrap();
        store
            .append("s_1", "input.submitted", "alice", "in_1", None)
            .await
            .unwrap();
        drop(store);

        let reopened = FileStore::open(&path).await.unwrap();
        assert_eq!(reopened.sessions().await.unwrap(), ["s_1"]);
        let events = reopened.since("s_1", 0).await.unwrap();
        assert_eq!(events.len(), 2);
        assert_eq!(events[1].seq, 2);
        assert_eq!(events[1].actor, "alice");
    }
}
