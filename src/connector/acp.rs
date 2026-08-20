use std::{
    collections::BTreeMap,
    path::PathBuf,
    sync::{Arc, Mutex as StdMutex},
    time::Duration,
};

use agent_client_protocol::schema::{
    ProtocolVersion,
    v1::{
        CancelNotification, ContentBlock, InitializeRequest, LoadSessionRequest, NewSessionRequest,
        PromptRequest, RequestPermissionOutcome, RequestPermissionRequest,
        RequestPermissionResponse, SessionId, SessionNotification, SessionUpdate, TextContent,
    },
};
use agent_client_protocol::{AcpAgent, AcpAgentConfig, Agent, ConnectionTo, Error as AcpError};
use anyhow::{Context, Result, anyhow, bail};
use chrono::{DateTime, Utc};
use serde::{Deserialize, Serialize};
use serde_json::Value;
use tokio::sync::{Mutex, mpsc, oneshot};
use tokio_util::sync::CancellationToken;

use super::{Part, RenderContext, Spec};

type ActiveSink = Arc<StdMutex<Option<ActiveTurn>>>;
type ReadySender = Arc<StdMutex<Option<oneshot::Sender<Result<(String, Value)>>>>>;

#[derive(Clone)]
struct ActiveTurn {
    turn: String,
    cancel: CancellationToken,
    parts: mpsc::UnboundedSender<Part>,
}

struct LaneIdentity {
    room: String,
    agent: String,
}

#[derive(Clone, Debug, Deserialize, Serialize)]
pub struct PendingInteraction {
    pub id: String,
    pub session: String,
    pub turn: String,
    pub agent: String,
    pub kind: String,
    pub title: String,
    pub request: Value,
    pub options: Vec<PermissionOption>,
    pub created: DateTime<Utc>,
}

#[derive(Clone, Debug, Deserialize, Serialize)]
pub struct PermissionOption {
    pub id: String,
    pub name: String,
    pub kind: String,
}

#[derive(Clone, Default)]
struct PermissionBroker {
    pending: Arc<Mutex<BTreeMap<String, PendingEntry>>>,
}

struct PendingEntry {
    interaction: PendingInteraction,
    resolution: oneshot::Sender<PermissionResolution>,
}

enum PermissionResolution {
    Selected(String),
    Cancelled,
}

#[derive(Clone, Default)]
pub(crate) struct AcpExecutor {
    /// One slot per lane. The slot is what callers contend on, so two turns
    /// arriving together for the same agent wait for one subprocess instead of
    /// starting two — while different agents still start in parallel.
    lanes: Arc<Mutex<BTreeMap<String, LaneSlot>>>,
    permissions: PermissionBroker,
}

#[derive(Clone)]
struct LaneClient {
    session_id: String,
    capabilities: Value,
    commands: mpsc::Sender<Command>,
}

enum Command {
    Prompt {
        text: String,
        turn: String,
        cancel: CancellationToken,
        parts: mpsc::UnboundedSender<Part>,
        done: oneshot::Sender<Result<()>>,
    },
    Close,
}

impl AcpExecutor {
    pub async fn open(
        &self,
        spec: &Spec,
        context: &RenderContext,
    ) -> Result<(String, BTreeMap<String, String>)> {
        let lane = self.ensure_lane(spec, context).await?;
        let mut captures = BTreeMap::new();
        captures.insert(
            "acp_capabilities".into(),
            serde_json::to_string(&lane.capabilities).unwrap_or_default(),
        );
        Ok((lane.session_id, captures))
    }

    pub async fn turn(
        &self,
        spec: &Spec,
        context: &RenderContext,
        cancel: CancellationToken,
        mut emit: impl FnMut(Part) + Send,
    ) -> Result<()> {
        let key = lane_key(context);
        let lane = self.ensure_lane(spec, context).await?;
        let (parts_tx, mut parts_rx) = mpsc::unbounded_channel();
        let (done_tx, mut done_rx) = oneshot::channel();
        if lane
            .commands
            .send(Command::Prompt {
                text: context.render_string(spec.acp_prompt()),
                turn: context.turn.clone(),
                cancel,
                parts: parts_tx,
                done: done_tx,
            })
            .await
            .is_err()
        {
            self.forget(&key).await;
            bail!("ACP lane disconnected before the prompt was sent");
        }

        loop {
            tokio::select! {
                biased;
                Some(part) = parts_rx.recv() => emit(part),
                result = &mut done_rx => {
                    while let Ok(part) = parts_rx.try_recv() {
                        emit(part);
                    }
                    return result.context("ACP lane stopped before completing the prompt")?;
                }
            }
        }
    }

