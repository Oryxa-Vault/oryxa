//! Small deterministic ACP agent used by the Rust integration tests.

use std::{
    collections::BTreeMap,
    sync::{
        Arc,
        atomic::{AtomicUsize, Ordering},
    },
    time::{Duration, SystemTime, UNIX_EPOCH},
};

use agent_client_protocol::schema::v1::{
    AgentCapabilities, CancelNotification, ContentBlock, ContentChunk, InitializeRequest,
    InitializeResponse, LoadSessionRequest, LoadSessionResponse, NewSessionRequest,
    NewSessionResponse, PermissionOption, PermissionOptionKind, PromptRequest, PromptResponse,
    RequestPermissionOutcome, RequestPermissionRequest, SessionNotification, SessionUpdate,
    StopReason, ToolCallUpdate, ToolCallUpdateFields,
};
use agent_client_protocol::{Agent, Result, Stdio};
use tokio::sync::Mutex;
use tokio_util::sync::CancellationToken;

#[tokio::main]
async fn main() -> Result<()> {
    let label = std::env::var("MOCK_ACP_AGENT_ID").unwrap_or_else(|_| "mock-acp".into());
    let delay = std::env::var("MOCK_ACP_DELAY_MS")
        .ok()
        .and_then(|value| value.parse::<u64>().ok())
        .unwrap_or(0);
    let require_permission =
        std::env::var("MOCK_ACP_REQUIRE_PERMISSION").is_ok_and(|value| value == "1");
    let counts = Arc::new(Mutex::new(BTreeMap::<String, usize>::new()));
    let active = Arc::new(Mutex::new(BTreeMap::<String, CancellationToken>::new()));
    let next_session = Arc::new(AtomicUsize::new(1));

    Agent
        .builder()
        .name("oryxa-mock-acp")
        .on_receive_request(
            async move |request: InitializeRequest, responder, _cx| {
                responder.respond(
                    InitializeResponse::new(request.protocol_version)
                        .agent_capabilities(AgentCapabilities::new().load_session(true)),
                )
            },
            agent_client_protocol::on_receive_request!(),
        )
        .on_receive_request(
            {
                let label = label.clone();
                let counts = counts.clone();
                let next_session = next_session.clone();
                async move |_request: NewSessionRequest, responder, _cx| {
                    let sequence = next_session.fetch_add(1, Ordering::SeqCst);
                    let session = format!("{label}-{}-{sequence}", std::process::id());
                    counts.lock().await.insert(session.clone(), 0);
                    responder.respond(NewSessionResponse::new(session))
                }
            },
            agent_client_protocol::on_receive_request!(),
        )
        .on_receive_request(
            {
                let counts = counts.clone();
                async move |request: LoadSessionRequest, responder, _cx| {
                    counts
                        .lock()
                        .await
                        .entry(request.session_id.to_string())
                        .or_insert(0);
                    responder.respond(LoadSessionResponse::new())
                }
            },
            agent_client_protocol::on_receive_request!(),
        )
        .on_receive_request(
            {
                let active = active.clone();
                let counts = counts.clone();
                let label = label.clone();
                async move |request: PromptRequest, responder, connection| {
                    let session_id = request.session_id.clone();
                    let input = request
                        .prompt
                        .iter()
                        .filter_map(|block| match block {
                            ContentBlock::Text(text) => Some(text.text.as_str()),
                            _ => None,
                        })
                        .collect::<Vec<_>>()
                        .join("");
                    let cancel = CancellationToken::new();
                    active
                        .lock()
                        .await
                        .insert(session_id.to_string(), cancel.clone());
                    let active = active.clone();
                    let counts = counts.clone();
                    let label = label.clone();
                    let prompt_connection = connection.clone();
                    connection.spawn(async move {
                        let started = epoch_millis();
                        if require_permission {
                            let permission = prompt_connection
                                .send_request(RequestPermissionRequest::new(
                                    session_id.clone(),
                                    ToolCallUpdate::new(
                                        "mock-write",
                                        ToolCallUpdateFields::new()
                                            .title("Write the generated patch")
                                            .raw_input(serde_json::json!({
                                                "path": "src/generated.rs",
                                                "operation": "write"
                                            })),
                                    ),
                                    vec![
                                        PermissionOption::new(
                                            "allow-once",
                                            "Allow once",
                                            PermissionOptionKind::AllowOnce,
                                        ),
                                        PermissionOption::new(
                                            "reject-once",
                                            "Reject",
                                            PermissionOptionKind::RejectOnce,
                                        ),
                                    ],
                                ))
                                .block_task()
                                .await?;
                            match permission.outcome {
                                RequestPermissionOutcome::Selected(selected)
                                    if selected.option_id.to_string() == "allow-once" => {}
                                _ => {
                                    active.lock().await.remove(&session_id.to_string());
                                    return responder.respond(PromptResponse::new(
                                        StopReason::Cancelled,
                                    ));
                                }
                            }
                        }
                        tokio::select! {
                            _ = tokio::time::sleep(Duration::from_millis(delay)) => {
                                let count = {
                                    let mut counts = counts.lock().await;
                                    let value = counts.entry(session_id.to_string()).or_insert(0);
                                    *value += 1;
                                    *value
                                };
                                let text = format!(
                                    "{label}:{}:{count}:{started}:{}:{input}",
                                    session_id,
                                    epoch_millis(),
                                );
                                prompt_connection.send_notification(SessionNotification::new(
                                    session_id.clone(),
                                    SessionUpdate::AgentMessageChunk(ContentChunk::new(text.into())),
                                ))?;
                                active.lock().await.remove(&session_id.to_string());
                                responder.respond(PromptResponse::new(StopReason::EndTurn))
                            }
                            _ = cancel.cancelled() => {
                                active.lock().await.remove(&session_id.to_string());
                                responder.respond(PromptResponse::new(StopReason::Cancelled))
                            }
                        }
                    })
                }
            },
            agent_client_protocol::on_receive_request!(),
        )
        .on_receive_notification(
            {
                let active = active.clone();
                async move |notification: CancelNotification, _cx| {
                    if let Some(cancel) = active.lock().await.get(&notification.session_id.to_string()) {
                        cancel.cancel();
                    }
                    Ok(())
                }
            },
            agent_client_protocol::on_receive_notification!(),
        )
        .connect_to(Stdio::new())
        .await
}

fn epoch_millis() -> u128 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap_or_default()
        .as_millis()
}
