//! Oryxa as an ACP agent, so an editor is a seat in the room.
//!
//! [`crate::connector::acp`] is the other direction: there Oryxa is the client
//! and a coding agent is the subprocess. Here Oryxa is the agent, launched by
//! an editor — Zed's agent panel, or anything else that speaks ACP — and one
//! ACP session is one Oryxa room.
//!
//! What that buys is the thing a single agent cannot do: the person in the
//! editor is in a room with several coding agents and with other people, and
//! everything said in it arrives in the panel, including what someone typed in
//! a terminal on the other side of the room.
//!
//! ACP has one agent voice and a room has several, so every block is attributed
//! before it is sent. That is a rendering decision, not a protocol one: the
//! editor is talking to a room, and it should be able to see who said what.

use std::{
    collections::{BTreeMap, BTreeSet},
    sync::Arc,
    time::Duration,
};

use agent_client_protocol::schema::v1::{
    AgentCapabilities, CancelNotification, ContentBlock, ContentChunk, InitializeRequest,
    InitializeResponse, LoadSessionRequest, LoadSessionResponse, NewSessionRequest,
    NewSessionResponse, PromptRequest, PromptResponse, SessionNotification, SessionUpdate,
    StopReason,
};
use agent_client_protocol::{Agent, Client as ClientRole, ConnectionTo, Stdio};
use anyhow::Result;
use futures_util::StreamExt;
use serde_json::json;
use tokio::sync::Mutex;
use tokio_util::sync::CancellationToken;

use crate::{
    cli::{Client, rooms},
    events::Event,
};

/// How long to wait for somebody to start answering before reporting that
/// nobody did.
///
/// A room can decide a message was not for any agent — thanks, or a question
/// aimed at a colleague. Without this the editor waits for a turn that is never
/// going to start.
const NOBODY_ANSWERED: Duration = Duration::from_secs(6);

pub struct Config {
    pub client: Client,
    /// The agents a new room is opened with.
    pub agents: Vec<String>,
    /// An existing room to join instead of opening one.
    pub room: Option<String>,
    /// Who the person in the editor speaks as.
    pub author: String,
}

struct State {
    config: Config,
    /// Cancellation for the prompt in flight, per ACP session.
    running: Mutex<BTreeMap<String, CancellationToken>>,
    /// The follower reading each room, and what it has seen.
    following: Mutex<BTreeMap<String, Follow>>,
}

/// A room being watched on behalf of one editor.
///
/// The follower runs whether or not the person in the editor is asking
/// anything, because most of what happens in a room is not addressed to them:
/// a colleague typing in a terminal, an agent finishing a turn somebody else
/// started. A panel that only updated while you were waiting on it would not be
/// a seat in the room, it would be a request box.
struct Follow {
    watcher: tokio::task::JoinHandle<()>,
    turns: tokio::sync::watch::Receiver<Turns>,
}

impl Drop for Follow {
    fn drop(&mut self) {
        self.watcher.abort();
    }
}

#[derive(Clone, Copy, Default, PartialEq)]
struct Turns {
    /// How many turns have ever started here, so a prompt can tell "started and
    /// finished" from "never started".
    started: u64,
    running: usize,
}