    pub async fn close(&self, context: &RenderContext) {
        let slot = self.lanes.lock().await.remove(&lane_key(context));
        if let Some(slot) = slot
            && let Some(lane) = slot.lock().await.take()
        {
            let _ = lane.commands.send(Command::Close).await;
        }
    }

    async fn forget(&self, key: &str) {
        if let Some(slot) = self.lanes.lock().await.remove(key) {
            slot.lock().await.take();
        }
    }

    pub async fn pending(&self, session: &str) -> Vec<PendingInteraction> {
        self.permissions.pending(session).await
    }

    pub async fn interaction(
        &self,
        session: &str,
        interaction: &str,
    ) -> Option<PendingInteraction> {
        self.permissions.interaction(session, interaction).await
    }

    pub async fn resolve(
        &self,
        session: &str,
        interaction: &str,
        option_id: Option<&str>,
    ) -> Result<PendingInteraction> {
        self.permissions
            .resolve(session, interaction, option_id)
            .await
    }

    /// The lane for this room and agent, started if it is not running.
    ///
    /// The slot is taken under the map's lock and then held across the spawn.
    /// Releasing it in between is a check-then-act race: two callers both miss,
    /// both start a subprocess, and the second insert orphans the first — which
    /// costs a stray process and, worse, splits the agent's turns across two
    /// ACP sessions, so it silently loses everything it had been told. That
    /// window is small when only turns open lanes and wide open when a room
    /// warms them as it is created.
    async fn ensure_lane(&self, spec: &Spec, context: &RenderContext) -> Result<LaneClient> {
        let key = lane_key(context);
        let slot = {
            let mut lanes = self.lanes.lock().await;
            lanes.entry(key).or_default().clone()
        };
        let mut held = slot.lock().await;
        if let Some(lane) = held.as_ref()
            && !lane.commands.is_closed()
        {
            return Ok(lane.clone());
        }
        let lane = spawn_lane(spec, context, self.permissions.clone()).await?;
        *held = Some(lane.clone());
        Ok(lane)
    }
}

/// A lane that may not have started yet, held while it does.
type LaneSlot = Arc<Mutex<Option<LaneClient>>>;

fn lane_key(context: &RenderContext) -> String {
    format!("{}\0{}", context.conversation, context.agent)
}

async fn spawn_lane(
    spec: &Spec,
    context: &RenderContext,
    permissions: PermissionBroker,
) -> Result<LaneClient> {
    let acp = spec
        .acp
        .as_ref()
        .ok_or_else(|| anyhow!("connector is not configured for ACP"))?;
    let rendered = context.render_string(&acp.cwd);
    let cwd = PathBuf::from(&rendered);
    // Empty and relative are different mistakes. Empty is almost always an
    // environment variable nobody set, and the old message — "must be an
    // absolute path, got """ — named neither the variable nor the fix, which
    // is a poor first thing to meet: it is what a coding agent does instead of
    // answering the first message in a room.
    if rendered.trim().is_empty() {
        bail!(
            "this agent has no workspace: {} rendered empty.\n  \
             It names the directory the agent may write in, so nothing is guessed for it — \
             set it to a checkout you can throw away.",
            acp.cwd
        );
    }
    if !cwd.is_absolute() {
        bail!(
            "this agent's workspace must be an absolute path: {} rendered {rendered:?}",
            acp.cwd
        );
    }

    let command = context.render_string(&acp.command);
    let args = acp
        .args
        .iter()
        .map(|value| context.render_string(value))
        .collect::<Vec<_>>();
    let env = acp
        .env
        .iter()
        .map(|(name, value)| (name.clone(), context.render_string(value)))
        .collect::<BTreeMap<_, _>>();
    let config = AcpAgentConfig::new(command).args(args).envs(env);
    let requested_session = context.handle.clone();
    let room = context.conversation.clone();
    let agent_name = context.agent.clone();
    let (commands, receiver) = mpsc::channel(8);
    let (ready_tx, ready_rx) = oneshot::channel::<Result<(String, Value)>>();
    let ready = Arc::new(StdMutex::new(Some(ready_tx)));
    let ready_for_task = ready.clone();

    tokio::spawn(async move {
        let result = run_lane(
            config,
            cwd,
            requested_session,
            LaneIdentity {
                room,
                agent: agent_name,
            },
            receiver,
            permissions,
            ready_for_task.clone(),
        )
        .await;
        if let Some(sender) = ready_for_task
            .lock()
            .expect("ACP ready lock poisoned")
            .take()
        {
            let message = result
                .as_ref()
                .err()
                .map(ToString::to_string)
                .unwrap_or_else(|| "ACP lane stopped during startup".into());
            let _ = sender.send(Err(anyhow!(message)));
        }
        if let Err(error) = result {
            eprintln!("oryxa: ACP lane stopped: {error:#}");
        }
    });

    let (session_id, capabilities) = ready_rx
        .await
        .context("ACP lane stopped during startup")??;
    Ok(LaneClient {
        session_id,
        capabilities,
        commands,
    })
}

