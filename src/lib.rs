//! Oryxa's framework-neutral core.
//!
//! The Rust implementation is being introduced alongside the original Go
//! implementation. Public behavior is defined by `openapi.yaml`, the connector
//! files, and the existing event semantics; this crate must preserve them.

pub mod api;
pub mod cli;
pub mod connector;
pub mod events;
pub mod runtime;
pub mod session;
pub mod sharedctx;
pub mod tui;