pub async fn serve(config: Config) -> Result<()> {
    let state = Arc::new(State {
        config,
        running: Mutex::new(BTreeMap::new()),
        following: Mutex::new(BTreeMap::new()),
    });

    Agent
        .builder()
        .name("oryxa")
        .on_receive_request(
            async move |request: InitializeRequest, responder, _cx| {
                responder.respond(
                    InitializeResponse::new(request.protocol_version)
                        // A room is a fold over its log, so an editor can be
                        // handed one that already has a history in it.
                        .agent_capabilities(AgentCapabilities::new().load_session(true)),
                )
            },
            agent_client_protocol::on_receive_request!(),
        )
        .on_receive_request(
            {
                let state = state.clone();
                async move |_request: NewSessionRequest, responder, connection| {
                    match open_room(&state.config).await {
                        Ok(room) => {
                            let follow = follow(state.clone(), room.clone(), connection);
                            state.following.lock().await.insert(room.clone(), follow);
                            responder.respond(NewSessionResponse::new(room))
                        }
                        // The editor gets the reason rather than a dead panel:
                        // "no server" and "no such agent" need different fixes.
                        Err(error) => responder.respond_with_internal_error(error),
                    }
                }
            },
            agent_client_protocol::on_receive_request!(),
        )
        .on_receive_request(
            {
                let state = state.clone();
                async move |request: LoadSessionRequest, responder, connection| {
                    // Replaying is what makes joining late work at all, and it
                    // is the same history every other client sees.
                    let session = request.session_id.clone();
                    match history(&state.config.client, &session.to_string()).await {
                        Ok(text) => {
                            if !text.is_empty() {
                                notify(&connection, &session.to_string(), &text);
                            }
                            let room = session.to_string();
                            let follow = follow(state.clone(), room.clone(), connection);
                            state.following.lock().await.insert(room, follow);
                            responder.respond(LoadSessionResponse::new())
                        }
                        Err(error) => responder.respond_with_internal_error(error),
                    }
                }
            },
            agent_client_protocol::on_receive_request!(),
        )
        .on_receive_request(
            {
                let state = state.clone();
                async move |request: PromptRequest, responder, connection| {
                    let session = request.session_id.to_string();
                    let text = request
                        .prompt
                        .iter()
                        .filter_map(|block| match block {
                            ContentBlock::Text(text) => Some(text.text.as_str()),
                            _ => None,
                        })
                        .collect::<Vec<_>>()
                        .join("");

                    let cancel = CancellationToken::new();
                    state
                        .running
                        .lock()
                        .await
                        .insert(session.clone(), cancel.clone());

                    let state = state.clone();
                    let stream_to = connection.clone();
                    connection.spawn(async move {
                        let reason = match relay(&state, &session, &text, &stream_to, &cancel).await
                        {
                            Ok(reason) => reason,
                            Err(error) => {
                                let _ = stream_to.send_notification(SessionNotification::new(
                                    request.session_id.clone(),
                                    SessionUpdate::AgentMessageChunk(ContentChunk::new(
                                        format!("\n\noryxa: {error}\n").into(),
                                    )),
                                ));
                                StopReason::EndTurn
                            }
                        };
                        state.running.lock().await.remove(&session);
                        responder.respond(PromptResponse::new(reason))
                    })
                }
            },
            agent_client_protocol::on_receive_request!(),
        )
        .on_receive_notification(
            {
                let state = state.clone();
                async move |notification: CancelNotification, _cx| {
                    let session = notification.session_id.to_string();
                    if let Some(cancel) = state.running.lock().await.get(&session) {
                        cancel.cancel();
                    }
                    // Cancelling in the editor cancels the room's turns, not
                    // just this view of them. Anything less would leave agents
                    // working on something nobody is waiting for.
                    let _: Result<serde_json::Value> = state
                        .config
                        .client
                        .post(
                            &format!(
                                "/v1/sessions/{session}/cancel?author={}",
                                state.config.author
                            ),
                            json!({}),
                        )
                        .await;
                    Ok(())
                }
            },
            agent_client_protocol::on_receive_notification!(),
        )
        .connect_to(Stdio::new())
        .await
        .map_err(|error| anyhow::anyhow!("{error}"))
}

async fn open_room(config: &Config) -> Result<String> {
    if let Some(room) = &config.room {
        // Named on the command line, so it must exist rather than be created
        // silently under a name someone expected to be theirs.
        let _: serde_json::Value = config.client.get(&format!("/v1/sessions/{room}")).await?;
        return Ok(room.clone());
    }
    let opened: serde_json::Value = config
        .client
        .post("/v1/sessions", json!({"agents": config.agents}))
        .await?;
    let id = opened["id"].as_str().unwrap_or_default().to_string();
    rooms::remember(&id, opened["secret"].as_str().unwrap_or_default());
    Ok(id)
}

/// Follows a room for as long as the editor holds the session open.
///
/// Everything the room says is forwarded, attributed, whoever it came from.
/// Reconnects on its own: an editor left open overnight should come back to a
/// live room rather than a pane that quietly stopped updating.
fn follow(state: Arc<State>, session: String, connection: ConnectionTo<ClientRole>) -> Follow {
    let (tx, rx) = tokio::sync::watch::channel(Turns::default());
    let watcher = tokio::spawn(async move {
        let client = state.config.client.clone();
        // From the end, not the beginning: the editor either just created this
        // room or was handed its history by session/load.
        let mut since = crate::cli::commands::current_seq(&client, &session)
            .await
            .unwrap_or(0);
        let mut running: BTreeSet<String> = BTreeSet::new();
        let mut speaking = String::new();
        // Counted here rather than read back off the channel: borrowing the
        // sender to build the next value holds a read guard while the send
        // wants the write lock, which deadlocks the follower on its first turn.
        let mut started = 0u64;
        loop {
            if let Ok(stream) = client.events(&session, since).await {
                futures_util::pin_mut!(stream);
                while let Some(Ok(event)) = stream.next().await {
                    since = event.seq;
                    render(
                        &state,
                        &connection,
                        &session,
                        &event,
                        &mut speaking,
                        &mut running,
                    );
                    started += u64::from(event.kind == "turn.started");
                    let _ = tx.send(Turns {
                        started,
                        running: running.len(),
                    });
                }
            }
            tokio::time::sleep(Duration::from_secs(1)).await;
        }
    });
    Follow { watcher, turns: rx }
}

