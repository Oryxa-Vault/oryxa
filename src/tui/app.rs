//! What the room view knows and what a key does to it.
//!
//! Every network call is spawned and reports back as a [`Message`], so this
//! half stays synchronous: a key press changes state or starts work, and the
//! answer arrives later as another message. Nothing here awaits, which is why
//! the interface cannot be blocked by a slow server.

use std::collections::{BTreeMap, BTreeSet};

use serde::Deserialize;
use serde_json::{Value, json};
use tokio::{
    sync::mpsc::UnboundedSender,
    task::JoinHandle,
    time::{Duration, sleep},
};

use crate::{
    cli::{Client, rooms},
    connector::PendingInteraction,
    events::Event,
    session::Summary,
    sharedctx::{Entry, Kind},
};

/// Anything the background wants to tell the interface.
pub enum Message {
    Rooms(Vec<Summary>),
    Connectors(Vec<String>),
    Opened {
        id: String,
        agents: Vec<String>,
    },
    Event(Box<Event>),
    Interactions(Vec<PendingInteraction>),
    Context(Vec<Entry>),
    Key {
        author: String,
        key: String,
    },
    /// The stream's own state, so the header can say when it is not live.
    Live(bool),
    Note(String),
    Error(String),
}

/// One line of the transcript, before wrapping.
pub struct Said {
    pub who: String,
    pub voice: Voice,
    pub text: String,
    pub turn: String,
}

#[derive(Clone, Copy, PartialEq)]
pub enum Voice {
    Person,
    Agent,
    /// Everything that is not speech: failures, context writes, permissions.
    Note,
}

pub enum Screen {
    Rooms(Rooms),
    NewRoom(NewRoom),
    Room(Box<Room>),
}

#[derive(Default)]
pub struct Rooms {
    pub list: Vec<Summary>,
    pub selected: usize,
    pub loading: bool,
}

#[derive(Default)]
pub struct NewRoom {
    pub connectors: Vec<String>,
    pub chosen: BTreeSet<usize>,
    pub selected: usize,
}

pub struct Room {
    pub id: String,
    pub agents: Vec<String>,
    pub said: Vec<Said>,
    /// Inputs already shown. One message fans out to a turn per agent, so the
    /// question would otherwise be printed once per agent.
    pub groups: BTreeSet<String>,
    pub running: BTreeMap<String, String>,
    pub pending: Vec<PendingInteraction>,
    /// Which option the approval prompt has selected, and whether it is open.
    pub approval: Option<usize>,
    pub context: Vec<Entry>,
    pub showing_context: bool,
    pub composer: Composer,
    pub scroll: u16,
    /// Stuck to the bottom until the reader scrolls up.
    pub follow: bool,
    pub raw: bool,
    pub live: bool,
    pub stream: Option<JoinHandle<()>>,
}

impl Room {
    fn new(id: String, agents: Vec<String>) -> Self {
        Self {
            id,
            agents,
            said: Vec::new(),
            groups: BTreeSet::new(),
            running: BTreeMap::new(),
            pending: Vec::new(),
            approval: None,
            context: Vec::new(),
            showing_context: false,
            composer: Composer::default(),
            scroll: 0,
            follow: true,
            raw: false,
            live: false,
            stream: None,
        }
    }

    pub fn note(&mut self, text: impl Into<String>) {
        self.said.push(Said {
            who: String::new(),
            voice: Voice::Note,
            text: text.into(),
            turn: String::new(),
        });
    }

    pub fn busy(&self) -> bool {
        !self.running.is_empty()
    }
}

/// The message being typed.
///
/// Byte offsets throughout, so a cursor never lands inside a character.
#[derive(Default)]
pub struct Composer {
    pub text: String,
    pub cursor: usize,
}

impl Composer {
    pub fn insert(&mut self, ch: char) {
        self.text.insert(self.cursor, ch);
        self.cursor += ch.len_utf8();
    }

    pub fn backspace(&mut self) {
        if let Some(previous) = self.text[..self.cursor].chars().next_back() {
            self.cursor -= previous.len_utf8();
            self.text.remove(self.cursor);
        }
    }

