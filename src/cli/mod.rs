//! The client half of the binary: everything that talks to a running Oryxa
//! rather than serving one.
//!
//! It is separate from `api` on purpose. `check`, `which` and `agents` read
//! connector files and need no server; the commands here need one, and the
//! split is what keeps that difference obvious.

pub mod client;
pub mod commands;
pub mod paths;
pub mod printer;
pub mod rooms;

pub use client::Client;
