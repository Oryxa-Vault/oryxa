//! Drawing.
//!
//! Text is wrapped here rather than by `Paragraph`, because the scrollback has
//! to be addressed in the lines a reader actually sees. Wrapping at render time
//! and scrolling by source line disagree the moment anything is longer than the
//! pane, which reads as the view jumping while an agent is speaking.

use ratatui::{
    Frame,
    layout::{Constraint, Layout, Rect},
    style::{Color, Modifier, Style},
    text::{Line, Span},
    widgets::{Block, Clear, List, ListItem, ListState, Paragraph},
};
use unicode_width::{UnicodeWidthChar, UnicodeWidthStr};

use crate::tui::app::{App, Screen, Voice, context_lines};

/// Restrained on purpose: green is the product's signal, and everything else
/// is there to separate a person from an agent from a note.
const ACCENT: Color = Color::Green;
const DIM: Color = Color::DarkGray;
const AGENT_COLORS: [Color; 6] = [
    Color::Cyan,
    Color::Magenta,
    Color::Yellow,
    Color::Blue,
    Color::LightGreen,
    Color::LightRed,
];

/// A stable colour per agent, so the same name is the same colour every run.
fn agent_color(name: &str) -> Color {
    let sum: usize = name.bytes().map(usize::from).sum();
    AGENT_COLORS[sum % AGENT_COLORS.len()]
}

pub fn draw(frame: &mut Frame, app: &mut App) {
    let area = frame.area();
    // Before anything else and instead of everything else: the risks are on it,
    // and a warning behind a room is a warning nobody read.
    if app.welcome {
        crate::tui::welcome::draw(frame, area);
        return;
    }
    let [header, body, footer] = Layout::vertical([
        Constraint::Length(1),
        Constraint::Min(3),
        Constraint::Length(1),
    ])
    .areas(area);

    draw_header(frame, app, header);
    match &mut app.screen {
        Screen::Rooms(_) => draw_rooms(frame, app, body),
        Screen::NewRoom(_) => draw_new_room(frame, app, body),
        Screen::Room(_) => draw_room(frame, app, body),
    }
    draw_footer(frame, app, footer);

    if app.help {
        draw_help(frame, area);
    }
}

fn draw_header(frame: &mut Frame, app: &App, area: Rect) {
    let mut spans = vec![
        Span::styled(" oryxa ", Style::new().fg(Color::Black).bg(ACCENT).bold()),
        Span::raw(" "),
    ];
    match &app.screen {
        Screen::Rooms(_) => spans.push(Span::styled("rooms", Style::new().bold())),
        Screen::NewRoom(_) => spans.push(Span::styled("new room", Style::new().bold())),
        Screen::Room(room) => {
            spans.push(Span::styled(room.id.clone(), Style::new().bold()));
            spans.push(Span::styled(
                format!("  {}", room.agents.join(", ")),
                Style::new().fg(DIM),
            ));
            if room.busy() {
                spans.push(Span::styled("  ● thinking", Style::new().fg(ACCENT)));
            }
            if !room.live {
                spans.push(Span::styled(
                    "  ○ reconnecting",
                    Style::new().fg(Color::Red),
                ));
            }
        }
    }
    if app.express {
        spans.push(Span::styled(
            "  ⚡ express",
            Style::new().fg(Color::Black).bg(Color::Yellow).bold(),
        ));
    }
    let right = if app.local_connectors.is_some() {
        format!("local runtime · {} ", app.server)
    } else {
        format!("{} ", app.server)
    };
    let used: usize = spans.iter().map(|span| span.content.width()).sum();
    let gap = area.width as usize - used.min(area.width as usize);
    let padding = gap.saturating_sub(right.width());
    spans.push(Span::raw(" ".repeat(padding)));
    spans.push(Span::styled(right, Style::new().fg(DIM)));
    frame.render_widget(Paragraph::new(Line::from(spans)), area);
}