    pub fn delete(&mut self) {
        if self.cursor < self.text.len() {
            self.text.remove(self.cursor);
        }
    }

    pub fn left(&mut self) {
        if let Some(previous) = self.text[..self.cursor].chars().next_back() {
            self.cursor -= previous.len_utf8();
        }
    }

    pub fn right(&mut self) {
        if let Some(next) = self.text[self.cursor..].chars().next() {
            self.cursor += next.len_utf8();
        }
    }

    pub fn kill_word(&mut self) {
        let head = self.text[..self.cursor].trim_end_matches(' ');
        let start = head.rfind(' ').map(|index| index + 1).unwrap_or(0);
        self.text.replace_range(start..self.cursor, "");
        self.cursor = start;
    }

    pub fn take(&mut self) -> String {
        self.cursor = 0;
        std::mem::take(&mut self.text)
    }
}

pub struct App {
    pub client: Client,
    pub tx: UnboundedSender<Message>,
    pub screen: Screen,
    pub author: String,
    pub server: String,
    /// Where the runtime this view started reads its connectors, when it
    /// started one at all.
    pub local_connectors: Option<std::path::PathBuf>,
    /// Permissions are being granted without asking, and the header says so —
    /// a mode this consequential must never be something you have to remember
    /// you turned on.
    pub express: bool,
    /// The directory rooms opened here will work in, when this view is the one
    /// deciding. Empty when attached to somebody else's server, where the
    /// directory is theirs and a local path would mean nothing.
    pub workspace: String,
    pub error: Option<String>,
    pub note: Option<String>,
    pub help: bool,
    /// The first screen, up until any key is pressed.
    pub welcome: bool,
    pub quit: bool,
}

impl App {
    pub fn new(
        client: Client,
        tx: UnboundedSender<Message>,
        local_connectors: Option<std::path::PathBuf>,
    ) -> Self {
        let server = client.base().to_string();
        let app = Self {
            client,
            tx,
            screen: Screen::Rooms(Rooms {
                loading: true,
                ..Rooms::default()
            }),
            author: std::env::var("USER").unwrap_or_else(|_| "you".into()),
            server,
            local_connectors,
            express: false,
            workspace: String::new(),
            error: None,
            note: None,
            help: false,
            welcome: false,
            quit: false,
        };
        app.load_rooms();
        app
    }

    // ---- background work ----

    fn load_rooms(&self) {
        #[derive(Deserialize)]
        struct Listed {
            #[serde(default)]
            sessions: Vec<Summary>,
        }
        let (client, tx) = (self.client.clone(), self.tx.clone());
        tokio::spawn(async move {
            match client.get::<Listed>("/v1/sessions").await {
                Ok(listed) => {
                    let _ = tx.send(Message::Rooms(listed.sessions));
                }
                Err(error) => {
                    let _ = tx.send(Message::Error(error.to_string()));
                }
            }
        });
    }

    fn load_connectors(&self) {
        #[derive(Deserialize)]
        struct Agent {
            name: String,
        }
        #[derive(Deserialize)]
        struct Listed {
            #[serde(default)]
            agents: Vec<Agent>,
        }
        let (client, tx) = (self.client.clone(), self.tx.clone());
        tokio::spawn(async move {
            match client.get::<Listed>("/v1/agents").await {
                Ok(listed) => {
                    let names = listed.agents.into_iter().map(|agent| agent.name).collect();
                    let _ = tx.send(Message::Connectors(names));
                }
                Err(error) => {
                    let _ = tx.send(Message::Error(error.to_string()));
                }
            }
        });
    }

    pub fn open(&self, agents: Vec<String>) {
        let (client, tx) = (self.client.clone(), self.tx.clone());
        let workspace = self.workspace.clone();
        tokio::spawn(async move {
            match client
                .post::<Value>(
                    "/v1/sessions",
                    json!({"agents": agents, "workspace": workspace}),
                )
                .await
            {
                Ok(opened) => {
                    let id = opened["id"].as_str().unwrap_or_default().to_string();
                    // Written down before the room is entered. The server cannot
                    // reissue it, and this view is not where a secret should only
                    // ever have lived.
                    rooms::remember(&id, opened["secret"].as_str().unwrap_or_default());
                    let _ = tx.send(Message::Opened { id, agents });
                }
                Err(error) => {
                    let _ = tx.send(Message::Error(error.to_string()));
                }
            }
        });
    }

