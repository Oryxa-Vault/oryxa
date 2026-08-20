use std::{
    collections::BTreeMap,
    sync::Arc,
    time::{Duration, Instant},
};

use oryxa::{
    connector::{AcpSpec, Executor, Origin, Registry, RenderContext, Spec, Step},
    events::{MemoryStore, Store},
    session::{Author, Manager, Turn, TurnState},
};
use tokio::time::sleep;

fn acp_spec(name: &str, delay_ms: u64) -> Spec {
    Spec {
        name: name.into(),
        base: String::new(),
        acp: Some(AcpSpec {
            command: env!("CARGO_BIN_EXE_mockacp").into(),
            args: Vec::new(),
            env: BTreeMap::from([
                ("MOCK_ACP_AGENT_ID".into(), name.into()),
                ("MOCK_ACP_DELAY_MS".into(), delay_ms.to_string()),
            ]),
            cwd: std::env::current_dir().unwrap().display().to_string(),
            prompt: String::new(),
        }),
        headers: BTreeMap::new(),
        vars: BTreeMap::new(),
        capabilities: vec!["streaming".into(), "sessions".into()],
        interests: Vec::new(),
        timeout: "5s".into(),
        open: None,
        turn: Step::default(),
        context: Vec::new(),
        origin: Origin::File,
    }
}

fn permission_spec(name: &str) -> Spec {
    let mut spec = acp_spec(name, 0);
    spec.acp
        .as_mut()
        .unwrap()
        .env
        .insert("MOCK_ACP_REQUIRE_PERMISSION".into(), "1".into());
    spec
}

async fn wait_history(manager: &Manager, id: &str, wanted: usize) -> Vec<Turn> {
    for _ in 0..300 {
        let history = manager.view(id).await.unwrap().history;
        if history.len() >= wanted {
            return history;
        }
        sleep(Duration::from_millis(10)).await;
    }
    panic!("ACP turn did not finish");
}

async fn wait_running(manager: &Manager, id: &str) {
    for _ in 0..200 {
        if manager.view(id).await.unwrap().current.is_some() {
            return;
        }
        sleep(Duration::from_millis(5)).await;
    }
    panic!("ACP turn did not start");
}

async fn wait_interaction(manager: &Manager, id: &str) -> oryxa::connector::PendingInteraction {
    for _ in 0..300 {
        if let Some(interaction) = manager
            .pending_interactions(id)
            .await
            .unwrap()
            .into_iter()
            .next()
        {
            return interaction;
        }
        sleep(Duration::from_millis(10)).await;
    }
    panic!("ACP permission request did not arrive");
}

#[tokio::test]
async fn acp_permission_waits_for_and_records_a_user_decision() {
    let registry = Registry::new();
    registry.put(permission_spec("guarded")).unwrap();
    let events = Arc::new(MemoryStore::new());
    let manager = Manager::new(registry, Executor::new(), events.clone());
    let (summary, _) = manager.create(&["guarded".into()]).await.unwrap();

    manager
        .submit(
            &summary.id,
            Author::claimed("alice"),
            "make a patch",
            &["guarded".into()],
        )
        .await
        .unwrap();
    let interaction = wait_interaction(&manager, &summary.id).await;
    assert_eq!(interaction.title, "Write the generated patch");
    assert!(manager.view(&summary.id).await.unwrap().history.is_empty());
    assert_eq!(interaction.options[0].kind, "allow_once");

    manager
        .resolve_interaction(&summary.id, &interaction.id, "alice", Some("allow-once"))
        .await
        .unwrap();
    let history = wait_history(&manager, &summary.id, 1).await;
    assert_eq!(history[0].state, TurnState::Done);
    assert!(history[0].output.ends_with(":make a patch"));
    assert!(
        manager
            .pending_interactions(&summary.id)
            .await
            .unwrap()
            .is_empty()
    );

    let recorded = events.since(&summary.id, 0).await.unwrap();
    assert!(
        recorded
            .iter()
            .any(|event| event.kind == "interaction.requested")
    );
    assert!(
        recorded
            .iter()
            .any(|event| event.kind == "interaction.resolution_requested")
    );
    assert!(
        recorded
            .iter()
            .any(|event| event.kind == "interaction.resolved")
    );
}

