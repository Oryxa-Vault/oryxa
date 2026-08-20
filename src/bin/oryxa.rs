use std::{net::SocketAddr, path::PathBuf};

use anyhow::{Context, Result, bail};
use clap::{Args, Parser, Subcommand};
use oryxa::{
    acp_server,
    cli::commands::{self, ServerOptions},
    connector::{Executor, Registry, RenderContext},
    runtime::{Config, Runtime, attach_or_start},
    session::who_wakes,
    tui,
};

#[derive(Parser)]
#[command(
    name = "oryxa",
    version,
    about = "Many people, many agents, one room",
    // Run with no command and you get the room view. That is the product; the
    // subcommands below are the same thing for a script.
    args_conflicts_with_subcommands = true
)]
struct Cli {
    #[command(subcommand)]
    command: Option<Command>,
    /// Grant every permission an agent asks for, without asking anybody.
    /// Applies to a runtime this starts, not to one it attaches to.
    #[arg(short = 'e', long, env = "ORYXA_EXPRESS", default_value_t = false)]
    express: bool,
    /// Show the welcome screen again — what the keys are, and what a room can
    /// do to this machine.
    #[arg(long, default_value_t = false)]
    welcome: bool,
    #[command(flatten)]
    options: ServerOptions,
}

#[derive(Subcommand)]
enum Command {
    /// Run the API and embedded viewer.
    Serve(Serve),
    /// List connector files.
    Agents(ConnectorOptions),
    /// Show where one connector resolves.
    Which(Which),
    /// Probe one agent with a real turn.
    Check(Check),
    /// Explain which agents would answer a message.
    Wake(Wake),
    /// List the rooms on a server.
    Sessions(ServerOptions),
    /// Start a room with one or more agents.
    Open(Open),
    /// Say something to a room.
    Send(Send),
    /// Follow a room's live stream.
    Tail(Tail),
    /// Print a room's history.
    Replay(Replay),
    /// Read or write a room's shared context.
    Context(ContextArgs),
    /// Issue a key that speaks as one name.
    Key(Key),
    /// Stop what is running: one agent, or the whole room.
    Cancel(Cancel),
    /// Answer a coding agent waiting for permission.
    Approve(Approve),
    /// Serve a room to an editor over the Agent Client Protocol.
    Acp(Acp),
}

#[derive(Args)]
struct ConnectorOptions {
    #[arg(long, env = "ORYXA_CONNECTORS", default_value = "./connectors")]
    connectors: PathBuf,
    #[arg(long)]
    json: bool,
}

#[derive(Args)]
struct Serve {
    #[arg(long, default_value = "0.0.0.0:8080")]
    addr: SocketAddr,
    #[arg(long, env = "ORYXA_CONNECTORS", default_value = "./connectors")]
    connectors: PathBuf,
    #[arg(long, env = "ORYXA_DATABASE_URL")]
    db: Option<String>,
    /// Durable append-only event file for a local runtime.
    #[arg(long, env = "ORYXA_EVENT_FILE")]
    event_file: Option<PathBuf>,
    #[arg(long, env = "ORYXA_TOKEN", default_value = "")]
    token: String,
    #[arg(long, env = "ORYXA_ADMIN_TOKEN", default_value = "")]
    admin_token: String,
    #[arg(long, env = "ORYXA_TRUST_HEADER", default_value = "")]
    trust_header: String,
    #[arg(long, env = "ORYXA_SUMMARISER", default_value = "")]
    summariser: String,
    #[arg(long, env = "ORYXA_ALLOW_PRIVATE_AGENTS", default_value_t = false)]
    allow_private_agents: bool,
    #[arg(long, env = "ORYXA_ROOM_TURNS_PER_MIN", default_value_t = 30)]
    room_turns_per_min: i32,
    #[arg(long, env = "ORYXA_TURNS_PER_MIN", default_value_t = 120)]
    turns_per_min: i32,
    #[arg(long, env = "ORYXA_RESET", default_value_t = false)]
    reset: bool,
    /// Grant every permission an agent asks for, without asking anybody.
    #[arg(short = 'e', long, env = "ORYXA_EXPRESS", default_value_t = false)]
    express: bool,
}

#[derive(Args)]
struct Which {
    name: String,
    #[command(flatten)]
    options: ConnectorOptions,
}

#[derive(Args)]
struct Check {
    name: String,
    #[arg(long, default_value = "ping from oryxa check")]
    probe: String,
    #[command(flatten)]
    options: ConnectorOptions,
}