    /// Follows a room for as long as the view is in it.
    ///
    /// Reconnects on its own, resuming from the last sequence it saw, because a
    /// laptop that slept should come back to a room rather than a dead pane.
    fn follow_room(&self, id: String) -> JoinHandle<()> {
        let (client, tx) = (self.client.clone(), self.tx.clone());
        tokio::spawn(async move {
            let mut since = 0;
            loop {
                match client.events(&id, since).await {
                    Ok(stream) => {
                        let _ = tx.send(Message::Live(true));
                        futures_util::pin_mut!(stream);
                        while let Some(Ok(event)) = futures_util::StreamExt::next(&mut stream).await
                        {
                            since = event.seq;
                            if tx.send(Message::Event(Box::new(event))).is_err() {
                                return;
                            }
                        }
                    }
                    Err(error) => {
                        let _ = tx.send(Message::Error(error.to_string()));
                    }
                }
                if tx.send(Message::Live(false)).is_err() {
                    return;
                }
                sleep(Duration::from_secs(1)).await;
            }
        })
    }

    fn load_interactions(&self, id: String) {
        #[derive(Deserialize)]
        struct Listed {
            #[serde(default)]
            interactions: Vec<PendingInteraction>,
        }
        let (client, tx) = (self.client.clone(), self.tx.clone());
        tokio::spawn(async move {
            if let Ok(listed) = client
                .get::<Listed>(&format!("/v1/sessions/{id}/interactions"))
                .await
            {
                let _ = tx.send(Message::Interactions(listed.interactions));
            }
        });
    }

    fn load_context(&self, id: String) {
        #[derive(Deserialize)]
        struct Listed {
            #[serde(default)]
            context: Vec<Entry>,
        }
        let (client, tx) = (self.client.clone(), self.tx.clone());
        tokio::spawn(async move {
            match client
                .get::<Listed>(&format!("/v1/sessions/{id}/context"))
                .await
            {
                Ok(listed) => {
                    let _ = tx.send(Message::Context(listed.context));
                }
                Err(error) => {
                    let _ = tx.send(Message::Error(error.to_string()));
                }
            }
        });
    }

    fn post(&self, path: String, body: Value, note: Option<String>) {
        let (client, tx) = (self.client.clone(), self.tx.clone());
        tokio::spawn(async move {
            match client.post::<Value>(&path, body).await {
                Ok(_) => {
                    if let Some(note) = note {
                        let _ = tx.send(Message::Note(note));
                    }
                }
                Err(error) => {
                    let _ = tx.send(Message::Error(error.to_string()));
                }
            }
        });
    }

    // ---- messages ----

    pub fn on_message(&mut self, message: Message) {
        match message {
            Message::Rooms(list) => {
                if let Screen::Rooms(screen) = &mut self.screen {
                    screen.selected = screen.selected.min(list.len().saturating_sub(1));
                    screen.list = list;
                    screen.loading = false;
                }
            }
            Message::Connectors(names) => {
                if let Screen::NewRoom(screen) = &mut self.screen {
                    screen.connectors = names;
                }
            }
            Message::Opened { id, agents } => self.enter_room(id, agents),
            Message::Event(event) => self.on_event(*event),
            Message::Interactions(pending) => {
                if let Screen::Room(room) = &mut self.screen {
                    // A request that arrives while nothing is open opens the
                    // prompt: the agent is blocked until someone answers, so
                    // burying it would be the wrong default.
                    if room.approval.is_none() && !pending.is_empty() {
                        room.approval = Some(0);
                    }
                    if pending.is_empty() {
                        room.approval = None;
                    }
                    room.pending = pending;
                }
            }
            Message::Context(entries) => {
                if let Screen::Room(room) = &mut self.screen {
                    room.context = entries;
                    room.showing_context = true;
                }
            }
            Message::Key { author, key } => {
                self.note = Some(format!("key for {author}: {key} — shown once"));
            }
            Message::Live(live) => {
                if let Screen::Room(room) = &mut self.screen {
                    room.live = live;
                }
            }
            Message::Note(note) => self.note = Some(note),
            Message::Error(error) => self.error = Some(error),
        }
    }

