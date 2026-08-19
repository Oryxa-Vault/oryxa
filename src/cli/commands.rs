//! The commands that talk to a running server.
//!
//! Each one is a thin shell over the same `/v1` API the viewer and any other
//! client uses. Nothing here reaches into the runtime directly, so a command
//! works the same against a local room and a server three time zones away.

use std::collections::BTreeSet;

use anyhow::{Result, bail};
use clap::Args;
use futures_util::{StreamExt, pin_mut};
use serde::Deserialize;
use serde_json::{Value, json};

use crate::{
    cli::{Client, printer::Printer, rooms},
    connector::PendingInteraction,
    events::Event,
    session::Summary,
    sharedctx::{Entry, Kind},
};

/// Flags every room command takes.
#[derive(Args, Clone)]
pub struct ServerOptions {
    /// Oryxa URL (or ORYXA_URL; default http://localhost:8080).
    #[arg(long, default_value = "")]
    pub server: String,
    /// API token (or ORYXA_TOKEN).
    #[arg(long, default_value = "")]
    pub token: String,
    /// Room secret, shown once when the room was opened (or
    /// ORYXA_SESSION_SECRET). Rooms opened on this machine are remembered, so
    /// this is for joining someone else's.
    #[arg(long)]
    pub secret: Option<String>,
    /// Print raw JSON instead of a table.
    #[arg(long)]
    pub json: bool,
}

impl ServerOptions {
    pub fn client(&self) -> Client {
        Client::new(&self.server, &self.token, self.secret.clone())
    }
}

fn print_json(value: &impl serde::Serialize) -> Result<()> {
    println!("{}", serde_json::to_string_pretty(value)?);
    Ok(())
}

// ---- sessions ----

#[derive(Deserialize)]
struct Sessions {
    #[serde(default)]
    sessions: Vec<Summary>,
}

pub async fn sessions(options: ServerOptions) -> Result<()> {
    let listed: Sessions = options.client().get("/v1/sessions").await?;
    if options.json {
        return print_json(&listed.sessions);
    }
    if listed.sessions.is_empty() {
        println!("\n  no rooms yet — oryxa open <agent>\n");
        return Ok(());
    }
    println!();
    println!("  {:<22} {:<9} {:<8} AGENTS", "ROOM", "STATE", "AGE");
    for session in &listed.sessions {
        println!(
            "  {:<22} {:<9} {:<8} {}",
            session.id,
            format!("{:?}", session.state).to_lowercase(),
            short_age(chrono::Utc::now() - session.created),
            session.agents.join(", ")
        );
    }
    println!();
    Ok(())
}

// ---- open ----

pub async fn open(agents: Vec<String>, workspace: String, options: ServerOptions) -> Result<()> {
    if agents.is_empty() {
        bail!("usage: oryxa open <agent> [agent...]");
    }
    let opened: Value = options
        .client()
        .post(
            "/v1/sessions",
            json!({"agents": agents, "workspace": workspace}),
        )
        .await?;

    // Written down before anything is printed. The server cannot reissue it, so
    // losing it here would mean losing the room to a terminal that scrolled.
    let id = opened["id"].as_str().unwrap_or_default();
    let secret = opened["secret"].as_str().unwrap_or_default();
    rooms::remember(id, secret);

    if options.json {
        return print_json(&opened);
    }
    println!("\n  {id}\n  agents: {}\n", agents.join(", "));
    println!("  oryxa               open it in the room view");
    println!("  oryxa send {id} \"your question\"");
    println!("  oryxa tail {id}\n");
    // Shown once, because this is the only time it exists. Everything on this
    // machine now finds it without being told; anyone else needs this line.
    println!("  secret  {secret}");
    println!("  someone else joins with: oryxa tail {id} --secret {secret}\n");
    Ok(())
}

// ---- send ----

pub async fn send(
    session: String,
    text: String,
    author: String,
    to: Vec<String>,
    follow_answers: bool,
    options: ServerOptions,
) -> Result<()> {
    let client = options.client();
    // Read where the room is before speaking, so the answers that follow start
    // from this message rather than replaying the history behind it.
    let from = if follow_answers {
        current_seq(&client, &session).await? - 1
    } else {
        0
    };
    let queued: Value = client
        .post(
            &format!("/v1/sessions/{session}/input"),
            json!({"text": text, "author": author, "to": to}),
        )
        .await?;
    if options.json {
        return print_json(&queued);
    }
    if !follow_answers {
        println!(
            "\n  queued as {}\n  oryxa tail {session}\n",
            queued["id"].as_str().unwrap_or_default()
        );
        return Ok(());
    }
    println!();
    follow(&client, &session, from, false, true).await?;
    println!();
    Ok(())
}