fn draw_footer(frame: &mut Frame, app: &App, area: Rect) {
    if let Some(error) = &app.error {
        let text = format!(" {error}");
        frame.render_widget(
            Paragraph::new(text).style(Style::new().fg(Color::Red)),
            area,
        );
        return;
    }
    if let Some(note) = &app.note {
        frame.render_widget(
            Paragraph::new(format!(" {note}")).style(Style::new().fg(ACCENT)),
            area,
        );
        return;
    }
    let hint = match &app.screen {
        Screen::Rooms(_) => "↑↓ select · enter open · n new room · r refresh · q quit",
        Screen::NewRoom(_) => "↑↓ select · space choose · enter open · esc back",
        Screen::Room(room) if room.approval.is_some() => {
            "↑↓ option · enter allow · d deny · esc decide later"
        }
        Screen::Room(_) => "enter send · @agent to direct · /help · esc rooms · ctrl-c cancel",
    };
    frame.render_widget(
        Paragraph::new(format!(" {hint}")).style(Style::new().fg(DIM)),
        area,
    );
}

// ---- rooms ----

fn draw_rooms(frame: &mut Frame, app: &mut App, area: Rect) {
    let Screen::Rooms(screen) = &mut app.screen else {
        return;
    };
    if screen.list.is_empty() {
        let text = if screen.loading {
            "\n  looking for rooms…"
        } else {
            "\n  no rooms yet.\n\n  press n to open one with the agents this server can reach."
        };
        frame.render_widget(Paragraph::new(text).style(Style::new().fg(DIM)), area);
        return;
    }
    let items = screen
        .list
        .iter()
        .map(|session| {
            ListItem::new(Line::from(vec![
                Span::styled(format!("{:<24}", session.id), Style::new().bold()),
                Span::styled(
                    format!("{:<9}", format!("{:?}", session.state).to_lowercase()),
                    Style::new().fg(DIM),
                ),
                Span::styled(session.agents.join(", "), Style::new().fg(ACCENT)),
            ]))
        })
        .collect::<Vec<_>>();
    let mut state = ListState::default().with_selected(Some(screen.selected));
    frame.render_stateful_widget(
        List::new(items)
            .block(Block::new().padding(ratatui::widgets::Padding::horizontal(1)))
            .highlight_style(Style::new().add_modifier(Modifier::REVERSED)),
        area,
        &mut state,
    );
}

fn draw_new_room(frame: &mut Frame, app: &mut App, area: Rect) {
    let Screen::NewRoom(screen) = &mut app.screen else {
        return;
    };
    if screen.connectors.is_empty() {
        // A fresh install has no agents and no way to guess where they go, so
        // the runtime this view started says which directory it read.
        let text = match &app.local_connectors {
            Some(path) => format!(
                "\n  no agents yet.\n\n  connectors are files, and this runtime reads them from\n\n    {}\n\n  put one there and press r.",
                path.display()
            ),
            None => "\n  this server has no connectors.\n\n  connectors are files — point the runtime at a directory of them."
                .into(),
        };
        frame.render_widget(Paragraph::new(text).style(Style::new().fg(DIM)), area);
        return;
    }
    let items = screen
        .connectors
        .iter()
        .enumerate()
        .map(|(index, name)| {
            let chosen = screen.chosen.contains(&index);
            ListItem::new(Line::from(vec![
                Span::styled(
                    if chosen { "  ✓ " } else { "  · " },
                    Style::new().fg(if chosen { ACCENT } else { DIM }),
                ),
                Span::raw(name.clone()),
            ]))
        })
        .collect::<Vec<_>>();
    let mut state = ListState::default().with_selected(Some(screen.selected));
    frame.render_stateful_widget(
        List::new(items).highlight_style(Style::new().add_modifier(Modifier::REVERSED)),
        area,
        &mut state,
    );
}

// ---- one room ----