    fn on_event(&mut self, event: Event) {
        let Screen::Room(room) = &mut self.screen else {
            return;
        };
        if event.session != room.id {
            return;
        }
        let data = event.data.clone().unwrap_or(Value::Null);
        let text = data["text"].as_str().unwrap_or_default().to_string();

        match event.kind.as_str() {
            "output.part" if data["kind"] == "text" => {
                // Deltas belong to the block the same agent is already speaking
                // in. Starting a new one per delta would print the agent's name
                // in front of every few characters.
                match room.said.last_mut() {
                    Some(last)
                        if last.voice == Voice::Agent
                            && last.who == event.actor
                            && last.turn == event.turn =>
                    {
                        last.text.push_str(&text);
                    }
                    _ => room.said.push(Said {
                        who: event.actor.clone(),
                        voice: Voice::Agent,
                        text,
                        turn: event.turn.clone(),
                    }),
                }
            }
            "output.part" if room.raw => {
                room.note(format!("{} · {}", event.actor, data));
            }
            "output.part" => {}
            "input.submitted" => {
                let group = data["group"].as_str().unwrap_or_default().to_string();
                if !group.is_empty() && !room.groups.insert(group) {
                    return;
                }
                room.said.push(Said {
                    who: event.actor.clone(),
                    voice: Voice::Person,
                    text,
                    turn: event.turn.clone(),
                });
            }
            "turn.started" => {
                room.running.insert(event.turn.clone(), event.actor.clone());
            }
            "turn.finished" => {
                room.running.remove(&event.turn);
            }
            "turn.empty" => {
                room.running.remove(&event.turn);
                let reason = data["reason"].as_str().unwrap_or_default();
                room.note(format!(
                    "{} finished without saying anything — {reason}",
                    event.actor
                ));
            }
            "turn.failed" => {
                room.running.remove(&event.turn);
                let error = data["error"].as_str().unwrap_or_default();
                room.note(format!("{} failed: {error}", event.actor));
            }
            "turn.cancelled" => {
                room.running.remove(&event.turn);
                room.note(format!("{} cancelled", event.actor));
            }
            "interaction.requested" | "interaction.resolved" | "interaction.cancel_requested" => {
                let id = room.id.clone();
                self.load_interactions(id);
            }
            // The agent could not be started at all, which is not a turn
            // failing — there was no turn.
            "lane.unavailable" => {
                let error = data["error"].as_str().unwrap_or_default();
                room.note(format!("{} could not start: {error}", event.actor));
            }
            "context.appended" => {
                let key = data["key"].as_str().unwrap_or_default();
                room.note(format!("{} appended to {key}", event.actor));
            }
            "context.set" => {
                let key = data["key"].as_str().unwrap_or_default();
                let value = data["value"].as_str().unwrap_or_default();
                room.note(format!("{} set {key} = {value}", event.actor));
            }
            _ if room.raw => room.note(format!("seq={} {} {}", event.seq, event.kind, event.actor)),
            _ => {}
        }
    }

    // ---- navigation ----

    pub fn enter_room(&mut self, id: String, agents: Vec<String>) {
        let mut room = Room::new(id.clone(), agents);
        room.stream = Some(self.follow_room(id.clone()));
        self.screen = Screen::Room(Box::new(room));
        self.load_interactions(id);
    }

    pub fn leave_room(&mut self) {
        if let Screen::Room(room) = &mut self.screen
            && let Some(stream) = room.stream.take()
        {
            stream.abort();
        }
        self.screen = Screen::Rooms(Rooms {
            loading: true,
            ..Rooms::default()
        });
        self.load_rooms();
    }