#[tokio::test]
async fn cancelling_a_turn_cancels_its_pending_acp_permission() {
    let registry = Registry::new();
    registry.put(permission_spec("guarded")).unwrap();
    let events = Arc::new(MemoryStore::new());
    let manager = Manager::new(registry, Executor::new(), events.clone());
    let (summary, _) = manager.create(&["guarded".into()]).await.unwrap();
    manager
        .submit(
            &summary.id,
            Author::claimed("alice"),
            "stop this",
            &["guarded".into()],
        )
        .await
        .unwrap();
    wait_interaction(&manager, &summary.id).await;

    manager.cancel(&summary.id, "alice", None).await.unwrap();
    let history = wait_history(&manager, &summary.id, 1).await;
    assert_eq!(history[0].state, TurnState::Cancelled);
    for _ in 0..100 {
        if manager
            .pending_interactions(&summary.id)
            .await
            .unwrap()
            .is_empty()
        {
            break;
        }
        sleep(Duration::from_millis(10)).await;
    }
    assert!(
        manager
            .pending_interactions(&summary.id)
            .await
            .unwrap()
            .is_empty()
    );
    let recorded = events.since(&summary.id, 0).await.unwrap();
    assert!(
        recorded
            .iter()
            .any(|event| event.kind == "interaction.cancel_requested")
    );
}

#[tokio::test]
async fn one_acp_session_is_ordered_within_a_lane() {
    let registry = Registry::new();
    registry.put(acp_spec("alpha", 80)).unwrap();
    let events = Arc::new(MemoryStore::new());
    let manager = Manager::new(registry, Executor::new(), events.clone());
    let (summary, _) = manager.create(&["alpha".into()]).await.unwrap();

    manager
        .submit(
            &summary.id,
            Author::claimed("alice"),
            "first",
            &["alpha".into()],
        )
        .await
        .unwrap();
    wait_running(&manager, &summary.id).await;
    manager
        .submit(
            &summary.id,
            Author::claimed("bob"),
            "second",
            &["alpha".into()],
        )
        .await
        .unwrap();

    let history = wait_history(&manager, &summary.id, 2).await;
    assert!(history.iter().all(|turn| turn.state == TurnState::Done));
    assert!(history[0].output.contains(":1:"), "{:?}", history[0].output);
    assert!(
        history[0].output.ends_with(":first"),
        "{:?}",
        history[0].output
    );
    assert!(history[1].output.contains(":2:"), "{:?}", history[1].output);
    assert!(
        history[1].output.ends_with(":second"),
        "{:?}",
        history[1].output
    );
    let view = manager.view(&summary.id).await.unwrap();
    let handle = view.summary.handles.get("alpha").unwrap();
    assert!(!handle.is_empty());

    let recorded = events.since(&summary.id, 0).await.unwrap();
    assert_eq!(
        recorded
            .iter()
            .filter(|event| event.kind == "session.opened")
            .count(),
        1
    );
}

#[tokio::test]
async fn separate_acp_lanes_run_in_parallel_and_keep_distinct_sessions() {
    let registry = Registry::new();
    registry.put(acp_spec("alpha", 300)).unwrap();
    registry.put(acp_spec("beta", 300)).unwrap();
    let manager = Manager::new(registry, Executor::new(), Arc::new(MemoryStore::new()));
    let (summary, _) = manager
        .create(&["alpha".into(), "beta".into()])
        .await
        .unwrap();

    manager
        .submit(
            &summary.id,
            Author::claimed("alice"),
            "parallel",
            &["alpha".into(), "beta".into()],
        )
        .await
        .unwrap();
    let history = wait_history(&manager, &summary.id, 2).await;
    assert_eq!(
        history
            .iter()
            .filter(|turn| turn.state == TurnState::Done)
            .count(),
        2
    );
    let intervals = history
        .iter()
        .map(|turn| {
            let fields = turn.output.split(':').collect::<Vec<_>>();
            let end = fields[fields.len() - 2].parse::<u128>().unwrap();
            let start = fields[fields.len() - 3].parse::<u128>().unwrap();
            (start, end)
        })
        .collect::<Vec<_>>();
    assert!(
        intervals.iter().map(|(start, _)| *start).max().unwrap()
            < intervals.iter().map(|(_, end)| *end).min().unwrap(),
        "ACP prompt intervals did not overlap: {intervals:?}"
    );

    let handles = manager.view(&summary.id).await.unwrap().summary.handles;
    assert_eq!(handles.len(), 2);
    assert_ne!(handles["alpha"], handles["beta"]);
}

#[tokio::test]
async fn cancellation_reaches_the_active_acp_lane() {
    let registry = Registry::new();
    registry.put(acp_spec("slow", 2_000)).unwrap();
    let manager = Manager::new(registry, Executor::new(), Arc::new(MemoryStore::new()));
    let (summary, _) = manager.create(&["slow".into()]).await.unwrap();
    manager
        .submit(
            &summary.id,
            Author::claimed("alice"),
            "wait",
            &["slow".into()],
        )
        .await
        .unwrap();
    wait_running(&manager, &summary.id).await;

    let started = Instant::now();
    manager.cancel(&summary.id, "alice", None).await.unwrap();
    let history = wait_history(&manager, &summary.id, 1).await;
    assert_eq!(history[0].state, TurnState::Cancelled);
    assert!(started.elapsed() < Duration::from_millis(600));
}

