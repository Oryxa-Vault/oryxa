//! Where an installed binary keeps things.
//!
//! Two directories, for two different kinds of file: configuration a person
//! writes and might copy between machines, and state the runtime owns and
//! would rather not lose.

use std::{env, path::PathBuf};

/// `~/.config/oryxa`, honouring `XDG_CONFIG_HOME`.
pub fn config_dir() -> Option<PathBuf> {
    let base = match env::var("XDG_CONFIG_HOME") {
        Ok(value) if !value.trim().is_empty() => PathBuf::from(value),
        _ => home()?.join(".config"),
    };
    Some(base.join("oryxa"))
}

/// Where the local runtime's event log lives.
///
/// macOS keeps application state in `Application Support`, and a file there is
/// backed up and migrated with everything else the user owns. Elsewhere this is
/// the XDG data directory.
pub fn data_dir() -> Option<PathBuf> {
    if cfg!(target_os = "macos") {
        return Some(home()?.join("Library/Application Support/Oryxa"));
    }
    let base = match env::var("XDG_DATA_HOME") {
        Ok(value) if !value.trim().is_empty() => PathBuf::from(value),
        _ => home()?.join(".local/share"),
    };
    Some(base.join("oryxa"))
}

/// The connector directory a local runtime should read.
///
/// `ORYXA_CONNECTORS` first, because every other command takes it and a runtime
/// started on your behalf should not be the one place it is ignored — an editor
/// launching this is setting environment, not passing flags.
///
/// Then `./connectors`, because inside a clone that is where the working
/// examples are and running there should use them. Otherwise the user's own,
/// which is what an installed binary in an unrelated directory should read
/// rather than reporting that a room has no agents.
pub fn connectors_dir() -> PathBuf {
    if let Ok(named) = env::var("ORYXA_CONNECTORS")
        && !named.trim().is_empty()
    {
        return PathBuf::from(named);
    }
    let local = PathBuf::from("./connectors");
    if local.is_dir() {
        return local;
    }
    config_dir()
        .map(|dir| dir.join("connectors"))
        .unwrap_or(local)
}

fn home() -> Option<PathBuf> {
    env::var("HOME").ok().map(PathBuf::from)
}