    pub fn refresh(&mut self) {
        match &self.screen {
            Screen::Rooms(_) => self.load_rooms(),
            Screen::NewRoom(_) => self.load_connectors(),
            Screen::Room(room) => {
                let id = room.id.clone();
                self.load_interactions(id);
            }
        }
    }

    pub fn new_room(&mut self) {
        self.screen = Screen::NewRoom(NewRoom::default());
        self.load_connectors();
    }

    // ---- actions ----

    /// Sends what has been typed.
    ///
    /// A leading `@name` directs the message at one agent rather than letting
    /// the room decide who it was for. Everything else is a plain message.
    pub fn send(&mut self) {
        let Screen::Room(room) = &mut self.screen else {
            return;
        };
        let text = room.composer.take();
        let text = text.trim().to_string();
        if text.is_empty() {
            return;
        }
        if let Some(command) = text.strip_prefix('/') {
            let command = command.to_string();
            return self.command(&command);
        }
        let (to, text) = directed(&text, &room.agents);
        let id = room.id.clone();
        self.post(
            format!("/v1/sessions/{id}/input"),
            json!({"text": text, "author": self.author, "to": to}),
            None,
        );
    }

    pub fn command(&mut self, command: &str) {
        let mut parts = command.split_whitespace();
        let name = parts.next().unwrap_or_default().to_string();
        let rest = parts.collect::<Vec<_>>().join(" ");
        let id = match &self.screen {
            Screen::Room(room) => room.id.clone(),
            _ => String::new(),
        };
        match name.as_str() {
            "help" | "?" => self.help = true,
            "quit" | "q" | "exit" => self.quit = true,
            "rooms" => self.leave_room(),
            "new" => self.new_room(),
            // `/cancel` stops everything; `/cancel codex` stops one lane and
            // leaves the others working, which is the whole point of lanes.
            "cancel" => {
                let (path, note) = match rest.trim() {
                    "" => (
                        format!("/v1/sessions/{id}/cancel?author={}", self.author),
                        "stopped every running turn".to_string(),
                    ),
                    agent => (
                        format!(
                            "/v1/sessions/{id}/cancel?author={}&agent={agent}",
                            self.author
                        ),
                        format!("stopped {agent}"),
                    ),
                };
                self.post(path, json!({}), Some(note))
            }
            "close" => self.post(
                format!("/v1/sessions/{id}/close?author={}", self.author),
                json!({}),
                Some("room closed".into()),
            ),
            "context" => self.load_context(id),
            "raw" => {
                if let Screen::Room(room) = &mut self.screen {
                    room.raw = !room.raw;
                    let raw = room.raw;
                    room.note(format!("raw events {}", if raw { "on" } else { "off" }));
                }
            }
            "as" if !rest.is_empty() => {
                self.author = rest;
                self.note = Some(format!("speaking as {}", self.author));
            }
            "key" if !rest.is_empty() => {
                let (client, tx) = (self.client.clone(), self.tx.clone());
                let author = rest.clone();
                tokio::spawn(async move {
                    match client
                        .post::<Value>(
                            &format!("/v1/sessions/{id}/keys"),
                            json!({"author": author}),
                        )
                        .await
                    {
                        Ok(issued) => {
                            let _ = tx.send(Message::Key {
                                author: issued["author"].as_str().unwrap_or_default().into(),
                                key: issued["key"].as_str().unwrap_or_default().into(),
                            });
                        }
                        Err(error) => {
                            let _ = tx.send(Message::Error(error.to_string()));
                        }
                    }
                });
            }
            "approve" => {
                if let Screen::Room(room) = &mut self.screen
                    && !room.pending.is_empty()
                {
                    room.approval = Some(0);
                }
            }
            other => self.error = Some(format!("no such command: /{other} — try /help")),
        }
    }