/// One event, as the panel should see it.
fn render(
    state: &State,
    connection: &ConnectionTo<ClientRole>,
    session: &str,
    event: &Event,
    speaking: &mut String,
    running: &mut BTreeSet<String>,
) {
    let data = event.data.clone().unwrap_or(serde_json::Value::Null);
    match event.kind.as_str() {
        "output.part" if data["kind"] == "text" => {
            let text = data["text"].as_str().unwrap_or_default();
            say(connection, session, speaking, &event.actor, text);
        }
        // Someone else in the room, in the editor's panel. This is the whole
        // point of the seat: a colleague typing in a terminal is a participant,
        // not something you find out about later.
        "input.submitted" if event.actor != state.config.author => {
            let said = data["text"].as_str().unwrap_or_default();
            speaking.clear();
            notify(
                connection,
                session,
                &format!("\n\n**{}:** {said}\n", event.actor),
            );
        }
        "turn.started" => {
            running.insert(event.turn.clone());
        }
        "turn.finished" | "turn.cancelled" => {
            running.remove(&event.turn);
        }
        "turn.empty" => {
            running.remove(&event.turn);
            let reason = data["reason"].as_str().unwrap_or_default();
            speaking.clear();
            notify(
                connection,
                session,
                &format!("\n\n_{} said nothing — {reason}_\n", event.actor),
            );
        }
        "turn.failed" => {
            running.remove(&event.turn);
            let error = data["error"].as_str().unwrap_or_default();
            speaking.clear();
            notify(
                connection,
                session,
                &format!("\n\n_{} failed: {error}_\n", event.actor),
            );
        }
        // A coding agent in the room is asking to act. The editor is not the
        // permission authority here — the room is — so this says who is waiting
        // rather than pretending the panel can answer it.
        "interaction.requested" => {
            speaking.clear();
            notify(
                connection,
                session,
                &format!(
                    "\n\n_{} is waiting for a decision — answer it in the room view, or `oryxa approve {session}`_\n",
                    event.actor
                ),
            );
        }
        _ => {}
    }
}

/// Submits the prompt and returns when the room has gone quiet again.
///
/// The text itself is not echoed back: the follower forwards what the room
/// heard, and the editor already knows what its own user typed.
async fn relay(
    state: &State,
    session: &str,
    text: &str,
    connection: &ConnectionTo<ClientRole>,
    cancel: &CancellationToken,
) -> Result<StopReason> {
    let mut turns = {
        let following = state.following.lock().await;
        let Some(follow) = following.get(session) else {
            anyhow::bail!("this room is not being followed");
        };
        follow.turns.clone()
    };
    let baseline = turns.borrow_and_update().started;

    state
        .config
        .client
        .post::<serde_json::Value>(
            &format!("/v1/sessions/{session}/input"),
            json!({"text": text, "author": state.config.author}),
        )
        .await?;

    let mut answered = false;
    loop {
        let changed = tokio::select! {
            changed = turns.changed() => changed,
            _ = cancel.cancelled() => return Ok(StopReason::Cancelled),
            // Nothing started, which is a decision rather than a failure: the
            // room worked out the message was not for an agent.
            _ = tokio::time::sleep(NOBODY_ANSWERED), if !answered => {
                notify(connection, session, "\n_nobody in the room answered that_\n");
                return Ok(StopReason::EndTurn);
            }
        };
        if changed.is_err() {
            return Ok(StopReason::EndTurn); // the follower is gone
        }
        let state = *turns.borrow_and_update();
        answered |= state.started > baseline;
        if answered && state.running == 0 {
            return Ok(StopReason::EndTurn);
        }
    }
}

/// Sends text, attributing it when the speaker changes.
///
/// Lanes run in parallel, so two agents answering at once would otherwise
/// arrive as one paragraph written by nobody.
fn say(
    connection: &ConnectionTo<ClientRole>,
    session: &str,
    speaking: &mut String,
    actor: &str,
    text: &str,
) {
    if speaking != actor {
        notify(connection, session, &format!("\n\n**{actor}**\n\n"));
        speaking.clear();
        speaking.push_str(actor);
    }
    notify(connection, session, text);
}

fn notify(connection: &ConnectionTo<ClientRole>, session: &str, text: &str) {
    let _ = connection.send_notification(SessionNotification::new(
        agent_client_protocol::schema::v1::SessionId::from(session.to_string()),
        SessionUpdate::AgentMessageChunk(ContentChunk::new(text.to_string().into())),
    ));
}

/// The room so far, rendered for a panel that has just been handed it.
async fn history(client: &Client, session: &str) -> Result<String> {
    #[derive(serde::Deserialize)]
    struct Events {
        #[serde(default)]
        events: Vec<Event>,
    }
    let history: Events = client
        .get(&format!("/v1/sessions/{session}/events"))
        .await?;

    let mut out = String::new();
    let mut speaking = String::new();
    let mut shown: BTreeSet<String> = BTreeSet::new();
    for event in &history.events {
        let data = event.data.clone().unwrap_or(serde_json::Value::Null);
        let text = data["text"].as_str().unwrap_or_default();
        match event.kind.as_str() {
            "output.part" if data["kind"] == "text" => {
                if speaking != event.actor {
                    out.push_str(&format!("\n\n**{}**\n\n", event.actor));
                    speaking = event.actor.clone();
                }
                out.push_str(text);
            }
            "input.submitted" => {
                let group = data["group"].as_str().unwrap_or_default().to_string();
                if !group.is_empty() && !shown.insert(group) {
                    continue;
                }
                speaking.clear();
                out.push_str(&format!("\n\n**{}:** {text}\n", event.actor));
            }
            _ => {}
        }
    }
    Ok(out)
}