#[derive(Args)]
struct Wake {
    message: String,
    #[arg(long, value_delimiter = ',')]
    agents: Vec<String>,
    /// The people in the room. `--speakers` is the same flag under the name
    /// the Rust build shipped with.
    #[arg(long = "people", alias = "speakers", value_delimiter = ',')]
    speakers: Vec<String>,
    #[arg(long, value_delimiter = ',')]
    to: Vec<String>,
    #[command(flatten)]
    options: ConnectorOptions,
}

#[derive(Args)]
struct Open {
    /// The agents in the room.
    #[arg(required = true)]
    agents: Vec<String>,
    /// A workspace path handed to agents that ask for one.
    #[arg(long, default_value = "")]
    workspace: String,
    #[command(flatten)]
    options: ServerOptions,
}

#[derive(Args)]
struct Send {
    session: String,
    text: String,
    /// Who is speaking.
    #[arg(long = "as", default_value_t = speaker())]
    author: String,
    /// Direct it at these agents instead of letting the room decide.
    #[arg(long, value_delimiter = ',')]
    to: Vec<String>,
    /// Follow the answers instead of returning immediately.
    #[arg(short, long)]
    follow: bool,
    #[command(flatten)]
    options: ServerOptions,
}

#[derive(Args)]
struct Tail {
    session: String,
    /// Start from this sequence number; -1 means only what happens next.
    #[arg(long, default_value_t = -1, allow_negative_numbers = true)]
    since: i64,
    /// Print every event, including the agent's opaque activity.
    #[arg(long)]
    raw: bool,
    #[command(flatten)]
    options: ServerOptions,
}

#[derive(Args)]
struct Replay {
    session: String,
    #[arg(long)]
    raw: bool,
    #[command(flatten)]
    options: ServerOptions,
}

#[derive(Args)]
struct ContextArgs {
    session: String,
    /// Append to this key.
    #[arg(long)]
    append: Option<String>,
    /// Set this key.
    #[arg(long)]
    set: Option<String>,
    /// The value for --append or --set.
    #[arg(long, default_value = "")]
    value: String,
    #[arg(long = "as", default_value_t = speaker())]
    author: String,
    #[command(flatten)]
    options: ServerOptions,
}

#[derive(Args)]
struct Key {
    session: String,
    author: String,
    #[command(flatten)]
    options: ServerOptions,
}

#[derive(Args)]
struct Cancel {
    session: String,
    /// Stop only this agent. Short names work: `codex` stops `codex-local`.
    #[arg(long)]
    agent: Option<String>,
    #[command(flatten)]
    options: ServerOptions,
}

#[derive(Args)]
struct Approve {
    session: String,
    /// Which request to answer. Only needed when several are waiting.
    #[arg(long)]
    interaction: Option<String>,
    /// The option id the agent offered.
    #[arg(long)]
    option: Option<String>,
    /// Refuse the request instead of choosing an option.
    #[arg(long)]
    deny: bool,
    #[arg(long = "as", default_value_t = speaker())]
    author: String,
    #[command(flatten)]
    options: ServerOptions,
}

/// Oryxa as an ACP agent, launched by an editor rather than by a person.
///
/// stdout is the protocol's channel in this mode, so nothing here may print to
/// it. Anything worth saying goes to stderr, where the editor collects it as
/// the agent's log.
#[derive(Args)]
struct Acp {
    /// The agents a new room is opened with.
    #[arg(long, value_delimiter = ',', default_value = "claude-code,codex")]
    agents: Vec<String>,
    /// Join this room instead of opening one.
    #[arg(long)]
    room: Option<String>,
    /// Who the person in the editor speaks as.
    #[arg(long = "as", default_value_t = speaker())]
    author: String,
    /// Grant every permission an agent asks for, without asking anybody.
    #[arg(short = 'e', long, env = "ORYXA_EXPRESS", default_value_t = false)]
    express: bool,
    #[command(flatten)]
    options: ServerOptions,
}

/// The name a message is attributed to when nobody says otherwise.
fn speaker() -> String {
    std::env::var("USER").unwrap_or_else(|_| "cli".into())
}