    /// Answers the permission request the prompt has open.
    pub fn resolve(&mut self, deny: bool) {
        let Screen::Room(room) = &mut self.screen else {
            return;
        };
        let Some(selected) = room.approval else {
            return;
        };
        let Some(request) = room.pending.first() else {
            return;
        };
        let body = if deny {
            json!({"cancel": true, "author": self.author})
        } else {
            match request.options.get(selected) {
                Some(option) => json!({"option_id": option.id, "author": self.author}),
                None => return,
            }
        };
        let path = format!(
            "/v1/sessions/{}/interactions/{}/resolve",
            room.id, request.id
        );
        // Removed here rather than on the event, so the prompt closes the moment
        // the key is pressed. The stream confirms it a beat later.
        room.pending.remove(0);
        // Straight on to the next one if an agent is still waiting: they are
        // each blocking a lane, and closing the prompt to reopen it would look
        // like a flicker rather than a queue.
        room.approval = (!room.pending.is_empty()).then_some(0);
        self.post(path, body, None);
    }

    pub fn cancel_turn(&mut self) {
        let Screen::Room(room) = &self.screen else {
            return;
        };
        let path = format!("/v1/sessions/{}/cancel?author={}", room.id, self.author);
        self.post(path, json!({}), Some("cancelling".into()));
    }
}

/// Splits a leading `@agent` off a message.
///
/// Only names that are actually in the room count, so an email address or a
/// `@here` aimed at people is left in the text where it was written. Short
/// names work here for the same reason they work on the server: people write
/// `@codex`, not `@codex-local`.
fn directed(text: &str, agents: &[String]) -> (Vec<String>, String) {
    let mut to = Vec::new();
    let mut rest = text;
    while let Some(after) = rest.strip_prefix('@') {
        let (written, tail) = match after.find(char::is_whitespace) {
            Some(index) => after.split_at(index),
            None => (after, ""),
        };
        let written = written.to_ascii_lowercase();
        let Some(agent) = agents
            .iter()
            .find(|agent| crate::session::names_for(agent).contains(&written))
        else {
            break;
        };
        to.push(agent.clone());
        rest = tail.trim_start();
    }
    (to, rest.to_string())
}

/// One line per shared-context entry, for the panel `/context` opens.
pub fn context_lines(entries: &[Entry]) -> Vec<String> {
    let mut lines = Vec::new();
    for entry in entries {
        let pin = if entry.pinned { "📌 " } else { "" };
        if entry.kind == Kind::Append {
            lines.push(format!(
                "{pin}{} — {} entries",
                entry.key,
                entry.items.len()
            ));
            for item in &entry.items {
                lines.push(format!("    {:<10} {}", item.by, item.text));
            }
        } else {
            lines.push(format!(
                "{pin}{} = {}  (v{} by {})",
                entry.key, entry.value, entry.version, entry.by
            ));
        }
    }
    if lines.is_empty() {
        lines.push("no shared context yet".into());
    }
    lines
}

#[cfg(test)]
mod tests {
    use serde_json::json;
    use tokio::sync::mpsc;

    use super::{App, Composer, Message, Screen, Voice, directed};
    use crate::{cli::Client, events::Event};

    /// An app pointed at a port nothing answers on. Every call it makes fails
    /// in the background, which is exactly the isolation these tests want.
    ///
    /// The receiver comes back with it so the caller can keep the channel open:
    /// dropping it would make the spawned tasks give up for a reason unrelated
    /// to what is being tested.
    fn app() -> (App, mpsc::UnboundedReceiver<Message>) {
        let (tx, rx) = mpsc::unbounded_channel();
        (
            App::new(Client::new("http://127.0.0.1:1", "", None), tx, None),
            rx,
        )
    }

    fn event(kind: &str, actor: &str, turn: &str, data: serde_json::Value) -> Event {
        Event {
            seq: 1,
            session: "s_1".into(),
            ts: chrono::Utc::now(),
            kind: kind.into(),
            actor: actor.into(),
            turn: turn.into(),
            data: Some(data),
        }
    }