fn draw_room(frame: &mut Frame, app: &mut App, area: Rect) {
    let composer_height = {
        let Screen::Room(room) = &app.screen else {
            return;
        };
        // Grows with the message being written, up to a point: past that the
        // transcript matters more than seeing every line of a long paste.
        let lines = room.composer.text.lines().count().max(1);
        (lines as u16 + 2).min(8)
    };
    let [transcript, composer] =
        Layout::vertical([Constraint::Min(3), Constraint::Length(composer_height)]).areas(area);

    let width = transcript.width.saturating_sub(2).max(10) as usize;
    let Screen::Room(room) = &mut app.screen else {
        return;
    };

    let mut lines: Vec<Line> = Vec::new();
    for said in &room.said {
        match said.voice {
            Voice::Person => {
                lines.push(Line::default());
                for (index, part) in wrap(&said.text, width.saturating_sub(said.who.width() + 3))
                    .into_iter()
                    .enumerate()
                {
                    let prefix = if index == 0 {
                        Span::styled(format!(" {} › ", said.who), Style::new().bold())
                    } else {
                        Span::raw(" ".repeat(said.who.width() + 4))
                    };
                    lines.push(Line::from(vec![prefix, Span::raw(part)]));
                }
            }
            Voice::Agent => {
                lines.push(Line::default());
                let color = agent_color(&said.who);
                for (index, part) in wrap(&said.text, width.saturating_sub(said.who.width() + 3))
                    .into_iter()
                    .enumerate()
                {
                    let prefix = if index == 0 {
                        Span::styled(format!(" {} ‹ ", said.who), Style::new().fg(color).bold())
                    } else {
                        Span::raw(" ".repeat(said.who.width() + 4))
                    };
                    lines.push(Line::from(vec![prefix, Span::raw(part)]));
                }
            }
            Voice::Note => {
                for part in wrap(&said.text, width.saturating_sub(3)) {
                    lines.push(Line::from(Span::styled(
                        format!(" · {part}"),
                        Style::new().fg(DIM),
                    )));
                }
            }
        }
    }
    if lines.is_empty() {
        lines.push(Line::from(Span::styled(
            "\n  say something to the room. @agent directs it at one of them.",
            Style::new().fg(DIM),
        )));
    }

    // Stuck to the bottom unless the reader has scrolled up, which is the
    // behaviour a live log needs: new text should not move what is being read.
    let visible = transcript.height as usize;
    let max_scroll = lines.len().saturating_sub(visible) as u16;
    if room.follow {
        room.scroll = max_scroll;
    } else {
        room.scroll = room.scroll.min(max_scroll);
        // Scrolled back to the bottom, so stick there again. Doing this here
        // rather than in the key handler is what makes it true however the
        // reader got to the end.
        room.follow = room.scroll == max_scroll;
    }
    let scroll = room.scroll;
    frame.render_widget(Paragraph::new(lines).scroll((scroll, 0)), transcript);

    let title = if room.composer.text.starts_with('/') {
        " command "
    } else {
        " message "
    };
    let composer_block = Block::bordered()
        .border_style(Style::new().fg(if room.busy() { ACCENT } else { DIM }))
        .title(Span::styled(title, Style::new().fg(DIM)));
    let inner = composer_block.inner(composer);
    frame.render_widget(composer_block, composer);
    frame.render_widget(Paragraph::new(room.composer.text.as_str()), inner);

    // The cursor sits where the next character will land, counting display
    // width so a wide character does not push it out of place.
    let before = &room.composer.text[..room.composer.cursor];
    let row = before.matches('\n').count() as u16;
    let column = before.rsplit('\n').next().unwrap_or_default().width() as u16;
    frame.set_cursor_position((
        inner.x + column.min(inner.width.saturating_sub(1)),
        inner.y + row.min(inner.height.saturating_sub(1)),
    ));

    if room.showing_context {
        draw_panel(
            frame,
            area,
            " shared context ",
            &context_lines(&room.context),
        );
    }
    if let Some(selected) = room.approval
        && let Some(request) = room.pending.first()
    {
        draw_approval(frame, area, request, selected, room.pending.len());
    }
}

fn draw_approval(
    frame: &mut Frame,
    area: Rect,
    request: &crate::connector::PendingInteraction,
    selected: usize,
    waiting: usize,
) {
    let mut lines = vec![
        Line::from(Span::styled(
            format!(" {} is asking to act", request.agent),
            Style::new().fg(Color::Yellow).bold(),
        )),
        Line::default(),
    ];
    for part in wrap(&request.title, area.width.saturating_sub(8) as usize) {
        lines.push(Line::from(format!(" {part}")));
    }
    lines.push(Line::default());
    for (index, option) in request.options.iter().enumerate() {
        let marker = if index == selected { "›" } else { " " };
        lines.push(Line::from(Span::styled(
            format!(" {marker} {}  ({})", option.name, option.kind),
            if index == selected {
                Style::new().fg(ACCENT).bold()
            } else {
                Style::new()
            },
        )));
    }
    if waiting > 1 {
        lines.push(Line::default());
        lines.push(Line::from(Span::styled(
            format!(" {} more waiting", waiting - 1),
            Style::new().fg(DIM),
        )));
    }
    let height = (lines.len() as u16 + 2).min(area.height);
    let popup = centered(area, area.width.saturating_sub(8).min(72), height);
    frame.render_widget(Clear, popup);
    frame.render_widget(
        Paragraph::new(lines).block(
            Block::bordered()
                .border_style(Style::new().fg(Color::Yellow))
                .title(Span::styled(
                    " permission ",
                    Style::new().fg(Color::Yellow).bold(),
                )),
        ),
        popup,
    );
}

