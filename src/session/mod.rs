//! Room coordination.

mod manager;
mod model;
mod wake;

pub use manager::{Manager, SessionError};
pub use model::{Author, Input, State, Summary, Turn, TurnState, View};
pub use wake::{WakeDecision, names_for, who_wakes};