    #[tokio::test]
    async fn deltas_from_one_agent_become_one_block() {
        let (mut app, _rx) = app();
        app.enter_room("s_1".into(), vec!["codex".into(), "claude-code".into()]);
        for text in ["event ", "sourcing ", "is fine"] {
            app.on_message(Message::Event(Box::new(event(
                "output.part",
                "codex",
                "t1",
                json!({"kind": "text", "text": text}),
            ))));
        }
        // A second agent speaking must not join the first one's paragraph.
        app.on_message(Message::Event(Box::new(event(
            "output.part",
            "claude-code",
            "t2",
            json!({"kind": "text", "text": "disagree"}),
        ))));

        let Screen::Room(room) = &app.screen else {
            panic!("not in a room")
        };
        assert_eq!(room.said.len(), 2);
        assert_eq!(room.said[0].who, "codex");
        assert_eq!(room.said[0].text, "event sourcing is fine");
        assert_eq!(room.said[1].who, "claude-code");
        assert!(room.said.iter().all(|said| said.voice == Voice::Agent));
    }

    #[tokio::test]
    async fn one_input_is_shown_once_however_many_agents_it_woke() {
        let (mut app, _rx) = app();
        app.enter_room("s_1".into(), vec!["codex".into(), "claude-code".into()]);
        for turn in ["t1", "t2"] {
            app.on_message(Message::Event(Box::new(event(
                "input.submitted",
                "shubham",
                turn,
                json!({"text": "ship it?", "group": "g1"}),
            ))));
        }
        let Screen::Room(room) = &app.screen else {
            panic!("not in a room")
        };
        assert_eq!(room.said.len(), 1);
    }

    #[tokio::test]
    async fn a_permission_request_opens_the_prompt_and_answering_closes_it() {
        let (mut app, _rx) = app();
        app.enter_room("s_1".into(), vec!["codex".into()]);
        let request = serde_json::from_value(json!({
            "id": "i_1",
            "session": "s_1",
            "turn": "t1",
            "agent": "codex",
            "kind": "permission",
            "title": "write src/main.rs",
            "request": {},
            "options": [
                {"id": "allow-once", "name": "Allow once", "kind": "allow_once"},
                {"id": "reject", "name": "Reject", "kind": "reject_once"}
            ],
            "created": chrono::Utc::now(),
        }))
        .expect("a permission request decodes");
        app.on_message(Message::Interactions(vec![request]));

        let Screen::Room(room) = &app.screen else {
            panic!("not in a room")
        };
        // A blocked agent is surfaced without being asked for: the prompt opens
        // itself, on the first option.
        assert_eq!(room.approval, Some(0));

        app.resolve(false);
        let Screen::Room(room) = &app.screen else {
            panic!("not in a room")
        };
        assert!(room.approval.is_none());
        assert!(room.pending.is_empty());
    }

    #[test]
    fn directs_only_at_agents_in_the_room() {
        let agents = vec!["codex".to_string(), "claude-code".to_string()];
        assert_eq!(
            directed("@codex look at this", &agents),
            (vec!["codex".to_string()], "look at this".to_string())
        );
        // Written short, sent to the agent's real name.
        let long = vec!["codex-local".to_string()];
        assert_eq!(
            directed("@codex look at this", &long),
            (vec!["codex-local".to_string()], "look at this".to_string())
        );
        assert_eq!(
            directed("@codex @claude-code both of you", &agents),
            (
                vec!["codex".to_string(), "claude-code".to_string()],
                "both of you".to_string()
            )
        );
        // Not an agent, so it stays in the message.
        assert_eq!(
            directed("@arsh can you look?", &agents),
            (Vec::new(), "@arsh can you look?".to_string())
        );
    }

    #[test]
    fn the_composer_never_splits_a_character() {
        let mut composer = Composer::default();
        for ch in "héllo".chars() {
            composer.insert(ch);
        }
        composer.left();
        composer.backspace();
        assert_eq!(composer.text, "hélo");
    }

    #[test]
    fn kill_word_removes_the_word_behind_the_cursor() {
        let mut composer = Composer::default();
        for ch in "review the diff".chars() {
            composer.insert(ch);
        }
        composer.kill_word();
        assert_eq!(composer.text, "review the ");
    }
}