fn draw_panel(frame: &mut Frame, area: Rect, title: &str, body: &[String]) {
    let width = area.width.saturating_sub(8).min(80);
    let mut lines = Vec::new();
    for entry in body {
        for part in wrap(entry, width.saturating_sub(4) as usize) {
            lines.push(Line::from(format!(" {part}")));
        }
    }
    let height = (lines.len() as u16 + 2).min(area.height);
    let popup = centered(area, width, height);
    frame.render_widget(Clear, popup);
    frame.render_widget(
        Paragraph::new(lines).block(
            Block::bordered()
                .border_style(Style::new().fg(DIM))
                .title(Span::styled(title.to_string(), Style::new().bold())),
        ),
        popup,
    );
}

fn draw_help(frame: &mut Frame, area: Rect) {
    let body = [
        "enter          send · @agent at the front directs it at one agent",
        "alt+enter      newline",
        "esc            back to the room list, or close a panel",
        "ctrl-c         cancel the running turn, or quit when nothing is running",
        "↑ ↓ pgup pgdn  scroll · scrolling back to the bottom follows live again",
        "",
        "/context       what the room knows, alongside the transcript",
        "/cancel [agent] stop one agent, or every running turn",
        "/close         close the room",
        "/key NAME      issue a key that speaks as NAME, shown once",
        "/as NAME       speak as someone else",
        "/raw           show the agent's opaque activity too",
        "/rooms  /new   the room list, or open another room",
        "/quit          leave",
    ]
    .map(String::from);
    draw_panel(frame, area, " keys and commands ", &body);
}

fn centered(area: Rect, width: u16, height: u16) -> Rect {
    let width = width.min(area.width);
    let height = height.min(area.height);
    Rect {
        x: area.x + (area.width - width) / 2,
        y: area.y + (area.height - height) / 2,
        width,
        height,
    }
}

/// Wraps on whitespace, measuring display width rather than characters.
///
/// A word longer than the pane is broken rather than allowed to run off the
/// edge — that word is usually a path or a URL, and half of one is still
/// readable where none of it is not.
fn wrap(text: &str, width: usize) -> Vec<String> {
    let width = width.max(8);
    let mut out = Vec::new();
    for source in text.split('\n') {
        let mut line = String::new();
        for word in source.split_inclusive(' ') {
            if line.width() + word.trim_end().width() > width && !line.is_empty() {
                out.push(std::mem::take(&mut line));
            }
            let mut word = word;
            while word.width() > width {
                let mut head = String::new();
                for ch in word.chars() {
                    if head.width() + ch.width().unwrap_or(0) > width {
                        break;
                    }
                    head.push(ch);
                }
                word = &word[head.len()..];
                out.push(head);
            }
            line.push_str(word);
        }
        out.push(line);
    }
    out
}

#[cfg(test)]
mod tests {
    use super::wrap;

    #[test]
    fn wraps_on_words_and_keeps_blank_lines() {
        assert_eq!(
            wrap("the quick brown fox jumps", 12),
            vec!["the quick ", "brown fox ", "jumps"]
        );
        assert_eq!(wrap("a\n\nb", 10), vec!["a", "", "b"]);
    }

    #[test]
    fn breaks_a_word_that_cannot_fit() {
        let long = "/a/very/long/path/that/never/ends";
        let wrapped = wrap(long, 10);
        assert!(wrapped.iter().all(|line| line.chars().count() <= 10));
        assert_eq!(wrapped.concat(), long);
    }
}