#[tokio::main]
async fn main() -> Result<()> {
    let cli = Cli::parse();
    match cli.command {
        None => tui::run(cli.options, cli.express, cli.welcome).await,
        Some(Command::Serve(options)) => serve(options).await,
        Some(Command::Agents(options)) => agents(options),
        Some(Command::Which(options)) => which(options),
        Some(Command::Check(options)) => check(options).await,
        Some(Command::Wake(options)) => wake(options),
        Some(Command::Sessions(options)) => commands::sessions(options).await,
        Some(Command::Open(args)) => {
            commands::open(args.agents, args.workspace, args.options).await
        }
        Some(Command::Send(args)) => {
            commands::send(
                args.session,
                args.text,
                args.author,
                args.to,
                args.follow,
                args.options,
            )
            .await
        }
        Some(Command::Tail(args)) => {
            commands::tail(args.session, args.since, args.raw, args.options).await
        }
        Some(Command::Replay(args)) => commands::replay(args.session, args.raw, args.options).await,
        Some(Command::Context(args)) => {
            commands::context(
                args.session,
                args.append,
                args.set,
                args.value,
                args.author,
                args.options,
            )
            .await
        }
        Some(Command::Key(args)) => commands::key(args.session, args.author, args.options).await,
        Some(Command::Acp(args)) => acp(args).await,
        Some(Command::Cancel(args)) => {
            commands::cancel(args.session, args.agent, args.options).await
        }
        Some(Command::Approve(args)) => {
            commands::approve(
                args.session,
                args.interaction,
                args.option,
                args.deny,
                args.author,
                args.options,
            )
            .await
        }
    }
}

fn load_registry(path: &PathBuf) -> Result<Registry> {
    let registry = Registry::new();
    registry
        .load_dir(path)
        .with_context(|| format!("loading connectors from {}", path.display()))?;
    Ok(registry)
}

async fn serve(options: Serve) -> Result<()> {
    let connectors = options.connectors.clone();
    let authenticated = !options.token.is_empty();
    let started = Runtime::bind(Config {
        addr: options.addr,
        connectors: options.connectors,
        db: options.db,
        event_file: options.event_file,
        token: options.token,
        admin_token: options.admin_token,
        trust_header: options.trust_header,
        summariser: options.summariser,
        allow_private_agents: options.allow_private_agents,
        room_turns_per_min: options.room_turns_per_min,
        turns_per_min: options.turns_per_min,
        reset: options.reset,
        express: options.express,
    })
    .await?;

    if options.reset {
        eprintln!("oryxa: reset erased {} stream(s)", started.reset_streams);
    }
    println!("\n  oryxa {} · pilot", env!("CARGO_PKG_VERSION"));
    println!("  ├─ viewer      http://localhost:{}", started.addr.port());
    println!(
        "  ├─ auth        {}",
        if authenticated {
            "shared token"
        } else {
            "none"
        }
    );
    if started.express {
        println!("  ├─ express     ON — every permission granted without asking");
    }
    println!(
        "  └─ connectors  {} loaded from {}\n",
        started.connectors,
        connectors.display()
    );
    if started.restored_agents > 0 || started.restored_sessions > 0 {
        println!(
            "  restored     {} API agent(s), {} session(s)\n",
            started.restored_agents, started.restored_sessions
        );
    }
    started.serve().await
}

async fn acp(args: Acp) -> Result<()> {
    let (client, local) = attach_or_start(
        &args.options.server,
        &args.options.token,
        args.options.secret.clone(),
        args.express,
    )
    .await?;
    if local.is_some() {
        eprintln!(
            "oryxa: no server was running, so this agent started one at {}",
            client.base()
        );
    }
    acp_server::serve(acp_server::Config {
        client,
        agents: args.agents,
        room: args.room,
        author: args.author,
    })
    .await
}

fn agents(options: ConnectorOptions) -> Result<()> {
    let registry = load_registry(&options.connectors)?;
    let specs = registry.list();
    if options.json {
        let specs = specs
            .into_iter()
            .map(|spec| (*spec).clone())
            .collect::<Vec<_>>();
        println!("{}", serde_json::to_string_pretty(&specs)?);
        return Ok(());
    }
    println!("\n  {:<14} {:<44} CAPABILITIES", "AGENT", "TRANSPORT");
    for spec in specs {
        let transport = if let Some(acp) = &spec.acp {
            format!("ACP stdio · {}", acp.command)
        } else {
            RenderContext {
                vars: spec.vars.clone(),
                ..Default::default()
            }
            .render_string(&spec.base)
        };
        let capabilities = if spec.capabilities.is_empty() {
            "—".into()
        } else {
            spec.capabilities.join(",")
        };
        println!("  {:<14} {:<44} {}", spec.name, transport, capabilities);
    }
    println!();
    Ok(())
}