// ---- tail ----

pub async fn tail(session: String, since: i64, raw: bool, options: ServerOptions) -> Result<()> {
    let client = options.client();
    let from = if since < 0 {
        // Default to the current end, so tail behaves like tail rather than
        // replaying an entire history nobody asked for.
        current_seq(&client, &session).await?
    } else {
        since
    };
    println!("\n  following {session} from seq {from} — ctrl-c to stop\n");
    follow(&client, &session, from, raw, false).await?;
    Ok(())
}

// ---- replay ----

#[derive(Deserialize)]
struct Events {
    #[serde(default)]
    events: Vec<Event>,
}

pub async fn replay(session: String, raw: bool, options: ServerOptions) -> Result<()> {
    let history: Events = options
        .client()
        .get(&format!("/v1/sessions/{session}/events"))
        .await?;
    if options.json {
        return print_json(&history.events);
    }
    println!();
    let mut printer = Printer::new(raw);
    for event in &history.events {
        printer.print(event);
    }
    printer.end_line();
    println!();
    Ok(())
}

// ---- keys ----

/// Issues a room key bound to a name.
///
/// The point of it: without one, an author is whatever a request says it is, so
/// `--as arsh` is a costume anyone can wear. With one, the name travels with the
/// credential and the server stops reading it from the message.
pub async fn key(session: String, author: String, options: ServerOptions) -> Result<()> {
    let issued: Value = options
        .client()
        .post(
            &format!("/v1/sessions/{session}/keys"),
            json!({"author": author}),
        )
        .await?;
    if options.json {
        return print_json(&issued);
    }
    println!(
        "\n  a key for {}\n",
        issued["author"].as_str().unwrap_or_default()
    );
    println!("  {}\n", issued["key"].as_str().unwrap_or_default());
    println!("  they use it with:");
    println!(
        "    oryxa tail {session} --secret {}\n",
        issued["key"].as_str().unwrap_or_default()
    );
    // Said plainly, because the failure is silent: a key handed to two people
    // makes two people indistinguishable in the log, which is the one thing it
    // was issued to prevent.
    println!("  shown once. One key per person — sharing one puts two people");
    println!("  behind one name, which is what this exists to stop.\n");
    Ok(())
}

// ---- context ----

#[derive(Deserialize)]
struct Context {
    #[serde(default)]
    context: Vec<Entry>,
}

pub async fn context(
    session: String,
    append: Option<String>,
    set: Option<String>,
    value: String,
    author: String,
    options: ServerOptions,
) -> Result<()> {
    let client = options.client();
    if let Some(key) = append {
        let written: Value = client
            .post(
                &format!("/v1/sessions/{session}/context/{key}"),
                json!({"append": value, "author": author}),
            )
            .await?;
        if options.json {
            return print_json(&written);
        }
        println!("\n  appended to {key}\n");
        return Ok(());
    }
    if let Some(key) = set {
        let written: Value = client
            .post(
                &format!("/v1/sessions/{session}/context/{key}"),
                json!({"value": value, "author": author}),
            )
            .await?;
        if options.json {
            return print_json(&written);
        }
        println!(
            "\n  {key} = {} (version {})\n",
            written["value"].as_str().unwrap_or_default(),
            written["version"]
        );
        return Ok(());
    }

    let shared: Context = client
        .get(&format!("/v1/sessions/{session}/context"))
        .await?;
    if options.json {
        return print_json(&shared.context);
    }
    if shared.context.is_empty() {
        println!("\n  no shared context yet\n");
        return Ok(());
    }
    println!();
    for entry in &shared.context {
        let pin = if entry.pinned { "📌" } else { "  " };
        if entry.kind == Kind::Append {
            println!("  {pin} {:<16} {} entries", entry.key, entry.items.len());
            for item in &entry.items {
                println!("       {:<8} {}", item.by, item.text);
            }
            continue;
        }
        println!(
            "  {pin} {:<16} v{:<4} {:<8} {}",
            entry.key, entry.version, entry.by, entry.value
        );
    }
    println!();
    Ok(())
}

// ---- approvals ----

#[derive(Deserialize)]
struct Interactions {
    #[serde(default)]
    interactions: Vec<PendingInteraction>,
}

