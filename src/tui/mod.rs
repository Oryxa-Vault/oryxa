//! The room view: Oryxa as an interactive terminal application.
//!
//! Running `oryxa` with no command lands here. If there is no server to talk
//! to it starts one in-process against a durable local event file, so an
//! installed binary is useful on a machine with nothing else set up. Pointing
//! it at a server with `--server` makes it an ordinary API client instead, and
//! then it is the same view over someone else's rooms.

pub mod app;
mod ui;

use std::{io::Stdout, path::PathBuf};

use anyhow::Result;
use crossterm::{
    ExecutableCommand,
    event::{
        DisableBracketedPaste, EnableBracketedPaste, Event, EventStream, KeyCode, KeyEvent,
        KeyEventKind, KeyModifiers,
    },
    terminal::{EnterAlternateScreen, LeaveAlternateScreen, disable_raw_mode, enable_raw_mode},
};
use futures_util::StreamExt;
use ratatui::{Terminal, backend::CrosstermBackend};
use tokio::sync::mpsc;

use crate::{
    cli::{Client, commands::ServerOptions},
    runtime::attach_or_start,
    tui::app::{App, Screen},
};

pub async fn run(options: ServerOptions) -> Result<()> {
    let (client, local) =
        attach_or_start(&options.server, &options.token, options.secret.clone()).await?;
    let mut terminal = enter()?;
    let result = loop_until_quit(&mut terminal, client, local).await;
    leave(terminal)?;
    result
}

type Screen0 = Terminal<CrosstermBackend<Stdout>>;

fn enter() -> Result<Screen0> {
    enable_raw_mode()?;
    let mut stdout = std::io::stdout();
    stdout.execute(EnterAlternateScreen)?;
    stdout.execute(EnableBracketedPaste)?;
    // A panic in raw mode leaves the terminal unusable and the message
    // invisible, which turns any bug in here into "my shell broke".
    let hook = std::panic::take_hook();
    std::panic::set_hook(Box::new(move |info| {
        let mut stdout = std::io::stdout();
        let _ = stdout.execute(DisableBracketedPaste);
        let _ = stdout.execute(LeaveAlternateScreen);
        let _ = disable_raw_mode();
        hook(info);
    }));
    Ok(Terminal::new(CrosstermBackend::new(stdout))?)
}

fn leave(mut terminal: Screen0) -> Result<()> {
    disable_raw_mode()?;
    terminal.backend_mut().execute(DisableBracketedPaste)?;
    terminal.backend_mut().execute(LeaveAlternateScreen)?;
    terminal.show_cursor()?;
    Ok(())
}

async fn loop_until_quit(
    terminal: &mut Screen0,
    client: Client,
    local: Option<PathBuf>,
) -> Result<()> {
    let (tx, mut rx) = mpsc::unbounded_channel();
    let mut app = App::new(client, tx, local);
    let mut keys = EventStream::new();

    loop {
        terminal.draw(|frame| ui::draw(frame, &mut app))?;
        tokio::select! {
            event = keys.next() => match event {
                Some(Ok(event)) => on_terminal_event(&mut app, event),
                Some(Err(error)) => return Err(error.into()),
                None => break,
            },
            message = rx.recv() => match message {
                Some(message) => app.on_message(message),
                None => break,
            },
        }
        if app.quit {
            break;
        }
    }
    Ok(())
}

fn on_terminal_event(app: &mut App, event: Event) {
    match event {
        Event::Key(key) if key.kind == KeyEventKind::Press => on_key(app, key),
        // A pasted block arrives as one event, so a multi-line paste does not
        // send a message per line.
        Event::Paste(text) => {
            if let Screen::Room(room) = &mut app.screen {
                for ch in text.chars() {
                    room.composer.insert(ch);
                }
            }
        }
        _ => {}
    }
}

