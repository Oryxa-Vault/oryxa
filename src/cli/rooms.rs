//! Where the CLI keeps room secrets.
//!
//! `oryxa open` and the next `oryxa send` are two processes, so a secret held
//! in memory is a secret the second one does not have. Writing it down is what
//! makes the shell usable at all — the same trade every CLI with a credential
//! makes, and the same mitigations: the user's own config directory, mode
//! 0600, and one file rather than a secret per shell-history line.
//!
//! A flag and an environment variable come first, so nothing has to be written
//! down in CI or in a container that should hold no state.

use std::{
    collections::BTreeMap,
    env, fs,
    io::Write,
    path::{Path, PathBuf},
};

use crate::cli::paths;

const SECRET_ENV: &str = "ORYXA_SESSION_SECRET";

/// `~/.config/oryxa/rooms.json`, honouring `XDG_CONFIG_HOME`.
///
/// The path is the one the Go CLI used, so a machine that already has rooms
/// written down keeps them across the change of implementation.
pub fn path() -> Option<PathBuf> {
    Some(paths::config_dir()?.join("rooms.json"))
}

pub fn load() -> BTreeMap<String, String> {
    path()
        .and_then(|path| fs::read(path).ok())
        .and_then(|raw| serde_json::from_slice(&raw).ok())
        .unwrap_or_default()
}

/// Writes one secret down. Failures are silent on purpose: not being able to
/// save it makes the next command ask for it, which is inconvenient, and
/// refusing to open the room over it would be worse.
pub fn remember(id: &str, secret: &str) {
    if id.is_empty() || secret.is_empty() {
        return;
    }
    let Some(path) = path() else { return };
    let mut rooms = load();
    rooms.insert(id.to_string(), secret.to_string());
    let Ok(body) = serde_json::to_vec_pretty(&rooms) else {
        return;
    };
    let _ = write_private(&path, &body);
}

/// Finds a room's secret: the flag, then the environment, then what was written
/// down when the room was opened.
pub fn secret(id: &str, flag: Option<&str>) -> Option<String> {
    if let Some(value) = flag.map(str::trim).filter(|value| !value.is_empty()) {
        return Some(value.to_string());
    }
    if let Ok(value) = env::var(SECRET_ENV)
        && !value.trim().is_empty()
    {
        return Some(value.trim().to_string());
    }
    load().get(id).cloned()
}

/// Pulls a session id out of `/v1/sessions/{id}/...`, so every request carries
/// its own room's secret without each command remembering to.
pub fn session_from_path(path: &str) -> Option<&str> {
    let rest = path.strip_prefix("/v1/sessions/")?;
    let rest = match rest.find('/') {
        Some(index) => &rest[..index],
        None => rest,
    };
    // Trim a query string, so `/stream?since=0` does not become part of the id.
    let rest = match rest.find('?') {
        Some(index) => &rest[..index],
        None => rest,
    };
    (!rest.is_empty()).then_some(rest)
}

/// Creates the file mode 0600 before anything is in it. Writing it
/// world-readable and then narrowing leaves a window, and the window is the
/// whole file.
fn write_private(path: &Path, body: &[u8]) -> std::io::Result<()> {
    if let Some(parent) = path.parent() {
        fs::create_dir_all(parent)?;
    }
    let mut options = fs::OpenOptions::new();
    options.write(true).create(true).truncate(true);
    #[cfg(unix)]
    {
        use std::os::unix::fs::OpenOptionsExt;
        options.mode(0o600);
    }
    options.open(path)?.write_all(body)
}

#[cfg(test)]
mod tests {
    use super::session_from_path;

    #[test]
    fn reads_the_session_out_of_every_room_path() {
        assert_eq!(session_from_path("/v1/sessions/s_1"), Some("s_1"));
        assert_eq!(session_from_path("/v1/sessions/s_1/input"), Some("s_1"));
        assert_eq!(
            session_from_path("/v1/sessions/s_1/stream?since=4"),
            Some("s_1")
        );
        assert_eq!(session_from_path("/v1/agents"), None);
    }
}