/// Answers a coding agent that is waiting for permission.
///
/// A local ACP agent blocks its lane until someone decides, so this is the
/// difference between a stalled room and a working one. The interactive room
/// view surfaces the same requests; this exists for the shell and for scripts.
pub async fn approve(
    session: String,
    interaction: Option<String>,
    option_id: Option<String>,
    deny: bool,
    author: String,
    options: ServerOptions,
) -> Result<()> {
    let client = options.client();
    let pending: Interactions = client
        .get(&format!("/v1/sessions/{session}/interactions"))
        .await?;

    if option_id.is_none() && !deny {
        if options.json {
            return print_json(&pending.interactions);
        }
        if pending.interactions.is_empty() {
            println!("\n  nothing is waiting for a decision\n");
            return Ok(());
        }
        println!();
        for request in &pending.interactions {
            println!("  {} · {}", request.agent, request.title);
            println!("  {:<12} {}", "interaction", request.id);
            for option in &request.options {
                println!("      {:<24} {} ({})", option.id, option.name, option.kind);
            }
            println!(
                "\n  oryxa approve {session} --interaction {} --option <id>\n",
                request.id
            );
        }
        return Ok(());
    }

    // With one request waiting, naming it is ceremony. With several, guessing
    // which one is being answered would be a decision made on the user's behalf
    // about a permission — so that is the one case this refuses.
    let id = match interaction {
        Some(id) => id,
        None => match pending.interactions.as_slice() {
            [only] => only.id.clone(),
            [] => bail!("nothing is waiting for a decision"),
            many => bail!(
                "{} requests are waiting — name one with --interaction",
                many.len()
            ),
        },
    };
    let body = if deny {
        json!({"cancel": true, "author": author})
    } else {
        json!({"option_id": option_id.unwrap_or_default(), "author": author})
    };
    let resolved: Value = client
        .post(
            &format!("/v1/sessions/{session}/interactions/{id}/resolve"),
            body,
        )
        .await?;
    if options.json {
        return print_json(&resolved);
    }
    println!(
        "\n  {} — the lane continues\n",
        resolved["outcome"].as_str().unwrap_or("resolved")
    );
    Ok(())
}

// ---- shared plumbing ----

pub async fn current_seq(client: &Client, session: &str) -> Result<i64> {
    let history: Events = client
        .get(&format!("/v1/sessions/{session}/events"))
        .await?;
    Ok(history.events.last().map(|event| event.seq).unwrap_or(0))
}

/// Prints a room's stream. Text parts are printed as they arrive so a streaming
/// agent reads like one, rather than appearing all at once when the turn ends.
///
/// `until_idle` is what makes `send -f` return: it stops once every turn that
/// started has finished, which is the room going quiet rather than a timeout.
async fn follow(
    client: &Client,
    session: &str,
    since: i64,
    raw: bool,
    until_idle: bool,
) -> Result<()> {
    let mut printer = Printer::new(raw);
    let mut running: BTreeSet<String> = BTreeSet::new();
    let mut started = false;

    let stream = client.events(session, since).await?;
    pin_mut!(stream);
    loop {
        let event = tokio::select! {
            event = stream.next() => match event {
                Some(event) => event?,
                None => break, // the server closed the stream
            },
            _ = tokio::signal::ctrl_c() => break,
        };
        printer.print(&event);
        match event.kind.as_str() {
            "turn.started" => {
                running.insert(event.turn.clone());
                started = true;
            }
            "turn.finished" | "turn.failed" | "turn.cancelled" | "turn.empty" => {
                running.remove(&event.turn);
                if until_idle && started && running.is_empty() {
                    break;
                }
            }
            _ => {}
        }
    }
    printer.end_line();
    Ok(())
}

fn short_age(age: chrono::TimeDelta) -> String {
    match age {
        age if age.num_minutes() < 1 => format!("{}s", age.num_seconds().max(0)),
        age if age.num_hours() < 1 => format!("{}m", age.num_minutes()),
        age if age.num_days() < 1 => format!("{}h", age.num_hours()),
        age => format!("{}d", age.num_days()),
    }
}

#[cfg(test)]
mod tests {
    use super::short_age;
    use chrono::TimeDelta;

    #[test]
    fn ages_read_as_one_unit() {
        assert_eq!(short_age(TimeDelta::seconds(9)), "9s");
        assert_eq!(short_age(TimeDelta::seconds(90)), "1m");
        assert_eq!(short_age(TimeDelta::hours(5)), "5h");
        assert_eq!(short_age(TimeDelta::days(3)), "3d");
    }
}
