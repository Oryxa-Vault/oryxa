use std::{
    collections::BTreeMap,
    sync::Arc,
    time::{Duration, Instant},
};

use oryxa::{
    connector::{AcpSpec, Executor, Origin, Registry, Spec, Step},
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

    manager.cancel(&summary.id, "alice").await.unwrap();
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
    manager.cancel(&summary.id, "alice").await.unwrap();
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
