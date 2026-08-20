use std::{
    collections::{BTreeMap, BTreeSet},
    sync::Arc,
};

use chrono::{DateTime, Utc};
use serde::Deserialize;
use serde_json::{Value, json};
use sha2::{Digest, Sha256};
use subtle::ConstantTimeEq;
use thiserror::Error;
use tokio::sync::{Mutex, RwLock, mpsc};
use tokio_util::sync::CancellationToken;

use crate::{
    connector::{
        ContextRule, ContextView, Executor, Part, PendingInteraction, Registry, RenderContext, Spec,
    },
    events::{Store as EventStore, StoreError},
    sharedctx::{self, ContextError},
};

use super::{Author, Input, State, Summary, Turn, TurnState, View, model::short_id, who_wakes};

#[derive(Debug, Error)]
pub enum SessionError {
    #[error("session not found")]
    NoSession,
    #[error("agent not registered: {0}")]
    NoAgent(String),
    #[error("session is closed")]
    Closed,
    #[error("turn not found or already running")]
    NoTurn,
    #[error("pending interaction not found")]
    NoInteraction,
    #[error("invalid interaction response: {0}")]
    InvalidInteraction(String),
    #[error(transparent)]
    Store(#[from] StoreError),
    #[error(transparent)]
    Context(#[from] ContextError),
}

struct LaneState {
    cursor: usize,
    current: Option<Turn>,
    handle: String,
    opened: bool,
    captures: BTreeMap<String, String>,
    cancel: Option<CancellationToken>,
}

struct Lane {
    agent: String,
    state: Mutex<LaneState>,
    wake: mpsc::Sender<()>,
}

impl Lane {
    fn new(agent: String) -> (Arc<Self>, mpsc::Receiver<()>) {
        let (wake, receiver) = mpsc::channel(1);
        (
            Arc::new(Self {
                agent,
                state: Mutex::new(LaneState {
                    cursor: 0,
                    current: None,
                    handle: String::new(),
                    opened: false,
                    captures: BTreeMap::new(),
                    cancel: None,
                }),
                wake,
            }),
            receiver,
        )
    }

    async fn nudge(&self) {
        let _ = self.wake.try_send(());
    }

    async fn take(&self, inbox: &[Input]) -> Option<Turn> {
        let mut state = self.state.lock().await;
        if state.cursor >= inbox.len() {
            return None;
        }
        let mut taken = Vec::new();
        let mut wanted = false;
        for input in &inbox[state.cursor..] {
            if input.withdrawn {
                continue;
            }
            wanted |= input.wake.contains(&self.agent);
            taken.push(input.clone());
        }
        if taken.is_empty() {
            state.cursor = inbox.len();
            return None;
        }
        if !wanted {
            return None;
        }
        state.cursor = inbox.len();
        let turn = Turn::new(&self.agent, taken);
        state.current = Some(turn.clone());
        Some(turn)
    }

    async fn finish(&self, turn: &Turn) {
        let mut state = self.state.lock().await;
        if state
            .current
            .as_ref()
            .is_some_and(|current| current.id == turn.id)
        {
            state.current = None;
        }
        state.cancel = None;
    }

    async fn apply_part(&self, turn_id: &str, part: &Part) {
        if part.kind != "text" || part.text.is_empty() {
            return;
        }
        let mut state = self.state.lock().await;
        if let Some(current) = state
            .current
            .as_mut()
            .filter(|current| current.id == turn_id)
        {
            current.output.push_str(&part.text);
        }
    }
}

struct Session {
    id: String,
    agents: Vec<String>,
    workspace: String,
    created: DateTime<Utc>,
    secret_hash: String,
    keys: Mutex<BTreeMap<String, String>>,
    state: RwLock<State>,
    lanes: BTreeMap<String, Arc<Lane>>,
    inbox: Mutex<Vec<Input>>,
    speakers: Mutex<Vec<String>>,
    history: Mutex<Vec<Turn>>,
    context: sharedctx::Store,
    context_write: Mutex<()>,
    rollups: Mutex<BTreeSet<String>>,
}

#[derive(Clone)]
pub struct Manager {
    sessions: Arc<RwLock<BTreeMap<String, Arc<Session>>>>,
    registry: Registry,
    executor: Executor,
    events: Arc<dyn EventStore>,
    summariser: String,
    /// Answer permission requests with the agent's own allow option instead of
    /// waiting for a person.
    ///
    /// This is a real grant: a coding agent can then write to its workspace and
    /// run what it is configured to run, and nobody is asked first. It exists
    /// because approving from a second window while you work in a first is the
    /// friction that gets a room abandoned. The room stays the audit trail —
    /// an express decision goes through the same resolution path a person's
    /// does and is recorded as one, attributed to `express` rather than to
    /// somebody who never saw it.
    express: bool,
}

impl Manager {
    pub fn new(registry: Registry, executor: Executor, events: Arc<dyn EventStore>) -> Arc<Self> {
        Self::configured(registry, executor, events, "", false)
    }

    pub fn configured(
        registry: Registry,
        executor: Executor,
        events: Arc<dyn EventStore>,
        summariser: impl Into<String>,
        express: bool,
    ) -> Arc<Self> {
        Arc::new(Self {
            sessions: Arc::new(RwLock::new(BTreeMap::new())),
            registry,
            executor,
            events,
            summariser: summariser.into(),
            express,
        })
    }

    pub fn registry(&self) -> &Registry {
        &self.registry
    }

    pub fn event_store(&self) -> Arc<dyn EventStore> {
        self.events.clone()
    }

    /// Rebuild every non-system room by folding its append-only event stream.
    /// A turn that was running when the process stopped is retained as failed:
    /// rerunning it could repeat an external side effect whose outcome is
    /// unknowable after a crash.
    pub async fn rehydrate(self: &Arc<Self>) -> Result<usize, SessionError> {
        let ids = self.events.sessions().await?;
        let mut restored = 0;
        for id in ids {
            if crate::events::reserved(&id) {
                continue;
            }
            let events = self.events.since(&id, 0).await?;
            let Some((session, receivers)) = rebuild_session(&id, &events).await else {
                continue;
            };
            self.sessions
                .write()
                .await
                .insert(id.clone(), session.clone());
            if *session.state.read().await != State::Closed {
                for (lane, receiver) in receivers {
                    let manager = self.clone();
                    let session = session.clone();
                    tokio::spawn(async move { manager.lane_loop(session, lane, receiver).await });
                }
                for lane in session.lanes.values() {
                    lane.nudge().await;
                }
            }
            restored += 1;
        }
        Ok(restored)
    }

    pub async fn create(
        self: &Arc<Self>,
        agents: &[String],
    ) -> Result<(Summary, String), SessionError> {
        let workspace = std::env::var("ORYXA_WORKSPACE").unwrap_or_default();
        self.create_scoped(agents, &workspace).await
    }

    /// Starts the ACP subprocesses a room will need, before anybody speaks.
    ///
    /// A local coding agent costs five to ten seconds to reach the point where
    /// it can answer: spawn, protocol handshake, then its own session and auth.
    /// Paying that when the room opens rather than on the first message is the
    /// difference between a room that feels live and one that appears to ignore
    /// its first question.
    ///
    /// Deliberately not awaited and deliberately quiet. A failure here is not
    /// worth failing a room over — the lane opens again on the first turn and
    /// reports properly there, naming the boundary that broke.
    fn warm_acp_lanes(self: &Arc<Self>, session: &Arc<Session>) {
        for agent in &session.agents {
            let Some(spec) = self.registry.get(agent) else {
                continue;
            };
            if !spec.is_acp() {
                continue;
            }
            let executor = self.executor.clone();
            let context = RenderContext {
                conversation: session.id.clone(),
                agent: agent.clone(),
                agents: session.agents.clone(),
                workspace: session.workspace.clone(),
                vars: spec.vars.clone(),
                ..Default::default()
            };
            let events = self.events.clone();
            let room = session.id.clone();
            tokio::spawn(async move {
                if let Err(error) = executor.open(&spec, &context).await {
                    // Into the room, not just onto stderr. A room whose agent
                    // cannot start looks perfectly well until somebody speaks
                    // to it, and then fails in a way that reads as the message
                    // being at fault. Whoever opened the room should be told
                    // while they are still looking at it.
                    let _ = events
                        .append(
                            &room,
                            "lane.unavailable",
                            &context.agent,
                            "",
                            Some(json!({"error": error.to_string()})),
                        )
                        .await;
                }
            });
        }
    }

    pub async fn create_scoped(
        self: &Arc<Self>,
        agents: &[String],
        workspace: &str,
    ) -> Result<(Summary, String), SessionError> {
        let mut unique = Vec::new();
        for agent in agents {
            if agent.is_empty() || unique.contains(agent) {
                continue;
            }
            if self.registry.get(agent).is_none() {
                return Err(SessionError::NoAgent(agent.clone()));
            }
            unique.push(agent.clone());
        }
        if unique.is_empty() {
            return Err(SessionError::NoAgent("none given".into()));
        }

        let secret = random_secret();
        let id = format!("s_{}", short_id(16));
        let mut lanes = BTreeMap::new();
        let mut receivers = Vec::new();
        for agent in &unique {
            let (lane, receiver) = Lane::new(agent.clone());
            lanes.insert(agent.clone(), lane.clone());
            receivers.push((lane, receiver));
        }
        let session = Arc::new(Session {
            id: id.clone(),
            agents: unique.clone(),
            workspace: workspace.to_owned(),
            created: Utc::now(),
            secret_hash: hash_secret(&secret),
            keys: Mutex::new(BTreeMap::new()),
            state: RwLock::new(State::Idle),
            lanes,
            inbox: Mutex::new(Vec::new()),
            speakers: Mutex::new(Vec::new()),
            history: Mutex::new(Vec::new()),
            context: sharedctx::Store::new(),
            context_write: Mutex::new(()),
            rollups: Mutex::new(BTreeSet::new()),
        });
        self.events
            .append(
                &id,
                "session.created",
                "",
                "",
                Some(json!({
                    "agent": unique[0],
                    "agents": unique,
                    "workspace": session.workspace,
                    "secret_sha256": session.secret_hash,
                })),
            )
            .await?;
        self.sessions
            .write()
            .await
            .insert(id.clone(), session.clone());
        for (lane, receiver) in receivers {
            let manager = self.clone();
            let session = session.clone();
            tokio::spawn(async move { manager.lane_loop(session, lane, receiver).await });
        }
        self.warm_acp_lanes(&session);
        Ok((self.summary(&session).await, secret))
    }

    pub async fn submit(
        &self,
        id: &str,
        author: Author,
        text: &str,
        to: &[String],
    ) -> Result<Input, SessionError> {
        let session = self.session(id).await?;
        if *session.state.read().await == State::Closed {
            return Err(SessionError::Closed);
        }
        let mut speakers = session.speakers.lock().await;
        let added_speaker = !speakers.contains(&author.name);
        if added_speaker {
            speakers.push(author.name.clone());
        }
        let decision = who_wakes(text, to, &session.agents, &speakers, &self.registry);
        let mut inbox = session.inbox.lock().await;
        let input = Input {
            id: format!("in_{}", short_id(12)),
            seq: inbox.len(),
            author: author.name.clone(),
            source: author.source.clone(),
            text: text.to_owned(),
            to: to.to_vec(),
            wake: decision.agents,
            why: decision.why,
            withdrawn: false,
        };
        let appended = self
            .events
            .append(
                id,
                "input.submitted",
                &author.name,
                &input.id,
                Some(json!({
                    "text": input.text,
                    "seq": input.seq,
                    "group": input.id,
                    "to": input.to,
                    "wake": input.wake,
                    "why": input.why,
                    "source": input.source,
                })),
            )
            .await;
        if let Err(error) = appended {
            if added_speaker {
                speakers.retain(|speaker| speaker != &author.name);
            }
            return Err(error.into());
        }
        inbox.push(input.clone());
        drop(inbox);
        drop(speakers);
        for lane in session.lanes.values() {
            lane.nudge().await;
        }
        Ok(input)
    }

    pub async fn withdraw(
        &self,
        id: &str,
        input_id: &str,
        actor: &str,
    ) -> Result<(), SessionError> {
        let session = self.session(id).await?;
        let mut inbox = session.inbox.lock().await;
        let Some(_) = inbox
            .iter()
            .find(|input| input.id == input_id && !input.withdrawn)
        else {
            return Err(SessionError::NoTurn);
        };
        self.events
            .append(id, "input.withdrawn", actor, input_id, None)
            .await?;
        let input = inbox
            .iter_mut()
            .find(|input| input.id == input_id)
            .expect("input remained while inbox lock was held");
        input.withdrawn = true;
        Ok(())
    }

    /// Stops running turns: one agent's, or the whole room's.
    ///
    /// Naming one matters because the lanes are independent. Stopping the room
    /// to stop one agent throws away the work every other agent had in flight,
    /// and in a room that exists to run several at once that is the common
    /// case, not the rare one.
    ///
    /// The name is matched the way a person writes it, so `codex` stops
    /// `codex-local` — the same rule that decides who a message wakes.
    pub async fn cancel(
        &self,
        id: &str,
        actor: &str,
        agent: Option<&str>,
    ) -> Result<(), SessionError> {
        let session = self.session(id).await?;
        let wanted = agent.map(str::to_ascii_lowercase);
        let mut named = Vec::new();
        let mut cancellations = Vec::new();
        for lane in session.lanes.values() {
            if let Some(wanted) = &wanted
                && !crate::session::names_for(&lane.agent).contains(wanted)
            {
                continue;
            }
            named.push(lane.agent.clone());
            if let Some(cancel) = lane.state.lock().await.cancel.clone() {
                cancellations.push((lane.agent.clone(), cancel));
            }
        }
        // "That agent is not in this room" and "that agent is not doing
        // anything" are different mistakes and need different answers.
        if let Some(wanted) = &wanted
            && named.is_empty()
        {
            return Err(SessionError::NoAgent(wanted.clone()));
        }
        if cancellations.is_empty() {
            return Err(SessionError::NoTurn);
        }
        let stopping = cancellations
            .iter()
            .map(|(agent, _)| agent.clone())
            .collect::<Vec<_>>();
        self.events
            .append(
                id,
                "turn.cancel_requested",
                actor,
                "",
                Some(json!({"lanes": cancellations.len(), "agents": stopping})),
            )
            .await?;
        // Only the interactions belonging to the lanes being stopped. Answering
        // for an agent that was left running would strand its turn on a
        // decision nobody made.
        for interaction in self.executor.pending_interactions(id).await {
            if !stopping.contains(&interaction.agent) {
                continue;
            }
            self.events
                .append(
                    id,
                    "interaction.cancel_requested",
                    actor,
                    &interaction.turn,
                    Some(json!({"interaction": interaction.id})),
                )
                .await?;
        }
        for (_, cancellation) in cancellations {
            cancellation.cancel();
        }
        Ok(())
    }

    pub async fn pending_interactions(
        &self,
        id: &str,
    ) -> Result<Vec<PendingInteraction>, SessionError> {
        self.session(id).await?;
        Ok(self.executor.pending_interactions(id).await)
    }

    pub async fn resolve_interaction(
        &self,
        id: &str,
        interaction: &str,
        actor: &str,
        option_id: Option<&str>,
    ) -> Result<PendingInteraction, SessionError> {
        self.session(id).await?;
        let pending = self
            .executor
            .interaction(id, interaction)
            .await
            .ok_or(SessionError::NoInteraction)?;
        if let Some(option_id) = option_id
            && !pending.options.iter().any(|option| option.id == option_id)
        {
            return Err(SessionError::InvalidInteraction(format!(
                "option {option_id:?} was not offered by the agent"
            )));
        }
        self.events
            .append(
                id,
                "interaction.resolution_requested",
                actor,
                &pending.turn,
                Some(json!({
                    "interaction": interaction,
                    "option_id": option_id,
                    "cancelled": option_id.is_none(),
                })),
            )
            .await?;
        let resolved = self
            .executor
            .resolve_interaction(id, interaction, option_id)
            .await
            .map_err(|error| SessionError::InvalidInteraction(error.to_string()))?;
        self.events
            .append(
                id,
                "interaction.resolved",
                actor,
                &resolved.turn,
                Some(json!({
                    "interaction": interaction,
                    "option_id": option_id,
                    "cancelled": option_id.is_none(),
                })),
            )
            .await?;
        Ok(resolved)
    }

    pub async fn close(&self, id: &str, actor: &str) -> Result<(), SessionError> {
        let session = self.session(id).await?;
        let mut state = session.state.write().await;
        if *state == State::Closed {
            return Ok(());
        }
        self.events
            .append(id, "session.closed", actor, "", None)
            .await?;
        *state = State::Closed;
        drop(state);
        for lane in session.lanes.values() {
            if let Some(cancel) = lane.state.lock().await.cancel.clone() {
                cancel.cancel();
            }
            lane.nudge().await;
        }
        for (agent, lane) in &session.lanes {
            let Some(spec) = self.registry.get(agent) else {
                continue;
            };
            let lane_state = lane.state.lock().await;
            let context = RenderContext {
                conversation: session.id.clone(),
                agent: agent.clone(),
                agents: session.agents.clone(),
                handle: lane_state.handle.clone(),
                ..Default::default()
            };
            drop(lane_state);
            self.executor.close(&spec, &context).await;
        }
        Ok(())
    }

    pub async fn exists(&self, id: &str) -> bool {
        self.sessions.read().await.contains_key(id)
    }

    pub async fn used_by(&self, agent: &str) -> Vec<String> {
        let sessions = self
            .sessions
            .read()
            .await
            .values()
            .cloned()
            .collect::<Vec<_>>();
        let mut ids = Vec::new();
        for session in sessions {
            if *session.state.read().await != State::Closed
                && session.agents.iter().any(|candidate| candidate == agent)
            {
                ids.push(session.id.clone());
            }
        }
        ids.sort();
        ids
    }

    pub async fn list(&self) -> Vec<Summary> {
        let sessions = self
            .sessions
            .read()
            .await
            .values()
            .cloned()
            .collect::<Vec<_>>();
        let mut output = Vec::new();
        for session in sessions {
            output.push(self.summary(&session).await);
        }
        output.sort_by(|left, right| {
            right
                .created
                .cmp(&left.created)
                .then(left.id.cmp(&right.id))
        });
        output
    }

    pub async fn view(&self, id: &str) -> Result<View, SessionError> {
        let session = self.session(id).await?;
        let inbox = session.inbox.lock().await.clone();
        let mut furthest = inbox.len();
        let mut current = None;
        for lane in session.lanes.values() {
            let state = lane.state.lock().await;
            if current.is_none() {
                current = state.current.clone();
            }
            furthest = furthest.min(state.cursor);
        }
        Ok(View {
            summary: self.summary(&session).await,
            waiting: inbox[furthest..]
                .iter()
                .filter(|input| !input.withdrawn)
                .cloned()
                .collect(),
            current,
            history: session.history.lock().await.clone(),
        })
    }

    pub async fn context(&self, id: &str) -> Result<Vec<sharedctx::Entry>, SessionError> {
        Ok(self.session(id).await?.context.snapshot())
    }

    pub async fn append_context(
        self: &Arc<Self>,
        id: &str,
        key: &str,
        by: &str,
        text: &str,
    ) -> Result<sharedctx::Entry, SessionError> {
        let session = self.session(id).await?;
        let event = self
            .events
            .append(
                id,
                "context.appended",
                by,
                "",
                Some(json!({"key": key, "text": text})),
            )
            .await?;
        let entry = session.context.append(key, by, text, event.seq, event.ts)?;
        self.maybe_rollup(session, key).await;
        Ok(entry)
    }

    pub async fn set_context(
        &self,
        id: &str,
        key: &str,
        by: &str,
        value: &str,
        if_match: i64,
    ) -> Result<sharedctx::Entry, SessionError> {
        let session = self.session(id).await?;
        let _guard = session.context_write.lock().await;
        let current = session.context.get(key);
        let conflict = current
            .as_ref()
            .is_some_and(|entry| if_match != -1 && entry.version != if_match)
            || (current.is_none() && if_match > 0);
        if conflict {
            let version = current.as_ref().map_or(0, |entry| entry.version);
            self.events
                .append(
                    id,
                    "conflict.rejected",
                    by,
                    "",
                    Some(json!({"key": key, "expected": if_match, "current": version})),
                )
                .await?;
            return Err(ContextError::Conflict {
                key: key.into(),
                current: current
                    .as_ref()
                    .map_or_else(String::new, |entry| entry.value.clone()),
                version,
                by: current.map_or_else(String::new, |entry| entry.by),
            }
            .into());
        }
        let event = self
            .events
            .append(
                id,
                "context.set",
                by,
                "",
                Some(json!({"key": key, "value": value})),
            )
            .await?;
        Ok(session.context.set(key, by, value, -1, event.seq)?)
    }

    pub async fn pin_context(
        &self,
        id: &str,
        key: &str,
        by: &str,
        pinned: bool,
    ) -> Result<sharedctx::Entry, SessionError> {
        let session = self.session(id).await?;
        if session.context.get(key).is_none() {
            return Err(ContextError::NotFound(key.into()).into());
        }
        let kind = if pinned {
            "context.pinned"
        } else {
            "context.unpinned"
        };
        self.events
            .append(id, kind, by, "", Some(json!({"key": key})))
            .await?;
        Ok(session.context.pin(key, pinned)?)
    }

    pub async fn authorize(&self, id: &str, secret: &str) -> bool {
        let Ok(session) = self.session(id).await else {
            return false;
        };
        constant_time_eq(&hash_secret(secret), &session.secret_hash)
    }

    pub async fn resolve(&self, id: &str, secret: &str) -> Option<String> {
        let session = self.session(id).await.ok()?;
        let got = hash_secret(secret);
        if constant_time_eq(&got, &session.secret_hash) {
            return Some(String::new());
        }
        let keys = session.keys.lock().await;
        let mut matched = None;
        for (name, hash) in keys.iter() {
            if constant_time_eq(&got, hash) {
                matched = Some(name.clone());
            }
        }
        matched
    }

    pub async fn issue_key(&self, id: &str, name: &str) -> Result<(String, String), SessionError> {
        let name = name.trim();
        if name.is_empty() {
            return Err(SessionError::NoAgent(
                "a key has to be for somebody: name is required".into(),
            ));
        }
        self.session(id).await?;
        let key = random_secret();
        let hash = hash_secret(&key);
        Ok((key, hash))
    }

    pub async fn record_invite(
        &self,
        id: &str,
        by: &str,
        author: &str,
        hash: &str,
    ) -> Result<(), SessionError> {
        self.events
            .append(
                id,
                "participant.invited",
                by,
                "",
                Some(json!({"author": author, "key_sha256": hash})),
            )
            .await?;
        let session = self.session(id).await?;
        session.keys.lock().await.insert(author.into(), hash.into());
        Ok(())
    }

    async fn session(&self, id: &str) -> Result<Arc<Session>, SessionError> {
        self.sessions
            .read()
            .await
            .get(id)
            .cloned()
            .ok_or(SessionError::NoSession)
    }

    async fn summary(&self, session: &Session) -> Summary {
        let state = *session.state.read().await;
        let mut handles = BTreeMap::new();
        let mut running = false;
        for (agent, lane) in &session.lanes {
            let lane_state = lane.state.lock().await;
            running |= lane_state.current.is_some();
            if !lane_state.handle.is_empty() {
                handles.insert(agent.clone(), lane_state.handle.clone());
            }
        }
        let state = if state == State::Closed {
            State::Closed
        } else if running {
            State::Running
        } else {
            State::Idle
        };
        let agent = session.agents.first().cloned().unwrap_or_default();
        Summary {
            id: session.id.clone(),
            handle: handles.get(&agent).cloned().unwrap_or_default(),
            agent,
            agents: session.agents.clone(),
            workspace: session.workspace.clone(),
            state,
            handles,
            created: session.created,
        }
    }

    async fn lane_loop(
        self: Arc<Self>,
        session: Arc<Session>,
        lane: Arc<Lane>,
        mut wake: mpsc::Receiver<()>,
    ) {
        while wake.recv().await.is_some() {
            if *session.state.read().await == State::Closed {
                return;
            }
            loop {
                let inbox = session.inbox.lock().await.clone();
                let Some(mut turn) = lane.take(&inbox).await else {
                    break;
                };
                self.run_turn(&session, &lane, &mut turn).await;
                lane.finish(&turn).await;
                session.history.lock().await.push(turn);
            }
        }
    }

    async fn run_turn(self: &Arc<Self>, session: &Session, lane: &Lane, turn: &mut Turn) {
        let Some(spec) = self.registry.get(&lane.agent) else {
            turn.state = TurnState::Failed;
            turn.error = format!("agent no longer registered: {}", lane.agent);
            let _ = self
                .events
                .append(
                    &session.id,
                    "turn.failed",
                    &lane.agent,
                    &turn.id,
                    Some(json!({"error": turn.error})),
                )
                .await;
            return;
        };

        let (context_view, digest) = context_snapshot(&session.context, &spec.context_refs());
        let input_ids = turn
            .inputs
            .iter()
            .map(|input| input.id.clone())
            .collect::<Vec<_>>();
        let _ = self
            .events
            .append(
                &session.id,
                "turn.started",
                &turn.author,
                &turn.id,
                Some(json!({"text": turn.text, "agent": lane.agent, "context": digest, "inputs": input_ids})),
            )
            .await;

        let cancel = CancellationToken::new();
        let mut lane_state = lane.state.lock().await;
        lane_state.cancel = Some(cancel.clone());
        let mut context = RenderContext {
            input: turn.text.clone(),
            turn: turn.id.clone(),
            conversation: session.id.clone(),
            handle: lane_state.handle.clone(),
            agent: lane.agent.clone(),
            agents: session.agents.clone(),
            workspace: session.workspace.clone(),
            vars: spec.vars.clone(),
            captures: lane_state.captures.clone(),
            context: context_view,
            ..Default::default()
        };
        let needs_open = (spec.open.is_some() || spec.is_acp()) && !lane_state.opened;
        drop(lane_state);

        if needs_open {
            let opened = tokio::select! {
                _ = cancel.cancelled() => None,
                result = tokio::time::timeout(spec.timeout_duration(), self.executor.open(&spec, &context)) => {
                    match result {
                        Ok(Ok(value)) => Some(Ok(value)),
                        Ok(Err(error)) => Some(Err(error.to_string())),
                        Err(_) => Some(Err("open timed out".into())),
                    }
                }
            };
            match opened {
                None => {
                    self.cancel_turn(session, lane, turn).await;
                    return;
                }
                Some(Err(error)) => {
                    self.fail_turn(session, lane, turn, error).await;
                    return;
                }
                Some(Ok((handle, captures))) => {
                    let mut lane_state = lane.state.lock().await;
                    lane_state.opened = true;
                    lane_state.handle = handle.clone();
                    lane_state.captures = captures.clone();
                    context.handle = handle.clone();
                    context.captures = captures;
                    let _ = self
                        .events
                        .append(
                            &session.id,
                            "session.opened",
                            &lane.agent,
                            &turn.id,
                            Some(json!({
                                "handle": handle,
                                "agent": lane.agent,
                                "captures": context.captures.clone(),
                            })),
                        )
                        .await;
                }
            }
        }

        let (parts_tx, mut parts_rx) = mpsc::unbounded_channel::<Part>();
        let executor = self.executor.clone();
        let spec_for_task = spec.clone();
        let context_for_task = context.clone();
        let timeout = spec.timeout_duration();
        let transport_cancel = cancel.child_token();
        let mut task = tokio::spawn(async move {
            let result = tokio::time::timeout(
                timeout,
                executor.turn_cancellable(
                    &spec_for_task,
                    &context_for_task,
                    transport_cancel.clone(),
                    |part| {
                        let _ = parts_tx.send(part);
                    },
                ),
            )
            .await;
            match result {
                Ok(result) => result.map_err(|error| error.to_string()),
                Err(_) => {
                    transport_cancel.cancel();
                    Err("turn timed out".to_owned())
                }
            }
        });

        let mut output = String::new();
        let mut raw = Vec::new();
        let outcome = loop {
            tokio::select! {
                _ = cancel.cancelled() => {
                    task.abort();
                    break Err(None);
                }
                Some(part) = parts_rx.recv() => {
                    if part.kind == "text" {
                        output.push_str(&part.text);
                    }
                    if let Some(value) = &part.raw {
                        raw.push(value.clone());
                    }
                    self.record_part(session, lane, turn, &part).await;
                }
                result = &mut task => {
                    while let Ok(part) = parts_rx.try_recv() {
                        if part.kind == "text" { output.push_str(&part.text); }
                        if let Some(value) = &part.raw { raw.push(value.clone()); }
                        self.record_part(session, lane, turn, &part).await;
                    }
                    break match result {
                        Ok(Ok(())) => Ok(()),
                        Ok(Err(error)) => Err(Some(error)),
                        Err(error) => Err(Some(error.to_string())),
                    };
                }
            }
        };

        match outcome {
            Err(None) => self.cancel_turn(session, lane, turn).await,
            Err(Some(error)) => self.fail_turn(session, lane, turn, error).await,
            Ok(()) => {
                turn.state = TurnState::Done;
                turn.output = output.clone();
                // Before `turn.finished`, not after. Everything downstream
                // treats that event as "this turn is done" and acts on it: the
                // room view, `send --follow`, the editor seat. Writing what the
                // turn produced afterwards means a message sent the instant a
                // turn ends is answered against the context of the turn before
                // it — which reads as an agent that cannot see what the agent
                // beside it just said, intermittently.
                self.apply_context_rules(&session.id, &spec, &lane.agent, &output, &raw)
                    .await;
                let _ = self
                    .events
                    .append(
                        &session.id,
                        "turn.finished",
                        &lane.agent,
                        &turn.id,
                        Some(json!({"chars": output.len()})),
                    )
                    .await;
                if output.trim().is_empty() {
                    let _ = self.events.append(&session.id, "turn.empty", &lane.agent, &turn.id, Some(json!({"parts": raw.len(), "reason": "the agent sent no readable text"}))).await;
                }
            }
        }
    }

    async fn fail_turn(&self, session: &Session, lane: &Lane, turn: &mut Turn, error: String) {
        turn.state = TurnState::Failed;
        turn.error = error.clone();
        let _ = self
            .events
            .append(
                &session.id,
                "turn.failed",
                &lane.agent,
                &turn.id,
                Some(json!({"error": error})),
            )
            .await;
    }

    /// Answers one request on the agent's own terms, under `--express`.
    ///
    /// Deliberately the agent's `allow_once` where it offers one, in preference
    /// to `allow_always`: always is the agent deciding it need not ask again,
    /// which takes the next action out of the log entirely. Once keeps every
    /// action a recorded request with a recorded answer, which is the only
    /// thing that makes this reviewable afterwards.
    ///
    /// An agent offering no allow option at all is left waiting for a person.
    /// Express is a standing yes to what an agent asks for, not a licence to
    /// invent an answer it did not offer.
    async fn express_resolve(&self, session: &str, interaction: &str) {
        let Some(pending) = self.executor.interaction(session, interaction).await else {
            return;
        };
        let chosen = pending
            .options
            .iter()
            .find(|option| option.kind == "allow_once")
            .or_else(|| {
                pending
                    .options
                    .iter()
                    .find(|option| option.kind.starts_with("allow"))
            });
        let Some(chosen) = chosen else {
            let _ = self
                .events
                .append(
                    session,
                    "interaction.express_declined",
                    "express",
                    &pending.turn,
                    Some(json!({
                        "interaction": pending.id,
                        "reason": "the agent offered no allow option",
                    })),
                )
                .await;
            return;
        };
        let _ = self
            .resolve_interaction(session, interaction, "express", Some(&chosen.id))
            .await;
    }

    async fn cancel_turn(&self, session: &Session, lane: &Lane, turn: &mut Turn) {
        turn.state = TurnState::Cancelled;
        turn.error = "cancelled".into();
        let _ = self
            .events
            .append(&session.id, "turn.cancelled", &lane.agent, &turn.id, None)
            .await;
    }

    async fn record_part(&self, session: &Session, lane: &Lane, turn: &Turn, part: &Part) {
        lane.apply_part(&turn.id, part).await;
        if let Some(interaction) = &part.interaction {
            let _ = self
                .events
                .append(
                    &session.id,
                    "interaction.requested",
                    &lane.agent,
                    &turn.id,
                    Some(json!({
                        "interaction": interaction,
                        "kind": part.kind,
                        "request": part.raw,
                    })),
                )
                .await;
            if self.express {
                self.express_resolve(&session.id, interaction).await;
            }
        }
        let _ = self
            .events
            .append(
                &session.id,
                "output.part",
                &lane.agent,
                &turn.id,
                serde_json::to_value(part).ok(),
            )
            .await;
    }

    async fn apply_context_rules(
        self: &Arc<Self>,
        id: &str,
        spec: &Spec,
        agent: &str,
        output: &str,
        chunks: &[Value],
    ) {
        for rule in &spec.context {
            let values = rule_values(rule, output, chunks);
            if values.is_empty() {
                continue;
            }
            let write = if rule.kind() == "value" {
                self.set_context(id, &rule.key, agent, values.last().expect("nonempty"), -1)
                    .await
            } else {
                let mut result = None;
                for value in values {
                    result = Some(self.append_context(id, &rule.key, agent, &value).await);
                    if result.as_ref().is_some_and(Result::is_err) {
                        break;
                    }
                }
                result.unwrap_or_else(|| unreachable!())
            };
            match write {
                Ok(entry) if rule.pin && !entry.pinned => {
                    if let Err(error) = self.pin_context(id, &rule.key, agent, true).await {
                        let _ = self
                            .events
                            .append(
                                id,
                                "context.rejected",
                                agent,
                                "",
                                Some(json!({"key": rule.key, "error": error.to_string()})),
                            )
                            .await;
                    }
                }
                Err(error) => {
                    let _ = self
                        .events
                        .append(
                            id,
                            "context.rejected",
                            agent,
                            "",
                            Some(json!({"key": rule.key, "error": error.to_string()})),
                        )
                        .await;
                }
                _ => {}
            }
        }
    }

    async fn maybe_rollup(self: &Arc<Self>, session: Arc<Session>, key: &str) {
        const ROLLUP_AFTER: usize = 10;
        if self.summariser.is_empty() {
            return;
        }
        let Some((items, through)) = session.context.needs_rollup(key, ROLLUP_AFTER) else {
            return;
        };
        let Some(spec) = self.registry.get(&self.summariser) else {
            let _ = self
                .events
                .append(
                    &session.id,
                    "rollup.failed",
                    &self.summariser,
                    "",
                    Some(json!({"key": key, "error": "summariser is not a registered connector"})),
                )
                .await;
            return;
        };
        {
            let mut running = session.rollups.lock().await;
            if !running.insert(key.to_owned()) {
                return;
            }
        }

        let manager = self.clone();
        let key = key.to_owned();
        tokio::spawn(async move {
            let result = manager.summarise(&spec, &items).await;
            match result {
                Ok(text) if !text.trim().is_empty() => {
                    match manager
                        .events
                        .append(
                            &session.id,
                            "context.rolled_up",
                            &manager.summariser,
                            "",
                            Some(json!({
                                "key": key,
                                "text": text.trim(),
                                "covers": items.len(),
                                "through": through,
                            })),
                        )
                        .await
                    {
                        Ok(_) => {
                            let _ = session.context.set_rollup(
                                &key,
                                text.trim(),
                                &manager.summariser,
                                items.len(),
                                through,
                            );
                        }
                        Err(error) => eprintln!("oryxa: context rollup event failed: {error}"),
                    }
                }
                Ok(_) => {
                    let _ = manager
                        .events
                        .append(
                            &session.id,
                            "rollup.failed",
                            &manager.summariser,
                            "",
                            Some(json!({"key": key, "error": "summariser returned nothing"})),
                        )
                        .await;
                }
                Err(error) => {
                    let _ = manager
                        .events
                        .append(
                            &session.id,
                            "rollup.failed",
                            &manager.summariser,
                            "",
                            Some(json!({"key": key, "error": error.to_string()})),
                        )
                        .await;
                }
            }
            session.rollups.lock().await.remove(&key);
        });
    }

    async fn summarise(&self, spec: &Spec, items: &[sharedctx::Item]) -> anyhow::Result<String> {
        let input = items
            .iter()
            .map(|item| format!("- {}: {}", item.by, item.text))
            .collect::<Vec<_>>()
            .join("\n");
        let context = RenderContext {
            input,
            turn: format!("rollup_{}", short_id(12)),
            conversation: format!("rollup_{}", short_id(12)),
            agent: spec.name.clone(),
            vars: spec.vars.clone(),
            ..Default::default()
        };
        let mut output = String::new();
        tokio::time::timeout(
            spec.timeout_duration(),
            self.executor.turn(spec, &context, |part| {
                if part.kind == "text" {
                    output.push_str(&part.text);
                }
            }),
        )
        .await
        .map_err(|_| anyhow::anyhow!("summariser timed out"))??;
        Ok(output.trim().to_owned())
    }
}

#[derive(Default, Deserialize)]
struct FoldData {
    #[serde(default)]
    agent: String,
    #[serde(default)]
    agents: Vec<String>,
    #[serde(default)]
    workspace: String,
    #[serde(default)]
    handle: String,
    #[serde(default)]
    text: String,
    #[serde(default)]
    value: String,
    #[serde(default)]
    key: String,
    #[serde(default)]
    kind: String,
    #[serde(default)]
    error: String,
    #[serde(default)]
    source: String,
    #[serde(default)]
    to: Vec<String>,
    #[serde(default)]
    wake: Vec<String>,
    #[serde(default)]
    why: String,
    #[serde(default)]
    inputs: Vec<String>,
    #[serde(default)]
    secret_sha256: String,
    #[serde(default)]
    author: String,
    #[serde(default)]
    key_sha256: String,
    #[serde(default)]
    covers: usize,
    #[serde(default)]
    through: i64,
}

async fn rebuild_session(
    id: &str,
    events: &[crate::events::Event],
) -> Option<(Arc<Session>, Vec<(Arc<Lane>, mpsc::Receiver<()>)>)> {
    let first = events.first()?;
    let mut agents = Vec::new();
    let mut workspace = String::new();
    let mut created = first.ts;
    let mut secret_hash = String::new();
    let mut keys = BTreeMap::new();
    let mut state = State::Idle;
    let mut inbox = Vec::<Input>::new();
    let mut speakers = Vec::<String>::new();
    let mut turns = BTreeMap::<String, Turn>::new();
    let mut turn_order = Vec::<String>::new();
    let mut lane_cursor = BTreeMap::<String, usize>::new();
    let mut handles = BTreeMap::<String, String>::new();
    let context = sharedctx::Store::new();

    for event in events {
        let data = event
            .data
            .clone()
            .and_then(|value| serde_json::from_value::<FoldData>(value).ok())
            .unwrap_or_default();
        match event.kind.as_str() {
            "session.created" => {
                created = event.ts;
                secret_hash = data.secret_sha256;
                workspace = data.workspace;
                agents = if data.agents.is_empty() {
                    (!data.agent.is_empty())
                        .then_some(data.agent)
                        .into_iter()
                        .collect()
                } else {
                    data.agents
                };
            }
            "session.opened" => {
                let agent = if data.agent.is_empty() {
                    event.actor.clone()
                } else {
                    data.agent
                };
                if !agent.is_empty() {
                    handles.insert(agent, data.handle);
                }
            }
            "participant.invited" if !data.author.is_empty() && !data.key_sha256.is_empty() => {
                keys.insert(data.author, data.key_sha256);
            }
            "session.closed" => state = State::Closed,
            "input.submitted" => {
                if !speakers.contains(&event.actor) {
                    speakers.push(event.actor.clone());
                }
                inbox.push(Input {
                    id: event.turn.clone(),
                    seq: inbox.len(),
                    author: event.actor.clone(),
                    source: data.source,
                    text: data.text,
                    to: data.to,
                    wake: data.wake,
                    why: data.why,
                    withdrawn: false,
                });
            }
            "input.withdrawn" => {
                if let Some(input) = inbox.iter_mut().find(|input| input.id == event.turn) {
                    input.withdrawn = true;
                }
            }
            "turn.started" if !turns.contains_key(&event.turn) => {
                let inputs = data
                    .inputs
                    .iter()
                    .filter_map(|wanted| inbox.iter().find(|input| &input.id == wanted).cloned())
                    .collect::<Vec<_>>();
                let group = inputs
                    .last()
                    .map(|input| input.id.clone())
                    .unwrap_or_else(|| event.turn.clone());
                turns.insert(
                    event.turn.clone(),
                    Turn {
                        id: event.turn.clone(),
                        agent: data.agent,
                        state: TurnState::Running,
                        inputs,
                        author: event.actor.clone(),
                        text: data.text,
                        error: String::new(),
                        output: String::new(),
                        group,
                    },
                );
                turn_order.push(event.turn.clone());
            }
            "output.part" if data.kind == "text" => {
                if let Some(turn) = turns.get_mut(&event.turn) {
                    turn.output.push_str(&data.text);
                }
            }
            "turn.finished" => {
                if let Some(turn) = turns.get_mut(&event.turn) {
                    turn.state = TurnState::Done;
                }
            }
            "turn.failed" => {
                if let Some(turn) = turns.get_mut(&event.turn) {
                    turn.state = TurnState::Failed;
                    turn.error = data.error;
                }
            }
            "turn.cancelled" => {
                if let Some(turn) = turns.get_mut(&event.turn) {
                    turn.state = TurnState::Cancelled;
                    turn.error = "cancelled".into();
                }
            }
            "context.appended" => {
                let _ = context.append(&data.key, &event.actor, &data.text, event.seq, event.ts);
            }
            "context.set" => {
                let _ = context.set(&data.key, &event.actor, &data.value, -1, event.seq);
            }
            "context.pinned" => {
                let _ = context.pin(&data.key, true);
            }
            "context.unpinned" => {
                let _ = context.pin(&data.key, false);
            }
            "context.rolled_up" => {
                let _ = context.set_rollup(
                    &data.key,
                    &data.text,
                    &event.actor,
                    data.covers,
                    data.through,
                );
            }
            _ => {}
        }
    }
    if agents.is_empty() {
        return None;
    }

    let mut history = Vec::new();
    for id in turn_order {
        let Some(mut turn) = turns.remove(&id) else {
            continue;
        };
        let cursor = turn
            .inputs
            .iter()
            .map(|input| input.seq + 1)
            .max()
            .unwrap_or(0);
        lane_cursor
            .entry(turn.agent.clone())
            .and_modify(|value| *value = (*value).max(cursor))
            .or_insert(cursor);
        if turn.state == TurnState::Running {
            turn.state = TurnState::Failed;
            turn.error = "interrupted by restart; outcome unknown".into();
        }
        history.push(turn);
    }

    let mut lanes = BTreeMap::new();
    let mut receivers = Vec::new();
    for agent in &agents {
        let (lane, receiver) = Lane::new(agent.clone());
        {
            let mut lane_state = lane.state.lock().await;
            lane_state.cursor = lane_cursor.get(agent).copied().unwrap_or(0);
            if let Some(handle) = handles.get(agent) {
                lane_state.handle = handle.clone();
                lane_state.opened = true;
            }
        }
        lanes.insert(agent.clone(), lane.clone());
        receivers.push((lane, receiver));
    }

    Some((
        Arc::new(Session {
            id: id.to_owned(),
            agents,
            workspace,
            created,
            secret_hash,
            keys: Mutex::new(keys),
            state: RwLock::new(state),
            lanes,
            inbox: Mutex::new(inbox),
            speakers: Mutex::new(speakers),
            history: Mutex::new(history),
            context,
            context_write: Mutex::new(()),
            rollups: Mutex::new(BTreeSet::new()),
        }),
        receivers,
    ))
}

fn rule_values(rule: &ContextRule, output: &str, chunks: &[Value]) -> Vec<String> {
    if rule.from_text() {
        return (!output.trim().is_empty())
            .then(|| output.trim().to_owned())
            .into_iter()
            .collect();
    }
    let mut values = chunks
        .iter()
        .filter(|chunk| rule.when.is_empty() || crate::connector::matches(chunk, &rule.when))
        .flat_map(|chunk| crate::connector::select(chunk, &rule.from))
        .map(|value| value.trim().to_owned())
        .filter(|value| !value.is_empty())
        .collect::<Vec<_>>();
    if rule.last && values.len() > 1 {
        values = values.split_off(values.len() - 1);
    }
    values
}

fn context_snapshot(store: &sharedctx::Store, refs: &[String]) -> (ContextView, Value) {
    let entries = store.snapshot();
    let pinned = entries
        .iter()
        .filter(|entry| entry.pinned)
        .cloned()
        .collect::<Vec<_>>();
    let keys = entries
        .iter()
        .map(|entry| (entry.key.clone(), sharedctx::render_entry(entry)))
        .collect::<BTreeMap<_, _>>();
    let view = ContextView {
        all: sharedctx::render(&entries),
        pinned: sharedctx::render(&pinned),
        keys,
    };
    let mut chars = 0;
    let mut elided = 0;
    for reference in refs {
        match reference.as_str() {
            "context" => {
                chars += view.all.len();
                elided += sharedctx::elided(&entries);
            }
            "context.pinned" => {
                chars += view.pinned.len();
                elided += sharedctx::elided(&pinned);
            }
            other => {
                let key = other.trim_start_matches("context.");
                chars += view.keys.get(key).map_or(0, String::len);
                if let Some(entry) = entries.iter().find(|entry| entry.key == key) {
                    elided += sharedctx::elided(std::slice::from_ref(entry));
                }
            }
        }
    }
    let version = entries.iter().map(|entry| entry.version).max().unwrap_or(0);
    let digest = json!({
        "version": version,
        "keys": entries.iter().map(|entry| entry.key.clone()).collect::<Vec<_>>(),
        "reads": refs,
        "chars": chars,
        "elided": elided,
    });
    (view, digest)
}

fn random_secret() -> String {
    use rand::RngCore;
    let mut bytes = [0_u8; 32];
    rand::rngs::OsRng.fill_bytes(&mut bytes);
    hex::encode(bytes)
}

fn hash_secret(secret: &str) -> String {
    hex::encode(Sha256::digest(secret.as_bytes()))
}

fn constant_time_eq(left: &str, right: &str) -> bool {
    left.as_bytes().ct_eq(right.as_bytes()).into()
}

#[cfg(test)]
mod tests {
    use std::{
        collections::BTreeMap,
        sync::atomic::{AtomicBool, AtomicUsize, Ordering},
    };

    use axum::{Json, Router, routing::post};
    use serde_json::json;
    use tokio::{
        net::TcpListener,
        time::{Duration, sleep},
    };

    use crate::{
        connector::{Origin, ResponseSpec, Step},
        events::MemoryStore,
    };

    use super::*;

    struct SwitchStore {
        fail: AtomicBool,
        inner: MemoryStore,
    }

    impl SwitchStore {
        fn new() -> Self {
            Self {
                fail: AtomicBool::new(false),
                inner: MemoryStore::new(),
            }
        }
    }

    #[async_trait::async_trait]
    impl crate::events::Store for SwitchStore {
        async fn append(
            &self,
            session: &str,
            kind: &str,
            actor: &str,
            turn: &str,
            data: Option<Value>,
        ) -> Result<crate::events::Event, StoreError> {
            if self.fail.load(Ordering::SeqCst) {
                return Err(StoreError::Message("storage unavailable".into()));
            }
            self.inner.append(session, kind, actor, turn, data).await
        }

        async fn since(
            &self,
            session: &str,
            since: i64,
        ) -> Result<Vec<crate::events::Event>, StoreError> {
            self.inner.since(session, since).await
        }

        fn subscribe(&self) -> tokio::sync::broadcast::Receiver<crate::events::Event> {
            self.inner.subscribe()
        }

        async fn sessions(&self) -> Result<Vec<String>, StoreError> {
            self.inner.sessions().await
        }

        async fn reset(&self) -> Result<usize, StoreError> {
            self.inner.reset().await
        }
    }

    async fn agent() -> String {
        async fn turn(Json(body): Json<Value>) -> Json<Value> {
            Json(
                json!({"output": format!("answer: {}", body["prompt"].as_str().unwrap_or_default())}),
            )
        }
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let address = listener.local_addr().unwrap();
        tokio::spawn(async move {
            axum::serve(listener, Router::new().route("/run", post(turn)))
                .await
                .unwrap()
        });
        format!("http://{address}")
    }

    async fn setup(names: &[&str]) -> (Arc<Manager>, String, String) {
        let registry = Registry::new();
        let base = agent().await;
        for name in names {
            registry
                .put(Spec {
                    name: (*name).into(),
                    base: base.clone(),
                    acp: None,
                    headers: BTreeMap::new(),
                    vars: BTreeMap::new(),
                    capabilities: Vec::new(),
                    interests: Vec::new(),
                    timeout: "2s".into(),
                    open: None,
                    turn: Step {
                        path: "/run".into(),
                        body: Some(json!({"prompt": "{{input}}"})),
                        response: Some(ResponseSpec {
                            format: "json".into(),
                            text: vec!["$.output".into()],
                            ..Default::default()
                        }),
                        ..Default::default()
                    },
                    context: Vec::new(),
                    origin: Origin::File,
                })
                .unwrap();
        }
        let manager = Manager::new(registry, Executor::new(), Arc::new(MemoryStore::new()));
        let agents = names.iter().map(|name| (*name).into()).collect::<Vec<_>>();
        let (summary, secret) = manager.create(&agents).await.unwrap();
        (manager, summary.id, secret)
    }

    async fn wait_history(manager: &Manager, id: &str, wanted: usize) -> Vec<Turn> {
        for _ in 0..100 {
            let view = manager.view(id).await.unwrap();
            if view.history.len() >= wanted {
                return view.history;
            }
            sleep(Duration::from_millis(10)).await;
        }
        panic!("turn did not finish")
    }

    #[tokio::test]
    async fn one_lane_runs_a_turn_end_to_end() {
        let (manager, id, secret) = setup(&["a"]).await;
        assert!(manager.authorize(&id, &secret).await);
        manager
            .submit(&id, Author::claimed("alice"), "what is next?", &[])
            .await
            .unwrap();
        let history = wait_history(&manager, &id, 1).await;
        assert_eq!(history[0].state, TurnState::Done);
        assert_eq!(history[0].output, "answer: what is next?");
    }

    #[tokio::test]
    async fn live_view_accumulates_text_parts_before_the_turn_finishes() {
        let (lane, _) = Lane::new("a".into());
        let input = Input {
            id: "i_1".into(),
            seq: 0,
            author: "alice".into(),
            source: "claimed".into(),
            text: "hello".into(),
            to: vec!["a".into()],
            wake: vec!["a".into()],
            why: "addressed".into(),
            withdrawn: false,
        };
        let turn = Turn::new("a", vec![input]);
        lane.state.lock().await.current = Some(turn.clone());

        lane.apply_part(
            &turn.id,
            &Part {
                kind: "text".into(),
                text: "live output".into(),
                raw: None,
                interaction: None,
            },
        )
        .await;

        assert_eq!(
            lane.state.lock().await.current.as_ref().unwrap().output,
            "live output"
        );
    }

    #[tokio::test]
    async fn lanes_run_independently_and_context_is_evented() {
        let (manager, id, _) = setup(&["a", "b"]).await;
        manager
            .submit(&id, Author::claimed("alice"), "question?", &[])
            .await
            .unwrap();
        assert_eq!(wait_history(&manager, &id, 2).await.len(), 2);
        manager
            .append_context(&id, "findings", "alice", "one")
            .await
            .unwrap();
        let entry = manager.context(&id).await.unwrap().remove(0);
        assert_eq!(entry.items[0].text, "one");
    }

    #[tokio::test]
    async fn rehydrate_restores_access_history_context_and_lane_position() {
        let registry = Registry::new();
        let base = agent().await;
        registry
            .put(Spec {
                name: "a".into(),
                base,
                acp: None,
                headers: BTreeMap::new(),
                vars: BTreeMap::new(),
                capabilities: Vec::new(),
                interests: Vec::new(),
                timeout: "2s".into(),
                open: None,
                turn: Step {
                    path: "/run".into(),
                    body: Some(json!({"prompt": "{{input}}"})),
                    response: Some(ResponseSpec {
                        format: "json".into(),
                        text: vec!["$.output".into()],
                        ..Default::default()
                    }),
                    ..Default::default()
                },
                context: Vec::new(),
                origin: Origin::File,
            })
            .unwrap();
        let events = Arc::new(MemoryStore::new());
        let original = Manager::new(registry.clone(), Executor::new(), events.clone());
        let (summary, room_secret) = original.create(&["a".into()]).await.unwrap();
        let (person_key, person_hash) = original.issue_key(&summary.id, "alice").await.unwrap();
        original
            .record_invite(&summary.id, "owner", "alice", &person_hash)
            .await
            .unwrap();
        original
            .append_context(&summary.id, "facts", "alice", "one")
            .await
            .unwrap();
        original
            .submit(&summary.id, Author::claimed("alice"), "first", &[])
            .await
            .unwrap();
        wait_history(&original, &summary.id, 1).await;

        let restored = Manager::new(registry, Executor::new(), events);
        assert_eq!(restored.rehydrate().await.unwrap(), 1);
        assert!(restored.authorize(&summary.id, &room_secret).await);
        assert_eq!(
            restored.resolve(&summary.id, &person_key).await.as_deref(),
            Some("alice")
        );
        let view = restored.view(&summary.id).await.unwrap();
        assert_eq!(view.history.len(), 1);
        assert_eq!(view.history[0].output, "answer: first");
        assert_eq!(
            restored.context(&summary.id).await.unwrap()[0].items[0].text,
            "one"
        );

        restored
            .submit(&summary.id, Author::claimed("bob"), "second", &[])
            .await
            .unwrap();
        let history = wait_history(&restored, &summary.id, 2).await;
        assert_eq!(history[1].output, "answer: second");
    }

    #[tokio::test]
    async fn long_context_is_rolled_up_once_and_replayed_as_data() {
        let calls = Arc::new(AtomicUsize::new(0));
        let calls_for_server = calls.clone();
        async fn summarise(
            axum::extract::State(calls): axum::extract::State<Arc<AtomicUsize>>,
            Json(body): Json<Value>,
        ) -> Json<Value> {
            calls.fetch_add(1, Ordering::SeqCst);
            let count = body["prompt"].as_str().unwrap_or_default().lines().count();
            Json(json!({"output": format!("summary of {count} findings")}))
        }
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let address = listener.local_addr().unwrap();
        tokio::spawn(async move {
            axum::serve(
                listener,
                Router::new()
                    .route("/run", post(summarise))
                    .with_state(calls_for_server),
            )
            .await
            .unwrap()
        });

        let registry = Registry::new();
        for name in ["a", "roller"] {
            registry
                .put(Spec {
                    name: name.into(),
                    base: format!("http://{address}"),
                    acp: None,
                    headers: BTreeMap::new(),
                    vars: BTreeMap::new(),
                    capabilities: Vec::new(),
                    interests: Vec::new(),
                    timeout: "2s".into(),
                    open: None,
                    turn: Step {
                        path: "/run".into(),
                        body: Some(json!({"prompt": "{{input}}"})),
                        response: Some(ResponseSpec {
                            format: "json".into(),
                            text: vec!["$.output".into()],
                            ..Default::default()
                        }),
                        ..Default::default()
                    },
                    context: Vec::new(),
                    origin: Origin::File,
                })
                .unwrap();
        }
        let events = Arc::new(MemoryStore::new());
        let manager = Manager::configured(
            registry.clone(),
            Executor::new(),
            events.clone(),
            "roller",
            false,
        );
        let (summary, _) = manager.create(&["a".into()]).await.unwrap();
        for index in 1..=(sharedctx::MAX_ITEMS_PER_ENTRY + 10) {
            manager
                .append_context(
                    &summary.id,
                    "findings",
                    "alice",
                    &format!("finding {index}"),
                )
                .await
                .unwrap();
        }
        let mut rolled = None;
        for _ in 0..1_000 {
            let entry = manager.context(&summary.id).await.unwrap().remove(0);
            if entry.rollup.is_some() {
                rolled = Some(entry);
                break;
            }
            sleep(Duration::from_millis(5)).await;
        }
        let entry = rolled.expect("rollup did not finish");
        assert_eq!(entry.items.len(), sharedctx::MAX_ITEMS_PER_ENTRY + 10);
        assert_eq!(entry.rollup.as_ref().unwrap().covers, 10);
        assert_eq!(calls.load(Ordering::SeqCst), 1);

        let restored = Manager::configured(registry, Executor::new(), events, "roller", false);
        restored.rehydrate().await.unwrap();
        let replayed = restored.context(&summary.id).await.unwrap().remove(0);
        assert_eq!(replayed.rollup.unwrap().text, "summary of 10 findings");
        assert_eq!(calls.load(Ordering::SeqCst), 1);
    }

    #[tokio::test]
    async fn failed_event_appends_do_not_create_unrecorded_room_state() {
        let registry = Registry::new();
        registry
            .put(Spec {
                name: "a".into(),
                base: "http://127.0.0.1:1".into(),
                acp: None,
                headers: BTreeMap::new(),
                vars: BTreeMap::new(),
                capabilities: Vec::new(),
                interests: Vec::new(),
                timeout: "10ms".into(),
                open: None,
                turn: Step::default(),
                context: Vec::new(),
                origin: Origin::File,
            })
            .unwrap();
        let store = Arc::new(SwitchStore::new());
        let manager = Manager::new(registry.clone(), Executor::new(), store.clone());
        let (room, _) = manager.create(&["a".into()]).await.unwrap();

        store.fail.store(true, Ordering::SeqCst);
        assert!(
            manager
                .submit(&room.id, Author::claimed("alice"), "lost", &[])
                .await
                .is_err()
        );
        assert!(manager.view(&room.id).await.unwrap().waiting.is_empty());
        assert!(manager.close(&room.id, "alice").await.is_err());
        assert_eq!(
            manager.view(&room.id).await.unwrap().summary.state,
            State::Idle
        );

        let failed_create = Manager::new(registry, Executor::new(), store);
        assert!(failed_create.create(&["a".into()]).await.is_err());
        assert!(failed_create.list().await.is_empty());
    }
}