#[tokio::test]
async fn rehydrate_loads_the_persisted_acp_session_handle() {
    let registry = Registry::new();
    registry.put(acp_spec("alpha", 0)).unwrap();
    let events = Arc::new(MemoryStore::new());
    let original = Manager::new(registry.clone(), Executor::new(), events.clone());
    let (summary, _) = original.create(&["alpha".into()]).await.unwrap();
    original
        .submit(
            &summary.id,
            Author::claimed("alice"),
            "before",
            &["alpha".into()],
        )
        .await
        .unwrap();
    wait_history(&original, &summary.id, 1).await;
    let first_handle = original.view(&summary.id).await.unwrap().summary.handles["alpha"].clone();

    let restored = Manager::new(registry, Executor::new(), events);
    assert_eq!(restored.rehydrate().await.unwrap(), 1);
    assert_eq!(
        restored.view(&summary.id).await.unwrap().summary.handles["alpha"],
        first_handle
    );
    restored
        .submit(
            &summary.id,
            Author::claimed("bob"),
            "after",
            &["alpha".into()],
        )
        .await
        .unwrap();
    let history = wait_history(&restored, &summary.id, 2).await;
    assert!(history[1].output.contains(&first_handle));
    assert!(history[1].output.contains(":1:"));
    assert!(history[1].output.ends_with(":after"));
}

/// Two callers wanting the same lane at once must get the same subprocess.
///
/// They race whenever a room warms its lanes as it opens and the first message
/// arrives while that is still happening, and whenever two turns for one agent
/// start together. Losing it costs a stray process and splits the agent's turns
/// across two ACP sessions, so it quietly forgets everything it was told.
#[tokio::test]
async fn concurrent_callers_share_one_lane() {
    let spec = Arc::new(acp_spec("alpha", 20));
    let executor = Executor::new();
    let context = RenderContext {
        conversation: "s_race".into(),
        agent: "alpha".into(),
        ..Default::default()
    };

    let opened = futures_util::future::join_all((0..8).map(|_| {
        let executor = executor.clone();
        let spec = spec.clone();
        let context = context.clone();
        async move { executor.open(&spec, &context).await }
    }))
    .await;

    let sessions = opened
        .into_iter()
        .map(|result| result.expect("the lane opens").0)
        .collect::<std::collections::BTreeSet<_>>();
    assert_eq!(
        sessions.len(),
        1,
        "eight callers started {} lanes: {sessions:?}",
        sessions.len()
    );
}

/// Stopping one agent must leave the others working.
///
/// The lanes are independent, so cancelling the room to stop one of them throws
/// away whatever every other agent had in flight. In a room that exists to run
/// several at once, that is the common case rather than the rare one.
#[tokio::test]
async fn stopping_one_agent_leaves_the_others_running() {
    let registry = Registry::new();
    registry.put(acp_spec("alpha", 3_000)).unwrap();
    registry.put(acp_spec("beta-local", 3_000)).unwrap();
    let manager = Manager::new(registry, Executor::new(), Arc::new(MemoryStore::new()));
    let (summary, _) = manager
        .create(&["alpha".into(), "beta-local".into()])
        .await
        .unwrap();
    manager
        .submit(
            &summary.id,
            Author::claimed("alice"),
            "both of you start",
            &["alpha".into(), "beta-local".into()],
        )
        .await
        .unwrap();
    wait_running(&manager, &summary.id).await;

    // Written the way a person writes it: `beta` for `beta-local`.
    manager
        .cancel(&summary.id, "alice", Some("beta"))
        .await
        .unwrap();

    let history = wait_history(&manager, &summary.id, 2).await;
    let state = |agent: &str| {
        history
            .iter()
            .find(|turn| turn.agent == agent)
            .unwrap_or_else(|| panic!("{agent} has no turn in {history:?}"))
            .state
    };
    assert_eq!(state("beta-local"), TurnState::Cancelled, "{history:?}");
    assert_eq!(
        state("alpha"),
        TurnState::Done,
        "stopping one agent stopped the other: {history:?}"
    );
}