async fn run_lane(
    config: AcpAgentConfig,
    cwd: PathBuf,
    requested_session: String,
    identity: LaneIdentity,
    mut commands: mpsc::Receiver<Command>,
    permissions: PermissionBroker,
    ready: ReadySender,
) -> Result<()> {
    let active: ActiveSink = Arc::new(StdMutex::new(None));
    let notification_sink = active.clone();
    let permission_sink = active.clone();
    let agent = AcpAgent::new(config);

    agent_client_protocol::Client
        .builder()
        .name("oryxa")
        .on_receive_notification(
            async move |notification: SessionNotification, _cx| {
                if let Some(part) = notification_part(notification)
                    && let Some(active) = notification_sink
                        .lock()
                        .expect("ACP notification sink poisoned")
                        .as_ref()
                {
                    let _ = active.parts.send(part);
                }
                Ok(())
            },
            agent_client_protocol::on_receive_notification!(),
        )
        .on_receive_request(
            async move |request: RequestPermissionRequest, responder, connection| {
                let active_turn = permission_sink
                    .lock()
                    .expect("ACP permission sink poisoned")
                    .clone();
                let Some(active_turn) = active_turn else {
                    return responder.respond(RequestPermissionResponse::new(
                        RequestPermissionOutcome::Cancelled,
                    ));
                };
                let pending = pending_interaction(
                    &identity.room,
                    &identity.agent,
                    &active_turn.turn,
                    &request,
                );
                let id = pending.id.clone();
                let request_value = pending.request.clone();
                let receiver = permissions.begin(pending).await;
                let _ = active_turn.parts.send(Part {
                    kind: "permission".into(),
                    text: String::new(),
                    raw: Some(request_value),
                    interaction: Some(id.clone()),
                });
                let permissions = permissions.clone();
                connection.spawn(async move {
                    let resolution = tokio::select! {
                        result = receiver => result.unwrap_or(PermissionResolution::Cancelled),
                        _ = active_turn.cancel.cancelled() => PermissionResolution::Cancelled,
                    };
                    permissions.finish(&id).await;
                    let outcome = match resolution {
                        PermissionResolution::Selected(option_id) => {
                            RequestPermissionOutcome::Selected(
                                agent_client_protocol::schema::v1::SelectedPermissionOutcome::new(option_id)
                            )
                        }
                        PermissionResolution::Cancelled => RequestPermissionOutcome::Cancelled,
                    };
                    responder.respond(RequestPermissionResponse::new(outcome))
                })
            },
            agent_client_protocol::on_receive_request!(),
        )
        .connect_with(agent, |connection: ConnectionTo<Agent>| async move {
            let initialized = connection
                .send_request(InitializeRequest::new(ProtocolVersion::V1))
                .block_task()
                .await
                .map_err(|error| acp_internal(format!("initialize ACP agent: {error}")))?;
            if initialized.protocol_version != ProtocolVersion::V1 {
                return Err(acp_internal(format!(
                    "ACP agent selected unsupported protocol version {:?}",
                    initialized.protocol_version
                )));
            }

            let session_id = if requested_session.is_empty() {
                connection
                    .send_request(NewSessionRequest::new(&cwd))
                    .block_task()
                    .await
                    .map_err(|error| acp_internal(format!("create ACP session: {error}")))?
                    .session_id
            } else {
                if !initialized.agent_capabilities.load_session {
                    return Err(acp_internal(format!(
                        "ACP agent cannot reload persisted session {requested_session:?}"
                    )));
                }
                let id = SessionId::new(requested_session);
                connection
                    .send_request(LoadSessionRequest::new(id.clone(), &cwd))
                    .block_task()
                    .await
                    .map_err(|error| acp_internal(format!("load ACP session: {error}")))?;
                id
            };

            let capabilities = serde_json::to_value(&initialized.agent_capabilities)
                .map_err(|error| acp_internal(format!("serialize ACP capabilities: {error}")))?;
            if let Some(sender) = ready.lock().expect("ACP ready lock poisoned").take() {
                let _ = sender.send(Ok((session_id.to_string(), capabilities)));
            }

            while let Some(command) = commands.recv().await {
                match command {
                    Command::Close => break,
                    Command::Prompt {
                        text,
                        turn,
                        cancel,
                        parts,
                        done,
                    } => {
                        *active.lock().expect("ACP notification sink poisoned") =
                            Some(ActiveTurn {
                                turn,
                                cancel: cancel.clone(),
                                parts,
                            });
                        let request = PromptRequest::new(
                            session_id.clone(),
                            vec![ContentBlock::Text(TextContent::new(text))],
                        );
                        let prompt = connection.send_request(request).block_task();
                        tokio::pin!(prompt);
                        let outcome = tokio::select! {
                            result = &mut prompt => result.context("ACP prompt").map(|_| ()),
                            _ = cancel.cancelled() => {
                                let sent = connection
                                    .send_notification(CancelNotification::new(session_id.clone()))
                                    .context("send ACP cancellation");
                                if sent.is_ok() {
                                    let _ = tokio::time::timeout(Duration::from_secs(5), &mut prompt).await;
                                }
                                Err(anyhow!("cancelled"))
                            }
                        };
                        *active.lock().expect("ACP notification sink poisoned") = None;
                        let _ = done.send(outcome);
                    }
                }
            }
            Ok(())
        })
        .await
        .context("run ACP connection")
}