fn which(options: Which) -> Result<()> {
    let registry = load_registry(&options.options.connectors)?;
    let Some(spec) = registry.get(&options.name) else {
        bail!("no connector named {:?}", options.name)
    };
    let resolved = RenderContext {
        vars: spec.vars.clone(),
        ..Default::default()
    }
    .render_string(&spec.base);
    if options.options.json {
        println!(
            "{}",
            serde_json::to_string_pretty(&serde_json::json!({
                "name": spec.name, "base": spec.base, "resolved": resolved,
                "acp": spec.acp, "vars": spec.vars, "capabilities": spec.capabilities,
            }))?
        );
    } else {
        println!("\n  {}\n", spec.name);
        if let Some(acp) = &spec.acp {
            let render = RenderContext {
                vars: spec.vars.clone(),
                ..Default::default()
            };
            println!("  {:<12} ACP v1 over stdio", "transport");
            println!("  {:<12} {} {}", "command", acp.command, acp.args.join(" "));
            println!("  {:<12} {}", "workspace", acp.cwd);
            // An ACP command is usually written against the environment, and an
            // unset variable renders as nothing — which fails at spawn time with
            // an error about a program called "". Saying what it resolved to is
            // the whole reason this command exists.
            let command = render.render_string(&acp.command);
            let workspace = render.render_string(&acp.cwd);
            if command != acp.command || workspace != acp.cwd {
                println!();
                println!(
                    "  {:<12} {}",
                    "resolves to",
                    if command.is_empty() {
                        "(nothing — the variable is unset)".into()
                    } else {
                        command
                    }
                );
                println!(
                    "  {:<12} {}",
                    "workspace",
                    if workspace.is_empty() {
                        "(nothing — the variable is unset)".into()
                    } else {
                        workspace
                    }
                );
            }
        } else {
            println!("  {:<12} {}", "base", spec.base);
            if resolved != spec.base {
                println!("  {:<12} {}", "resolves to", resolved);
            }
            println!(
                "  {:<12} {} {}",
                "turn",
                if spec.turn.method.is_empty() {
                    "POST"
                } else {
                    &spec.turn.method
                },
                spec.turn.path
            );
        }
        if let Some(open) = &spec.open {
            println!(
                "  {:<12} {} {}",
                "open",
                if open.method.is_empty() {
                    "POST"
                } else {
                    &open.method
                },
                open.path
            );
        }
        println!();
    }
    Ok(())
}

async fn check(options: Check) -> Result<()> {
    let registry = load_registry(&options.options.connectors)?;
    let Some(spec) = registry.get(&options.name) else {
        bail!("no connector named {:?}", options.name)
    };
    let result = Executor::new().check(&spec, &options.probe).await;
    if options.options.json {
        println!("{}", serde_json::to_string_pretty(&result)?);
    } else {
        println!("\n  checking {}\n", options.name);
        println!(
            "  reachable    {}",
            if result.reachable { "ok" } else { "FAIL" }
        );
        if let Some(open) = &result.open {
            println!(
                "  open         {} {}",
                if open.ok { "ok" } else { "FAIL" },
                if open.ok { &open.handle } else { &open.error }
            );
        }
        if let Some(turn) = &result.turn {
            println!(
                "  turn         {} {}ms · {} parts · {} chars",
                if turn.ok { "ok" } else { "FAIL" },
                turn.ms,
                turn.parts,
                turn.text_len
            );
        }
        for warning in &result.warnings {
            println!("  warning      ! {warning}");
        }
        if !result.error.is_empty() {
            println!("  error        FAIL {}", result.error);
        }
        println!();
    }
    if result.ok {
        Ok(())
    } else {
        bail!(result.error)
    }
}

fn wake(options: Wake) -> Result<()> {
    let registry = load_registry(&options.options.connectors)?;
    let agents = if options.agents.is_empty() {
        registry
            .list()
            .into_iter()
            .map(|spec| spec.name.clone())
            .collect()
    } else {
        options.agents
    };
    let decision = who_wakes(
        &options.message,
        &options.to,
        &agents,
        &options.speakers,
        &registry,
    );
    if options.options.json {
        println!(
            "{}",
            serde_json::to_string_pretty(
                &serde_json::json!({"agents": decision.agents, "why": decision.why})
            )?
        );
    } else if decision.agents.is_empty() {
        println!("\n  nobody — {}\n", decision.why);
    } else {
        println!("\n  {} — {}\n", decision.agents.join(", "), decision.why);
    }
    Ok(())
}
