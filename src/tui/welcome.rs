//! The first screen, and the only place the risks are stated before anything
//! can act on a machine.
//!
//! It is shown until it has been read once, then never again unless asked for
//! with `--welcome`. A splash that reappears every launch is one people learn
//! to dismiss without reading, which would defeat the half of this that
//! matters.

use std::{fs, path::PathBuf};

use ratatui::{
    Frame,
    layout::{Constraint, Layout, Rect},
    style::{Color, Modifier, Style},
    text::{Line, Span},
    widgets::Paragraph,
};

use crate::cli::paths;

const WORDMARK: [&str; 5] = [
    " ██████  ██████  ██    ██ ██   ██  █████ ",
    "██    ██ ██   ██  ██  ██   ██ ██  ██   ██",
    "██    ██ ██████    ████     ███   ███████",
    "██    ██ ██   ██    ██     ██ ██  ██   ██",
    " ██████  ██   ██    ██    ██   ██ ██   ██",
];

const ACCENT: Color = Color::Green;
const DIM: Color = Color::DarkGray;

/// Where the fact that it has been seen is kept.
///
/// Beside the room secrets rather than in the event log: this is about a person
/// and their terminal, not about anything that happened in a room.
fn seen_marker() -> Option<PathBuf> {
    Some(paths::config_dir()?.join("welcomed"))
}

pub fn already_seen() -> bool {
    seen_marker().is_some_and(|path| path.exists())
}

/// Records that it has been read. Failing is not worth a word to the user —
/// the cost is seeing this again.
pub fn remember_seen() {
    let Some(path) = seen_marker() else { return };
    if let Some(parent) = path.parent() {
        let _ = fs::create_dir_all(parent);
    }
    let _ = fs::write(path, "");
}

pub fn draw(frame: &mut Frame, area: Rect, workspace: &str) {
    let mut lines = Vec::new();
    lines.push(Line::from(Span::styled(
        "   Multiplayer AI — many people and many agents in one room, live.",
        Style::new().fg(DIM),
    )));
    lines.push(Line::default());
    lines.push(Line::from(vec![
        Span::raw("   "),
        Span::styled(
            format!("v{}", env!("CARGO_PKG_VERSION")),
            Style::new().add_modifier(Modifier::BOLD),
        ),
        Span::raw("  "),
        Span::styled(
            " pilot ",
            Style::new()
                .fg(Color::Black)
                .bg(ACCENT)
                .add_modifier(Modifier::BOLD),
        ),
    ]));
    // What pilot means in practice, rather than as a label. Someone deciding
    // whether to build on this needs the second sentence, not the badge.
    for line in [
        "You are in the pilot phase of Oryxa's multiplayer framework. Rooms, lanes",
        "and the log work; names, flags and connector shapes can still change.",
    ] {
        lines.push(Line::from(Span::styled(
            format!("   {line}"),
            Style::new().fg(DIM),
        )));
    }
    lines.push(Line::default());

    section(&mut lines, "Getting started");
    for (key, what) in [
        ("n", "open a room, and choose the agents in it with space"),
        (
            "enter",
            "say something · a leading @agent aims it at one of them",
        ),
        ("esc", "back · ctrl-c stops the turn that is running"),
        ("/help", "every key and every command"),
    ] {
        lines.push(Line::from(vec![
            Span::raw("     "),
            Span::styled(format!("{key:<8}"), Style::new().fg(ACCENT)),
            Span::styled(what, Style::new()),
        ]));
    }
    lines.push(Line::default());

    // Stated before anything can act, because after is too late for the only
    // one of these that cannot be undone.
    section(&mut lines, "What a room can do to this machine");
    if !workspace.is_empty() {
        // The directory, named, before anything has been opened in it. This is
        // where you ran the command, and it is what the agents get.
        lines.push(Line::from(vec![
            Span::raw("     "),
            Span::styled("workspace  ", Style::new().fg(DIM)),
            Span::styled(
                workspace.to_string(),
                Style::new().fg(Color::Yellow).add_modifier(Modifier::BOLD),
            ),
        ]));
    }
    for warning in [
        "Rooms opened here work in that directory, and the coding agents in them",
        "read and write in it as local processes. Anyone who can reach the room",
        "can cause that, so run this somewhere you would not mind an agent",
        "editing. --workspace names a different one.",
    ] {
        lines.push(Line::from(Span::styled(
            format!("     {warning}"),
            Style::new().fg(Color::Yellow),
        )));
    }
    lines.push(Line::default());

    section(&mut lines, "Where the rest is written down");
    lines.push(Line::from(Span::styled(
        "     docs/cli.md   the room view, the commands, installing",
        Style::new(),
    )));
    lines.push(Line::from(Span::styled(
        "     SECURITY.md   the posture, and what it does not cover",
        Style::new(),
    )));
    lines.push(Line::from(Span::styled(
        "     oryxa.in      everything else",
        Style::new(),
    )));
    lines.push(Line::default());
    lines.push(Line::from(Span::styled(
        "   any key to begin — this is shown once, and again with --welcome",
        Style::new().fg(DIM).add_modifier(Modifier::ITALIC),
    )));

    // The wordmark is the part worth losing. On a short terminal it is the
    // difference between a warning that is read and one that is scrolled off
    // the bottom, so it goes on only once everything else fits.
    let wordmark = WORDMARK.len() as u16 + 2;
    if area.height >= lines.len() as u16 + wordmark {
        let mut top = vec![Line::default()];
        for row in WORDMARK {
            top.push(Line::from(Span::styled(
                format!("   {row}"),
                Style::new().fg(ACCENT).add_modifier(Modifier::BOLD),
            )));
        }
        top.push(Line::default());
        top.extend(lines);
        lines = top;
    }

    // Centred vertically when there is room, and simply from the top when
    // there is not, because a clipped warning is worse than an unbalanced one.
    let height = lines.len() as u16;
    let top = area.height.saturating_sub(height) / 2;
    let [_, body] = Layout::vertical([Constraint::Length(top), Constraint::Min(0)]).areas(area);
    frame.render_widget(Paragraph::new(lines), body);
}

fn section(lines: &mut Vec<Line<'static>>, title: &str) {
    lines.push(Line::from(Span::styled(
        format!("   {title}"),
        Style::new().add_modifier(Modifier::BOLD),
    )));
}
