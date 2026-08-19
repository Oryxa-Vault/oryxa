//! Starting a server, once, for both the `serve` command and the room view.
//!
//! The room view runs a runtime in-process when there is no server to talk to,
//! which is what makes an installed binary useful on its own. That is the same
//! bootstrap `serve` performs, so it lives here rather than being written twice
//! and drifting.

use std::{net::SocketAddr, path::PathBuf, sync::Arc};

use anyhow::{Context, Result};
use tokio::net::TcpListener;

use crate::{
    api::{AppState, restore_agents, router},
    connector::{Executor, Origin, Registry},
    events::{FileStore, MemoryStore, PostgresStore, Store},
    session::Manager,
};

pub struct Config {
    pub addr: SocketAddr,
    pub connectors: PathBuf,
    /// PostgreSQL DSN. Takes precedence over `event_file`.
    pub db: Option<String>,
    /// A durable append-only file, for a runtime that belongs to one person.
    pub event_file: Option<PathBuf>,
    pub token: String,
    pub admin_token: String,
    pub trust_header: String,
    pub summariser: String,
    pub allow_private_agents: bool,
    pub room_turns_per_min: i32,
    pub turns_per_min: i32,
    pub reset: bool,
}

impl Default for Config {
    fn default() -> Self {
        Self {
            addr: ([0, 0, 0, 0], 8080).into(),
            connectors: PathBuf::from("./connectors"),
            db: None,
            event_file: None,
            token: String::new(),
            admin_token: String::new(),
            trust_header: String::new(),
            summariser: String::new(),
            allow_private_agents: false,
            room_turns_per_min: 30,
            turns_per_min: 120,
            reset: false,
        }
    }
}

/// A bound, ready server.
///
/// Binding is separated from serving so a caller can learn the port before the
/// first request. The room view asks for port 0 and needs the answer.
pub struct Runtime {
    pub addr: SocketAddr,
    pub connectors: usize,
    pub restored_agents: usize,
    pub restored_sessions: usize,
    pub reset_streams: usize,
    listener: TcpListener,
    router: axum::Router,
}

impl Runtime {
    pub async fn bind(config: Config) -> Result<Self> {
        let registry = Registry::new();
        registry
            .load_dir(&config.connectors)
            .with_context(|| format!("loading connectors from {}", config.connectors.display()))?;

        let events: Arc<dyn Store> = if let Some(dsn) = config
            .db
            .as_deref()
            .filter(|value| !value.trim().is_empty())
        {
            Arc::new(
                PostgresStore::connect(dsn)
                    .await
                    .context("open postgres event store")?,
            )
        } else if let Some(path) = &config.event_file {
            Arc::new(
                FileStore::open(path)
                    .await
                    .with_context(|| format!("open local event store {}", path.display()))?,
            )
        } else {
            Arc::new(MemoryStore::new())
        };
        let reset_streams = if config.reset {
            events.reset().await?
        } else {
            0
        };

        let origin = if config.allow_private_agents {
            Origin::ApiPrivate
        } else {
            Origin::Api
        };
        let restored_agents = restore_agents(events.as_ref(), &registry, origin)
            .await
            .context("restore API-registered agents")?;
        let executor = Executor::new();
        let manager = Manager::configured(
            registry.clone(),
            executor.clone(),
            events.clone(),
            config.summariser.clone(),
        );
        let restored_sessions = manager.rehydrate().await.context("restore sessions")?;
        let connectors = registry.list().len();

        let mut state = AppState::new(registry, executor, manager, events)
            .with_turn_limits(config.room_turns_per_min, config.turns_per_min)
            .with_private_agents(config.allow_private_agents);
        state.token = config.token;
        state.admin_token = config.admin_token;
        state.trust_header = config.trust_header;

        let listener = TcpListener::bind(config.addr)
            .await
            .with_context(|| format!("listen on {}", config.addr))?;
        Ok(Self {
            addr: listener.local_addr()?,
            connectors,
            restored_agents,
            restored_sessions,
            reset_streams,
            listener,
            router: router(state),
        })
    }

    /// Serves until the process is interrupted.
    pub async fn serve(self) -> Result<()> {
        axum::serve(self.listener, self.router)
            .with_graceful_shutdown(async {
                let _ = tokio::signal::ctrl_c().await;
            })
            .await?;
        Ok(())
    }

    /// Serves in the background, for a caller that has its own reason to live.
    ///
    /// Deliberately without the ctrl-c shutdown above: the room view wants that
    /// key for itself, and the runtime should outlive a cancelled turn.
    pub fn spawn(self) -> tokio::task::JoinHandle<()> {
        tokio::spawn(async move {
            let _ = axum::serve(self.listener, self.router).await;
        })
    }
}