fn on_key(app: &mut App, key: KeyEvent) {
    // Any key clears the last thing said in the footer, so a stale error does
    // not sit under a working interface.
    app.error = None;
    app.note = None;
    if app.help {
        app.help = false;
        return;
    }
    let control = key.modifiers.contains(KeyModifiers::CONTROL);
    let alt = key.modifiers.contains(KeyModifiers::ALT);

    match &mut app.screen {
        Screen::Rooms(screen) => {
            let last = screen.list.len().saturating_sub(1);
            match key.code {
                KeyCode::Char('q') | KeyCode::Esc => app.quit = true,
                KeyCode::Char('c') if control => app.quit = true,
                KeyCode::Char('n') => app.new_room(),
                KeyCode::Char('r') => app.refresh(),
                KeyCode::Char('?') => app.help = true,
                KeyCode::Up | KeyCode::Char('k') => {
                    screen.selected = screen.selected.saturating_sub(1);
                }
                KeyCode::Down | KeyCode::Char('j') => {
                    screen.selected = (screen.selected + 1).min(last);
                }
                KeyCode::Enter => {
                    if let Some(session) = screen.list.get(screen.selected) {
                        let (id, agents) = (session.id.clone(), session.agents.clone());
                        app.enter_room(id, agents);
                    }
                }
                _ => {}
            }
        }
        Screen::NewRoom(screen) => {
            let last = screen.connectors.len().saturating_sub(1);
            match key.code {
                KeyCode::Esc => app.leave_room(),
                KeyCode::Char('c') if control => app.quit = true,
                KeyCode::Up | KeyCode::Char('k') => {
                    screen.selected = screen.selected.saturating_sub(1);
                }
                KeyCode::Down | KeyCode::Char('j') => {
                    screen.selected = (screen.selected + 1).min(last);
                }
                KeyCode::Char(' ') => {
                    if !screen.chosen.insert(screen.selected) {
                        screen.chosen.remove(&screen.selected);
                    }
                }
                KeyCode::Enter => {
                    // Enter on a name nobody ticked opens a room with that one,
                    // which is what pressing enter on a list ought to mean.
                    let mut chosen = screen
                        .chosen
                        .iter()
                        .filter_map(|index| screen.connectors.get(*index).cloned())
                        .collect::<Vec<_>>();
                    if chosen.is_empty() {
                        chosen.extend(screen.connectors.get(screen.selected).cloned());
                    }
                    if !chosen.is_empty() {
                        app.open(chosen);
                    }
                }
                _ => {}
            }
        }
        Screen::Room(room) => {
            // The permission prompt takes the keyboard while it is open: the
            // agent is blocked on the answer, and a decision should not be
            // typed into the composer by accident.
            if let Some(selected) = room.approval {
                let options = room.pending.first().map(|r| r.options.len()).unwrap_or(0);
                match key.code {
                    KeyCode::Up => room.approval = Some(selected.saturating_sub(1)),
                    KeyCode::Down => {
                        room.approval = Some((selected + 1).min(options.saturating_sub(1)));
                    }
                    KeyCode::Enter => app.resolve(false),
                    KeyCode::Char('d') => app.resolve(true),
                    KeyCode::Esc => room.approval = None,
                    _ => {}
                }
                return;
            }
            if room.showing_context {
                room.showing_context = false;
                return;
            }
            match key.code {
                KeyCode::Esc => app.leave_room(),
                KeyCode::Char('c') if control => {
                    // Ctrl-C stops the work before it stops the session, which
                    // is the meaning it has everywhere else in a terminal.
                    if room.busy() {
                        app.cancel_turn();
                    } else {
                        app.quit = true;
                    }
                }
                KeyCode::Char('d') if control && room.composer.text.is_empty() => app.quit = true,
                KeyCode::Enter if alt => room.composer.insert('\n'),
                KeyCode::Enter => app.send(),
                KeyCode::Backspace => room.composer.backspace(),
                KeyCode::Delete => room.composer.delete(),
                KeyCode::Left => room.composer.left(),
                KeyCode::Right => room.composer.right(),
                KeyCode::Char('a') if control => room.composer.cursor = 0,
                KeyCode::Char('e') if control => {
                    room.composer.cursor = room.composer.text.len();
                }
                KeyCode::Home => room.composer.cursor = 0,
                KeyCode::End => room.composer.cursor = room.composer.text.len(),
                KeyCode::Char('u') if control => {
                    room.composer.text.clear();
                    room.composer.cursor = 0;
                }
                KeyCode::Char('w') if control => room.composer.kill_word(),
                KeyCode::Up => {
                    room.follow = false;
                    room.scroll = room.scroll.saturating_sub(1);
                }
                KeyCode::Down => {
                    room.scroll = room.scroll.saturating_add(1);
                }
                KeyCode::PageUp => {
                    room.follow = false;
                    room.scroll = room.scroll.saturating_sub(10);
                }
                KeyCode::PageDown => room.scroll = room.scroll.saturating_add(10),
                KeyCode::Char(ch) if !control => room.composer.insert(ch),
                _ => {}
            }
        }
    }
}