fn notification_part(notification: SessionNotification) -> Option<Part> {
    let kind = match &notification.update {
        SessionUpdate::AgentMessageChunk(_) => "text",
        SessionUpdate::AgentThoughtChunk(_) => "thought",
        SessionUpdate::UserMessageChunk(_) => return None,
        _ => "activity",
    };
    let text = match &notification.update {
        SessionUpdate::AgentMessageChunk(chunk) | SessionUpdate::AgentThoughtChunk(chunk) => {
            match &chunk.content {
                ContentBlock::Text(content) => content.text.clone(),
                _ => String::new(),
            }
        }
        _ => String::new(),
    };
    Some(Part {
        kind: kind.into(),
        text,
        raw: serde_json::to_value(notification).ok(),
        interaction: None,
    })
}

fn pending_interaction(
    session: &str,
    agent: &str,
    turn: &str,
    request: &RequestPermissionRequest,
) -> PendingInteraction {
    let title = request
        .tool_call
        .fields
        .title
        .clone()
        .unwrap_or_else(|| "Approve agent action".into());
    let options = request
        .options
        .iter()
        .map(|option| PermissionOption {
            id: option.option_id.to_string(),
            name: option.name.clone(),
            kind: serde_json::to_value(option.kind)
                .ok()
                .and_then(|value| value.as_str().map(str::to_owned))
                .unwrap_or_else(|| "unknown".into()),
        })
        .collect();
    PendingInteraction {
        id: uuid::Uuid::new_v4().to_string(),
        session: session.into(),
        turn: turn.into(),
        agent: agent.into(),
        kind: "permission".into(),
        title,
        request: serde_json::to_value(request).unwrap_or(Value::Null),
        options,
        created: Utc::now(),
    }
}

impl PermissionBroker {
    async fn begin(
        &self,
        interaction: PendingInteraction,
    ) -> oneshot::Receiver<PermissionResolution> {
        let (resolution, receiver) = oneshot::channel();
        self.pending.lock().await.insert(
            interaction.id.clone(),
            PendingEntry {
                interaction,
                resolution,
            },
        );
        receiver
    }

    async fn pending(&self, session: &str) -> Vec<PendingInteraction> {
        let mut interactions = self
            .pending
            .lock()
            .await
            .values()
            .filter(|entry| entry.interaction.session == session)
            .map(|entry| entry.interaction.clone())
            .collect::<Vec<_>>();
        interactions.sort_by_key(|interaction| interaction.created);
        interactions
    }

    async fn interaction(&self, session: &str, interaction: &str) -> Option<PendingInteraction> {
        self.pending
            .lock()
            .await
            .get(interaction)
            .filter(|entry| entry.interaction.session == session)
            .map(|entry| entry.interaction.clone())
    }

    async fn resolve(
        &self,
        session: &str,
        interaction: &str,
        option_id: Option<&str>,
    ) -> Result<PendingInteraction> {
        let mut pending = self.pending.lock().await;
        let entry = pending
            .get(interaction)
            .filter(|entry| entry.interaction.session == session)
            .ok_or_else(|| anyhow!("pending interaction not found"))?;
        if let Some(option_id) = option_id
            && !entry
                .interaction
                .options
                .iter()
                .any(|option| option.id == option_id)
        {
            bail!("permission option {option_id:?} is not valid for this interaction");
        }
        let entry = pending
            .remove(interaction)
            .expect("pending interaction disappeared while locked");
        drop(pending);
        let interaction = entry.interaction;
        let resolution = option_id.map_or(PermissionResolution::Cancelled, |option| {
            PermissionResolution::Selected(option.into())
        });
        entry
            .resolution
            .send(resolution)
            .map_err(|_| anyhow!("ACP permission request is no longer active"))?;
        Ok(interaction)
    }

    async fn finish(&self, interaction: &str) {
        self.pending.lock().await.remove(interaction);
    }
}

fn acp_internal(message: String) -> AcpError {
    let mut error = AcpError::internal_error();
    error.message = message;
    error
}