/// Naming an agent that is not in the room is a different mistake from naming
/// one that happens to be idle, and saying so is the difference between "fix
/// the name" and "there was nothing to stop".
#[tokio::test]
async fn stopping_an_agent_that_is_not_here_says_so() {
    let registry = Registry::new();
    registry.put(acp_spec("alpha", 2_000)).unwrap();
    let manager = Manager::new(registry, Executor::new(), Arc::new(MemoryStore::new()));
    let (summary, _) = manager.create(&["alpha".into()]).await.unwrap();
    manager
        .submit(
            &summary.id,
            Author::claimed("alice"),
            "wait",
            &["alpha".into()],
        )
        .await
        .unwrap();
    wait_running(&manager, &summary.id).await;

    let error = manager
        .cancel(&summary.id, "alice", Some("nobody"))
        .await
        .expect_err("an agent that is not in the room cannot be stopped");
    assert!(
        error.to_string().contains("nobody"),
        "the error should name it: {error}"
    );
    // And the agent that is here was left alone.
    let history = wait_history(&manager, &summary.id, 1).await;
    assert_eq!(history[0].state, TurnState::Done);
}

/// Express answers the request the agent actually asked, and records it.
///
/// The point of the recording is that this mode is reviewable afterwards. A
/// grant that leaves no trace is indistinguishable from an agent that never
/// asked, which is the property that would make it unusable on anything that
/// matters.
#[tokio::test]
async fn express_grants_permission_and_writes_it_down() {
    let registry = Registry::new();
    registry.put(permission_spec("guarded")).unwrap();
    let events = Arc::new(MemoryStore::new());
    let manager = Manager::configured(registry, Executor::new(), events.clone(), "", true);
    let (summary, _) = manager.create(&["guarded".into()]).await.unwrap();
    manager
        .submit(
            &summary.id,
            Author::claimed("alice"),
            "do the thing",
            &["guarded".into()],
        )
        .await
        .unwrap();

    // Nobody resolves anything: the turn has to finish on its own.
    let history = wait_history(&manager, &summary.id, 1).await;
    assert_eq!(history[0].state, TurnState::Done, "{history:?}");
    assert!(
        manager
            .pending_interactions(&summary.id)
            .await
            .unwrap()
            .is_empty()
    );

    let recorded = events.since(&summary.id, 0).await.unwrap();
    let resolved = recorded
        .iter()
        .find(|event| event.kind == "interaction.resolved")
        .expect("an express decision is still a recorded decision");
    assert_eq!(resolved.actor, "express", "{resolved:?}");
    let data = resolved.data.clone().unwrap_or_default();
    // The agent's own allow-once, not a licence invented for it.
    assert_eq!(data["option_id"], "allow-once", "{data}");
    assert!(
        recorded
            .iter()
            .any(|event| event.kind == "interaction.requested"),
        "the request itself must still be in the log"
    );
}

/// Without it, the same room waits for a person — the default has to stay the
/// safe one.
#[tokio::test]
async fn without_express_the_request_still_waits() {
    let registry = Registry::new();
    registry.put(permission_spec("guarded")).unwrap();
    let manager = Manager::new(registry, Executor::new(), Arc::new(MemoryStore::new()));
    let (summary, _) = manager.create(&["guarded".into()]).await.unwrap();
    manager
        .submit(
            &summary.id,
            Author::claimed("alice"),
            "do the thing",
            &["guarded".into()],
        )
        .await
        .unwrap();
    let pending = wait_interaction(&manager, &summary.id).await;
    assert_eq!(pending.agent, "guarded");
}

/// A room whose agent cannot start says so when it opens.
///
/// Otherwise it looks perfectly well until somebody speaks to it, and then
/// fails in a way that reads as the message being at fault — which is exactly
/// how an unset workspace variable presents.
#[tokio::test]
async fn a_lane_that_cannot_start_is_reported_into_the_room() {
    let registry = Registry::new();
    let mut broken = acp_spec("alpha", 0);
    // The shape of the real mistake: a template that renders to nothing.
    broken.acp.as_mut().unwrap().cwd = "{{env.ORYXA_NOT_SET_ANYWHERE}}".into();
    registry.put(broken).unwrap();
    let events = Arc::new(MemoryStore::new());
    let manager = Manager::new(registry, Executor::new(), events.clone());
    let (summary, _) = manager.create(&["alpha".into()]).await.unwrap();

    // Nobody says anything to it. The room should still report the problem.
    let mut reported = None;
    for _ in 0..300 {
        let recorded = events.since(&summary.id, 0).await.unwrap();
        if let Some(event) = recorded
            .iter()
            .find(|event| event.kind == "lane.unavailable")
        {
            reported = Some(event.clone());
            break;
        }
        sleep(Duration::from_millis(10)).await;
    }
    let reported = reported.expect("the room is told its agent could not start");
    assert_eq!(reported.actor, "alpha");
    let error = reported.data.unwrap_or_default()["error"].to_string();
    assert!(
        error.contains("workspace"),
        "the reason should name what is missing: {error}"
    );
}
