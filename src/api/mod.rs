//! Axum translation layer for the stable `/v1` contract.

use std::{
    collections::BTreeMap,
    convert::Infallible,
    sync::{Arc, Mutex as StdMutex},
    time::{Duration, Instant},
};

use async_stream::stream;
use axum::{
    Json, Router,
    extract::{DefaultBodyLimit, FromRequest, Path, Query, Request, State},
    http::{HeaderMap, HeaderValue, StatusCode, header},
    middleware::{self, Next},
    response::{Html, IntoResponse, Response, Sse, sse::Event as SseEvent},
    routing::{delete, get, post},
};
use serde::{Deserialize, de::DeserializeOwned};
use serde_json::{Map, Value, json};
use subtle::ConstantTimeEq;

use crate::{
    connector::{Executor, Origin, Registry, Spec},
    events::{SYSTEM_STREAM, Store as EventStore, StoreError},
    session::{Author, Manager, SessionError},
    sharedctx::ContextError,
};

/// Fold durable API registrations over the file-backed registry. The API is
/// later in time, so it deliberately wins name collisions with connector
/// files. Invalid historical specs are skipped so one stale entry cannot stop
/// the server from starting after validation rules tighten.
pub async fn restore_agents(
    events: &dyn EventStore,
    registry: &Registry,
    origin: Origin,
) -> Result<usize, StoreError> {
    let mut restored = std::collections::BTreeSet::new();
    for event in events.since(SYSTEM_STREAM, 0).await? {
        match event.kind.as_str() {
            "agent.registered" => {
                let Some(value) = event.data.and_then(|data| data.get("spec").cloned()) else {
                    continue;
                };
                let Ok(mut spec) = serde_json::from_value::<Spec>(value) else {
                    continue;
                };
                spec.origin = origin;
                let name = spec.name.clone();
                if registry.put(spec).is_ok() {
                    restored.insert(name);
                }
            }
            "agent.removed" => {
                let Some(name) = event
                    .data
                    .and_then(|data| data.get("name").and_then(Value::as_str).map(str::to_owned))
                else {
                    continue;
                };
                registry.delete(&name);
                restored.remove(&name);
            }
            _ => {}
        }
    }
    Ok(restored.len())
}

const SESSION_HEADER: &str = "x-oryxa-session";
const ADMIN_HEADER: &str = "x-oryxa-admin";
const TOKEN_COOKIE: &str = "oryxa_token";

struct ApiJson<T>(T);

impl<T, S> FromRequest<S> for ApiJson<T>
where
    T: DeserializeOwned,
    S: Send + Sync,
{
    type Rejection = ApiError;

    async fn from_request(request: Request, state: &S) -> Result<Self, Self::Rejection> {
        Json::<T>::from_request(request, state)
            .await
            .map(|Json(value)| Self(value))
            .map_err(|error| ApiError::bad_request(error.body_text()))
    }
}

#[derive(Clone)]
pub struct AppState {
    pub registry: Registry,
    pub executor: Executor,
    pub manager: Arc<Manager>,
    pub events: Arc<dyn EventStore>,
    pub token: String,
    pub admin_token: String,
    pub trust_header: String,
    pub allow_private_agents: bool,
    room_turns: Option<Limiter>,
    all_turns: Option<Limiter>,
}

impl AppState {
    pub fn new(
        registry: Registry,
        executor: Executor,
        manager: Arc<Manager>,
        events: Arc<dyn EventStore>,
    ) -> Self {
        Self {
            registry,
            executor,
            manager,
            events,
            token: String::new(),
            admin_token: String::new(),
            trust_header: String::new(),
            allow_private_agents: false,
            room_turns: None,
            all_turns: None,
        }
    }

    pub fn with_turn_limits(mut self, per_room: i32, total: i32) -> Self {
        self.room_turns = Limiter::new(per_room);
        self.all_turns = Limiter::new(total);
        self
    }

    pub fn with_private_agents(mut self, allow: bool) -> Self {
        self.allow_private_agents = allow;
        self
    }

    fn admit(&self, room: &str) -> Result<(), ApiError> {
        if let Some(limiter) = &self.room_turns
            && let Err(wait) = limiter.allow(room)
        {
            return Err(ApiError::too_many(
                wait,
                "this room has started too many turns; it is limited so one room cannot spend the whole server budget",
            ));
        }
        if let Some(limiter) = &self.all_turns
            && let Err(wait) = limiter.allow("")
        {
            return Err(ApiError::too_many(
                wait,
                "the server has started too many turns",
            ));
        }
        Ok(())
    }

    fn charge(&self, room: &str, turns: usize) {
        if let Some(limiter) = &self.room_turns {
            limiter.charge(room, turns);
        }
        if let Some(limiter) = &self.all_turns {
            limiter.charge("", turns);
        }
    }
}

#[derive(Clone)]
struct Limiter {
    buckets: Arc<StdMutex<BTreeMap<String, Bucket>>>,
    per_second: f64,
    burst: f64,
}

struct Bucket {
    tokens: f64,
    last: Instant,
}

impl Limiter {
    fn new(per_minute: i32) -> Option<Self> {
        (per_minute > 0).then(|| Self {
            buckets: Arc::new(StdMutex::new(BTreeMap::new())),
            per_second: f64::from(per_minute) / 60.0,
            burst: f64::from(per_minute),
        })
    }

    fn allow(&self, key: &str) -> Result<(), Duration> {
        let mut buckets = self.buckets.lock().expect("turn limiter poisoned");
        let bucket = self.refill(&mut buckets, key);
        if bucket.tokens >= 1.0 {
            return Ok(());
        }
        let seconds = ((1.0 - bucket.tokens) / self.per_second).ceil().max(1.0);
        Err(Duration::from_secs_f64(seconds))
    }

    fn charge(&self, key: &str, turns: usize) {
        if turns == 0 {
            return;
        }
        let mut buckets = self.buckets.lock().expect("turn limiter poisoned");
        self.refill(&mut buckets, key).tokens -= turns as f64;
    }

    fn refill<'a>(&self, buckets: &'a mut BTreeMap<String, Bucket>, key: &str) -> &'a mut Bucket {
        let now = Instant::now();
        if !buckets.contains_key(key) && buckets.len() >= 1_024 {
            let idle = Duration::from_secs_f64(2.0 * self.burst / self.per_second);
            buckets.retain(|_, bucket| now.duration_since(bucket.last) <= idle);
        }
        let bucket = buckets.entry(key.to_owned()).or_insert(Bucket {
            tokens: self.burst,
            last: now,
        });
        let elapsed = now.duration_since(bucket.last).as_secs_f64();
        bucket.tokens = (bucket.tokens + elapsed * self.per_second).min(self.burst);
        bucket.last = now;
        bucket
    }
}

pub fn router(state: AppState) -> Router {
    Router::new()
        .route("/health", get(health))
        .route("/v1/agents", post(register_agent).get(list_agents))
        .route("/v1/agents/{name}", get(get_agent).delete(delete_agent))
        .route("/v1/agents/{name}/check", post(check_agent))
        .route("/v1/sessions", post(create_session).get(list_sessions))
        .route("/v1/sessions/{id}", get(get_session))
        .route("/v1/sessions/{id}/join", post(join_room))
        .route("/v1/sessions/{id}/keys", post(issue_key))
        .route("/v1/sessions/{id}/input", post(submit_input))
        .route("/v1/sessions/{id}/input/{input}", delete(withdraw_input))
        .route("/v1/sessions/{id}/cancel", post(cancel_turn))
        .route("/v1/sessions/{id}/interactions", get(list_interactions))
        .route(
            "/v1/sessions/{id}/interactions/{interaction}/resolve",
            post(resolve_interaction),
        )
        .route("/v1/sessions/{id}/close", post(close_session))
        .route("/v1/sessions/{id}/context", get(get_context))
        .route("/v1/sessions/{id}/context/{key}", post(write_context))
        .route("/v1/sessions/{id}/context/{key}/pin", post(pin_context))
        .route("/v1/sessions/{id}/events", get(get_events))
        .route("/v1/sessions/{id}/stream", get(event_stream))
        .route("/v1/auth/status", get(auth_status))
        .route("/v1/auth/login", post(login))
        .fallback(get(viewer))
        .layer(DefaultBodyLimit::max(1 << 20))
        .layer(middleware::from_fn_with_state(state.clone(), require_auth))
        .with_state(state)
}

async fn health() -> Json<Value> {
    Json(json!({"status": "ok"}))
}

async fn viewer() -> Html<&'static str> {
    Html(include_str!("../../web/index.html"))
}

async fn require_auth(State(state): State<AppState>, request: Request, next: Next) -> Response {
    let path = request.uri().path();
    if state.token.is_empty()
        || !path.starts_with("/v1/")
        || matches!(path, "/v1/auth/login" | "/v1/auth/status")
        || token_from_headers(request.headers())
            .is_some_and(|given| secret_matches(&given, &state.token))
    {
        return next.run(request).await;
    }
    let mut response = ApiError::new(StatusCode::UNAUTHORIZED, "unauthorized").into_response();
    response.headers_mut().insert(
        header::WWW_AUTHENTICATE,
        HeaderValue::from_static("Bearer realm=\"oryxa\""),
    );
    response
}

fn token_from_headers(headers: &HeaderMap) -> Option<String> {
    bearer(headers).or_else(|| cookie(headers, TOKEN_COOKIE))
}

fn bearer(headers: &HeaderMap) -> Option<String> {
    let value = headers.get(header::AUTHORIZATION)?.to_str().ok()?;
    let (scheme, token) = value.split_once(' ')?;
    scheme
        .eq_ignore_ascii_case("bearer")
        .then(|| token.to_owned())
}

fn secret_matches(given: &str, expected: &str) -> bool {
    given.as_bytes().ct_eq(expected.as_bytes()).into()
}

fn cookie(headers: &HeaderMap, name: &str) -> Option<String> {
    headers
        .get(header::COOKIE)?
        .to_str()
        .ok()?
        .split(';')
        .filter_map(|part| part.trim().split_once('='))
        .find_map(|(key, value)| (key == name).then(|| value.to_owned()))
}

async fn require_room(state: &AppState, headers: &HeaderMap, id: &str) -> Result<String, ApiError> {
    let secret = headers
        .get(SESSION_HEADER)
        .and_then(|value| value.to_str().ok())
        .map(str::trim)
        .filter(|value| !value.is_empty())
        .map(str::to_owned)
        .or_else(|| cookie(headers, &format!("oryxa_room_{id}")))
        .unwrap_or_default();
    state.manager.resolve(id, &secret).await.ok_or_else(|| {
        ApiError::new(
            StatusCode::NOT_FOUND,
            "no such session, or the wrong session secret. Pass it as X-Oryxa-Session, or sign in to the room in the viewer",
        )
    })
}

fn identify(
    state: &AppState,
    headers: &HeaderMap,
    claimed: &str,
    bound: &str,
) -> Result<Author, ApiError> {
    if !state.trust_header.is_empty() {
        let name = headers
            .get(&state.trust_header)
            .and_then(|value| value.to_str().ok())
            .map(str::trim)
            .filter(|value| !value.is_empty())
            .ok_or_else(|| {
                ApiError::new(
                    StatusCode::UNAUTHORIZED,
                    format!("missing {}", state.trust_header),
                )
            })?;
        return Ok(Author {
            name: name.into(),
            source: "trusted".into(),
        });
    }
    if !bound.is_empty() {
        return Ok(Author {
            name: bound.into(),
            source: "key".into(),
        });
    }
    Ok(Author {
        name: if claimed.trim().is_empty() {
            "anonymous".into()
        } else {
            claimed.trim().into()
        },
        source: "claimed".into(),
    })
}

async fn list_agents(State(state): State<AppState>) -> Json<Value> {
    let agents = state
        .registry
        .list()
        .into_iter()
        .map(|spec| (*spec).clone())
        .collect::<Vec<_>>();
    Json(json!({"agents": agents}))
}

async fn get_agent(
    State(state): State<AppState>,
    Path(name): Path<String>,
) -> Result<Json<Spec>, ApiError> {
    state
        .registry
        .get(&name)
        .map(|spec| Json((*spec).clone()))
        .ok_or_else(|| ApiError::not_found("agent not found"))
}

async fn register_agent(
    State(state): State<AppState>,
    headers: HeaderMap,
    body: axum::body::Bytes,
) -> Result<(StatusCode, Json<Spec>), ApiError> {
    require_admin(&state, &headers)?;
    let mut spec = if headers
        .get(header::CONTENT_TYPE)
        .and_then(|value| value.to_str().ok())
        .is_some_and(|value| value.contains("yaml") || value.contains("yml"))
    {
        Spec::from_yaml(&body)
    } else {
        Spec::from_json(&body)
    }
    .map_err(|error| ApiError::bad_request(error.to_string()))?;
    spec.origin = if state.allow_private_agents {
        Origin::ApiPrivate
    } else {
        Origin::Api
    };
    state
        .registry
        .put(spec.clone())
        .map_err(|error| ApiError::bad_request(error.to_string()))?;
    if let Err(error) = state
        .events
        .append(
            crate::events::SYSTEM_STREAM,
            "agent.registered",
            &identify(&state, &headers, "", "")
                .map_or_else(|_| String::new(), |author| author.name),
            "",
            Some(json!({"spec": spec})),
        )
        .await
    {
        eprintln!("oryxa: could not persist agent {:?}: {error}", spec.name);
    }
    Ok((StatusCode::CREATED, Json(spec)))
}

async fn delete_agent(
    State(state): State<AppState>,
    Path(name): Path<String>,
    Query(query): Query<DeleteAgentQuery>,
    headers: HeaderMap,
) -> Result<StatusCode, ApiError> {
    require_admin(&state, &headers)?;
    if state.registry.get(&name).is_none() {
        return Err(ApiError::not_found("agent not found"));
    }
    let rooms = state.manager.used_by(&name).await;
    if !query.force && !rooms.is_empty() {
        let mut error = ApiError::new(
            StatusCode::CONFLICT,
            format!(
                "{name:?} is in {} open room(s); close them first, or repeat with ?force=true",
                rooms.len()
            ),
        );
        error.extra.insert("sessions".into(), json!(rooms));
        return Err(error);
    }
    if !state.registry.delete(&name) {
        return Err(ApiError::not_found("agent not found"));
    }
    if let Err(error) = state
        .events
        .append(
            crate::events::SYSTEM_STREAM,
            "agent.removed",
            &identify(&state, &headers, "", "")
                .map_or_else(|_| String::new(), |author| author.name),
            "",
            Some(json!({"name": name})),
        )
        .await
    {
        eprintln!("oryxa: could not persist removal of agent {name:?}: {error}");
    }
    Ok(StatusCode::NO_CONTENT)
}

#[derive(Default, Deserialize)]
struct DeleteAgentQuery {
    #[serde(default)]
    force: bool,
}

fn require_admin(state: &AppState, headers: &HeaderMap) -> Result<(), ApiError> {
    if state.admin_token.is_empty()
        || bearer(headers).is_some_and(|given| secret_matches(&given, &state.admin_token))
        || headers
            .get(ADMIN_HEADER)
            .and_then(|value| value.to_str().ok())
            .is_some_and(|given| secret_matches(given, &state.admin_token))
    {
        Ok(())
    } else {
        Err(ApiError::new(
            StatusCode::FORBIDDEN,
            "changing the agent registry needs the admin token, sent as X-Oryxa-Admin",
        ))
    }
}

#[derive(Deserialize)]
struct Probe {
    #[serde(default)]
    probe: String,
}

async fn check_agent(
    State(state): State<AppState>,
    Path(name): Path<String>,
    probe: Option<Json<Probe>>,
) -> Result<Response, ApiError> {
    let spec = state
        .registry
        .get(&name)
        .ok_or_else(|| ApiError::not_found("agent not found"))?;
    let probe = probe
        .map(|Json(value)| value.probe)
        .filter(|value| !value.is_empty())
        .unwrap_or_else(|| "ping from oryxa check".into());
    let result = state.executor.check(&spec, &probe).await;
    let status = if result.ok {
        StatusCode::OK
    } else {
        StatusCode::BAD_GATEWAY
    };
    Ok((status, Json(result)).into_response())
}

#[derive(Deserialize)]
struct CreateSession {
    #[serde(default)]
    agent: String,
    #[serde(default)]
    agents: Vec<String>,
    #[serde(default)]
    workspace: String,
}

async fn create_session(
    State(state): State<AppState>,
    ApiJson(mut request): ApiJson<CreateSession>,
) -> Result<Response, ApiError> {
    if !request.agent.is_empty() {
        request.agents.insert(0, request.agent);
    }
    let requested_workspace = if request.workspace.trim().is_empty() {
        std::env::var("ORYXA_WORKSPACE")
            .ok()
            .filter(|value| !value.trim().is_empty())
            .or_else(|| {
                std::env::current_dir()
                    .ok()
                    .map(|path| path.to_string_lossy().into_owned())
            })
            .unwrap_or_default()
    } else {
        request.workspace
    };
    let workspace = std::fs::canonicalize(&requested_workspace)
        .map_err(|_| ApiError::bad_request("workspace must be an existing local directory"))?;
    if !workspace.is_dir() {
        return Err(ApiError::bad_request(
            "workspace must be an existing local directory",
        ));
    }
    let workspace = workspace.to_string_lossy().into_owned();
    let (summary, secret) = state
        .manager
        .create_scoped(&request.agents, &workspace)
        .await
        .map_err(|error| match error {
            SessionError::NoAgent(_) => ApiError::bad_request(error.to_string()),
            error => ApiError::session(error),
        })?;
    let mut object = serde_json::to_value(summary)
        .expect("summary serializes")
        .as_object()
        .cloned()
        .unwrap_or_default();
    object.insert("secret".into(), Value::String(secret.clone()));
    let cookie = room_cookie(object["id"].as_str().unwrap_or_default(), &secret);
    let mut response = (StatusCode::CREATED, Json(Value::Object(object))).into_response();
    response.headers_mut().append(
        header::SET_COOKIE,
        HeaderValue::from_str(&cookie).expect("cookie"),
    );
    Ok(response)
}

async fn list_sessions(State(state): State<AppState>) -> Json<Value> {
    Json(json!({"sessions": state.manager.list().await}))
}

async fn get_session(
    State(state): State<AppState>,
    Path(id): Path<String>,
    headers: HeaderMap,
) -> Result<Json<crate::session::View>, ApiError> {
    require_room(&state, &headers, &id).await?;
    Ok(Json(
        state.manager.view(&id).await.map_err(ApiError::session)?,
    ))
}

#[derive(Deserialize)]
struct JoinRoom {
    #[serde(default)]
    secret: String,
}

async fn join_room(
    State(state): State<AppState>,
    Path(id): Path<String>,
    headers: HeaderMap,
    request: Option<Json<JoinRoom>>,
) -> Result<Response, ApiError> {
    let requested = request.map(|Json(value)| value.secret).unwrap_or_default();
    let secret: String = if requested.trim().is_empty() {
        headers
            .get(SESSION_HEADER)
            .and_then(|value| value.to_str().ok())
            .map(str::trim)
            .filter(|value| !value.is_empty())
            .map(str::to_owned)
            .or_else(|| cookie(&headers, &format!("oryxa_room_{id}")))
            .unwrap_or_default()
    } else {
        requested.trim().into()
    };
    let author = state
        .manager
        .resolve(&id, &secret)
        .await
        .ok_or_else(|| ApiError::not_found("no such session, or the wrong session secret"))?;
    let mut response = Json(json!({"id": id, "joined": true, "author": author})).into_response();
    response.headers_mut().append(
        header::SET_COOKIE,
        HeaderValue::from_str(&room_cookie(&id, &secret)).expect("cookie"),
    );
    Ok(response)
}

#[derive(Deserialize)]
struct IssueKey {
    author: String,
}

async fn issue_key(
    State(state): State<AppState>,
    Path(id): Path<String>,
    headers: HeaderMap,
    ApiJson(request): ApiJson<IssueKey>,
) -> Result<(StatusCode, Json<Value>), ApiError> {
    let bound = require_room(&state, &headers, &id).await?;
    let who = identify(&state, &headers, "", &bound)?;
    let (key, hash) = state
        .manager
        .issue_key(&id, &request.author)
        .await
        .map_err(ApiError::session)?;
    state
        .manager
        .record_invite(&id, &who.name, request.author.trim(), &hash)
        .await
        .map_err(ApiError::session)?;
    Ok((
        StatusCode::CREATED,
        Json(json!({"session": id, "author": request.author.trim(), "key": key})),
    ))
}

#[derive(Deserialize)]
struct SubmitInput {
    text: String,
    #[serde(default)]
    author: String,
    #[serde(default)]
    to: Vec<String>,
}

async fn submit_input(
    State(state): State<AppState>,
    Path(id): Path<String>,
    headers: HeaderMap,
    ApiJson(request): ApiJson<SubmitInput>,
) -> Result<(StatusCode, Json<Value>), ApiError> {
    if request.text.trim().is_empty() {
        return Err(ApiError::bad_request("text is required"));
    }
    let bound = require_room(&state, &headers, &id).await?;
    let who = identify(&state, &headers, &request.author, &bound)?;
    state.admit(&id)?;
    let input = state
        .manager
        .submit(&id, who, &request.text, &request.to)
        .await
        .map_err(ApiError::session)?;
    state.charge(&id, input.wake.len());
    Ok((
        StatusCode::ACCEPTED,
        Json(json!({
            "id": input.id, "author": input.author, "text": input.text, "seq": input.seq,
            "state": "queued", "group": input.id, "wake": input.wake, "why": input.why,
        })),
    ))
}

async fn withdraw_input(
    State(state): State<AppState>,
    Path((id, input)): Path<(String, String)>,
    Query(query): Query<ActorQuery>,
    headers: HeaderMap,
) -> Result<StatusCode, ApiError> {
    let bound = require_room(&state, &headers, &id).await?;
    let who = identify(&state, &headers, &query.author, &bound)?;
    state
        .manager
        .withdraw(&id, &input, &who.name)
        .await
        .map_err(ApiError::session)?;
    Ok(StatusCode::NO_CONTENT)
}

async fn cancel_turn(
    State(state): State<AppState>,
    Path(id): Path<String>,
    Query(query): Query<ActorQuery>,
    headers: HeaderMap,
) -> Result<StatusCode, ApiError> {
    let bound = require_room(&state, &headers, &id).await?;
    let who = identify(&state, &headers, &query.author, &bound)?;
    let agent = query.agent.trim();
    state
        .manager
        .cancel(&id, &who.name, (!agent.is_empty()).then_some(agent))
        .await
        .map_err(ApiError::session)?;
    Ok(StatusCode::ACCEPTED)
}

async fn list_interactions(
    State(state): State<AppState>,
    Path(id): Path<String>,
    headers: HeaderMap,
) -> Result<Json<Value>, ApiError> {
    require_room(&state, &headers, &id).await?;
    let interactions = state
        .manager
        .pending_interactions(&id)
        .await
        .map_err(ApiError::session)?;
    Ok(Json(json!({"interactions": interactions})))
}

#[derive(Default, Deserialize)]
struct ResolveInteractionRequest {
    #[serde(default)]
    author: String,
    #[serde(default)]
    option_id: String,
    #[serde(default)]
    cancel: bool,
}

async fn resolve_interaction(
    State(state): State<AppState>,
    Path((id, interaction)): Path<(String, String)>,
    headers: HeaderMap,
    ApiJson(request): ApiJson<ResolveInteractionRequest>,
) -> Result<Json<Value>, ApiError> {
    let bound = require_room(&state, &headers, &id).await?;
    let who = identify(&state, &headers, &request.author, &bound)?;
    let option_empty = request.option_id.trim().is_empty();
    if (request.cancel && !option_empty) || (!request.cancel && option_empty) {
        return Err(ApiError::bad_request(
            "provide exactly one of option_id or cancel=true",
        ));
    }
    let option_id = (!request.cancel).then_some(request.option_id.as_str());
    let resolved = state
        .manager
        .resolve_interaction(&id, &interaction, &who.name, option_id)
        .await
        .map_err(ApiError::session)?;
    Ok(Json(json!({
        "interaction": resolved,
        "outcome": if request.cancel { "cancelled" } else { "selected" },
        "option_id": option_id,
    })))
}

async fn close_session(
    State(state): State<AppState>,
    Path(id): Path<String>,
    Query(query): Query<ActorQuery>,
    headers: HeaderMap,
) -> Result<StatusCode, ApiError> {
    let bound = require_room(&state, &headers, &id).await?;
    let who = identify(&state, &headers, &query.author, &bound)?;
    state
        .manager
        .close(&id, &who.name)
        .await
        .map_err(ApiError::session)?;
    Ok(StatusCode::NO_CONTENT)
}

#[derive(Default, Deserialize)]
struct ActorQuery {
    #[serde(default)]
    author: String,
    /// Stop only this agent. Absent stops every running turn in the room.
    #[serde(default)]
    agent: String,
}

async fn get_context(
    State(state): State<AppState>,
    Path(id): Path<String>,
    headers: HeaderMap,
) -> Result<Json<Value>, ApiError> {
    require_room(&state, &headers, &id).await?;
    Ok(Json(
        json!({"context": state.manager.context(&id).await.map_err(ApiError::session)?}),
    ))
}

#[derive(Deserialize)]
struct ContextWrite {
    #[serde(default)]
    append: String,
    value: Option<String>,
    #[serde(default)]
    author: String,
}

async fn write_context(
    State(state): State<AppState>,
    Path((id, key)): Path<(String, String)>,
    headers: HeaderMap,
    ApiJson(request): ApiJson<ContextWrite>,
) -> Result<Json<crate::sharedctx::Entry>, ApiError> {
    let bound = require_room(&state, &headers, &id).await?;
    let who = identify(&state, &headers, &request.author, &bound)?;
    let entry = if let Some(value) = request.value {
        let if_match = headers
            .get(header::IF_MATCH)
            .and_then(|value| value.to_str().ok())
            .map(|value| value.trim_matches('"').parse::<i64>())
            .transpose()
            .map_err(|_| ApiError::bad_request("If-Match must be a version number"))?
            .unwrap_or(-1);
        state
            .manager
            .set_context(&id, &key, &who.name, &value, if_match)
            .await
            .map_err(ApiError::session)?
    } else {
        if request.append.trim().is_empty() {
            return Err(ApiError::bad_request("send either append or value"));
        }
        state
            .manager
            .append_context(&id, &key, &who.name, &request.append)
            .await
            .map_err(ApiError::session)?
    };
    Ok(Json(entry))
}

#[derive(Deserialize)]
struct PinContext {
    pinned: Option<bool>,
    #[serde(default)]
    author: String,
}

async fn pin_context(
    State(state): State<AppState>,
    Path((id, key)): Path<(String, String)>,
    headers: HeaderMap,
    request: Option<Json<PinContext>>,
) -> Result<Json<crate::sharedctx::Entry>, ApiError> {
    let bound = require_room(&state, &headers, &id).await?;
    let request = request.map(|Json(value)| value).unwrap_or(PinContext {
        pinned: None,
        author: String::new(),
    });
    let who = identify(&state, &headers, &request.author, &bound)?;
    Ok(Json(
        state
            .manager
            .pin_context(&id, &key, &who.name, request.pinned.unwrap_or(true))
            .await
            .map_err(ApiError::session)?,
    ))
}

#[derive(Deserialize)]
struct Since {
    #[serde(default)]
    since: i64,
}

async fn get_events(
    State(state): State<AppState>,
    Path(id): Path<String>,
    Query(query): Query<Since>,
    headers: HeaderMap,
) -> Result<Json<Value>, ApiError> {
    require_room(&state, &headers, &id).await?;
    let since = since_value(&headers, query.since);
    Ok(Json(
        json!({"events": state.events.since(&id, since).await.map_err(ApiError::internal)?}),
    ))
}

async fn event_stream(
    State(state): State<AppState>,
    Path(id): Path<String>,
    Query(query): Query<Since>,
    headers: HeaderMap,
) -> Result<Response, ApiError> {
    require_room(&state, &headers, &id).await?;
    let since = since_value(&headers, query.since);
    // Subscribe first, then backfill. Reversing these two operations can lose
    // an event that lands after the read and before the subscription exists.
    let mut receiver = state.events.subscribe();
    let backfill = state
        .events
        .since(&id, since)
        .await
        .map_err(ApiError::internal)?;
    let output = stream! {
        let mut last = since;
        for event in backfill {
            last = event.seq;
            yield Ok::<_, Infallible>(sse_event(&event));
        }
        loop {
            match receiver.recv().await {
                Ok(event) if event.session == id && event.seq > last => {
                    last = event.seq;
                    yield Ok(sse_event(&event));
                }
                Ok(_) => {}
                Err(tokio::sync::broadcast::error::RecvError::Lagged(_)) => {
                    if let Ok(events) = state.events.since(&id, last).await {
                        for event in events {
                            last = event.seq;
                            yield Ok(sse_event(&event));
                        }
                    }
                }
                Err(tokio::sync::broadcast::error::RecvError::Closed) => break,
            }
        }
    };
    let mut response = Sse::new(output)
        .keep_alive(axum::response::sse::KeepAlive::new().interval(Duration::from_secs(20)))
        .into_response();
    response
        .headers_mut()
        .insert(header::CACHE_CONTROL, HeaderValue::from_static("no-cache"));
    response
        .headers_mut()
        .insert("x-accel-buffering", HeaderValue::from_static("no"));
    Ok(response)
}

fn since_value(headers: &HeaderMap, query: i64) -> i64 {
    headers
        .get("last-event-id")
        .and_then(|value| value.to_str().ok())
        .and_then(|value| value.parse().ok())
        .unwrap_or(query)
}

fn sse_event(event: &crate::events::Event) -> SseEvent {
    SseEvent::default()
        .id(event.seq.to_string())
        .json_data(event)
        .expect("event serializes")
}

async fn auth_status(State(state): State<AppState>, headers: HeaderMap) -> Json<Value> {
    let mode = if state.token.is_empty() {
        "open"
    } else {
        "token"
    };
    let authed = state.token.is_empty()
        || token_from_headers(&headers).is_some_and(|given| secret_matches(&given, &state.token));
    let mut output = json!({
        "mode": mode,
        "authed": authed,
        "identity": if state.trust_header.is_empty() { "claimed" } else { "trusted" },
    });
    if !state.trust_header.is_empty()
        && let Some(author) = headers
            .get(&state.trust_header)
            .and_then(|value| value.to_str().ok())
            .map(str::trim)
            .filter(|value| !value.is_empty())
    {
        output["author"] = Value::String(author.into());
    }
    Json(output)
}

#[derive(Deserialize)]
struct Login {
    #[serde(default)]
    token: String,
}

async fn login(
    State(state): State<AppState>,
    ApiJson(request): ApiJson<Login>,
) -> Result<Response, ApiError> {
    if !state.token.is_empty() && !secret_matches(&request.token, &state.token) {
        return Err(ApiError::new(StatusCode::UNAUTHORIZED, "invalid token"));
    }
    let mut response =
        Json(json!({"ok": true, "auth": if state.token.is_empty() { "open" } else { "token" }}))
            .into_response();
    if !state.token.is_empty() {
        response.headers_mut().append(
            header::SET_COOKIE,
            HeaderValue::from_str(&format!(
                "{TOKEN_COOKIE}={}; Path=/; HttpOnly; SameSite=Strict",
                request.token
            ))
            .expect("cookie"),
        );
    }
    Ok(response)
}

fn room_cookie(id: &str, secret: &str) -> String {
    format!("oryxa_room_{id}={secret}; Path=/v1/sessions/{id}; HttpOnly; SameSite=Strict")
}

struct ApiError {
    status: StatusCode,
    message: String,
    extra: Map<String, Value>,
    retry_after: Option<u64>,
}

impl ApiError {
    fn new(status: StatusCode, message: impl Into<String>) -> Self {
        Self {
            status,
            message: message.into(),
            extra: Map::new(),
            retry_after: None,
        }
    }
    fn bad_request(message: impl Into<String>) -> Self {
        Self::new(StatusCode::BAD_REQUEST, message)
    }
    fn not_found(message: impl Into<String>) -> Self {
        Self::new(StatusCode::NOT_FOUND, message)
    }
    fn internal(error: impl std::fmt::Display) -> Self {
        Self::new(StatusCode::INTERNAL_SERVER_ERROR, error.to_string())
    }
    fn too_many(wait: Duration, message: impl Into<String>) -> Self {
        let seconds = wait.as_secs().max(1);
        let mut error = Self::new(StatusCode::TOO_MANY_REQUESTS, message);
        error
            .extra
            .insert("retry_after".into(), Value::from(seconds));
        error.retry_after = Some(seconds);
        error
    }
    fn session(error: SessionError) -> Self {
        match error {
            SessionError::NoSession | SessionError::NoAgent(_) => {
                Self::new(StatusCode::NOT_FOUND, error.to_string())
            }
            SessionError::Context(ContextError::Conflict {
                key,
                current,
                version,
                by,
            }) => {
                let mut error = Self::new(
                    StatusCode::CONFLICT,
                    format!("stale write to {key:?}: current version is {version}"),
                );
                error.extra.insert("key".into(), Value::String(key));
                error.extra.insert("current".into(), Value::String(current));
                error.extra.insert("version".into(), Value::from(version));
                error.extra.insert("by".into(), Value::String(by));
                error
            }
            error @ (SessionError::Closed | SessionError::Context(ContextError::WrongKind(_))) => {
                Self::new(StatusCode::CONFLICT, error.to_string())
            }
            SessionError::Context(ContextError::NotFound(_)) => {
                Self::new(StatusCode::NOT_FOUND, error.to_string())
            }
            SessionError::NoInteraction => Self::new(StatusCode::NOT_FOUND, error.to_string()),
            SessionError::NoTurn | SessionError::InvalidInteraction(_) => {
                Self::new(StatusCode::BAD_REQUEST, error.to_string())
            }
            SessionError::Store(_) => Self::internal(error),
        }
    }
}

impl IntoResponse for ApiError {
    fn into_response(self) -> Response {
        let mut body = self.extra;
        body.insert("error".into(), Value::String(self.message));
        let mut response = (self.status, Json(Value::Object(body))).into_response();
        if let Some(seconds) = self.retry_after {
            response.headers_mut().insert(
                header::RETRY_AFTER,
                HeaderValue::from_str(&seconds.to_string()).expect("retry-after is numeric"),
            );
        }
        response
    }
}

#[cfg(test)]
mod tests {
    use std::collections::BTreeMap;

    use axum::{body::Body, http::Request};
    use http_body_util::BodyExt;
    use tower::ServiceExt;

    use crate::{
        connector::{ResponseSpec, Step},
        events::MemoryStore,
    };

    use super::*;

    fn app() -> Router {
        let registry = Registry::new();
        registry
            .put(Spec {
                name: "a".into(),
                base: "http://127.0.0.1:1".into(),
                acp: None,
                headers: BTreeMap::new(),
                vars: BTreeMap::new(),
                capabilities: Vec::new(),
                interests: Vec::new(),
                timeout: String::new(),
                open: None,
                turn: Step {
                    response: Some(ResponseSpec::default()),
                    ..Default::default()
                },
                context: Vec::new(),
                origin: Origin::File,
            })
            .unwrap();
        let events: Arc<dyn EventStore> = Arc::new(MemoryStore::new());
        let executor = Executor::new();
        let manager = Manager::new(registry.clone(), executor.clone(), events.clone());
        router(AppState::new(registry, executor, manager, events))
    }

    async fn create_room(app: &Router) -> (String, String) {
        let response = app
            .clone()
            .oneshot(
                Request::post("/v1/sessions")
                    .header("content-type", "application/json")
                    .body(Body::from(r#"{"agents":["a"]}"#))
                    .unwrap(),
            )
            .await
            .unwrap();
        assert_eq!(response.status(), StatusCode::CREATED);
        let body = json_body(response).await;
        (
            body["id"].as_str().unwrap().into(),
            body["secret"].as_str().unwrap().into(),
        )
    }

    async fn json_body(response: Response) -> Value {
        serde_json::from_slice(&response.into_body().collect().await.unwrap().to_bytes()).unwrap()
    }

    #[tokio::test]
    async fn create_requires_the_room_secret_afterwards() {
        let app = app();
        let response = app
            .clone()
            .oneshot(
                Request::post("/v1/sessions")
                    .header("content-type", "application/json")
                    .body(Body::from(r#"{"agents":["a"]}"#))
                    .unwrap(),
            )
            .await
            .unwrap();
        assert_eq!(response.status(), StatusCode::CREATED);
        let body: Value =
            serde_json::from_slice(&response.into_body().collect().await.unwrap().to_bytes())
                .unwrap();
        let id = body["id"].as_str().unwrap();
        let secret = body["secret"].as_str().unwrap();

        let denied = app
            .clone()
            .oneshot(
                Request::get(format!("/v1/sessions/{id}"))
                    .body(Body::empty())
                    .unwrap(),
            )
            .await
            .unwrap();
        assert_eq!(denied.status(), StatusCode::NOT_FOUND);
        let admitted = app
            .oneshot(
                Request::get(format!("/v1/sessions/{id}"))
                    .header(SESSION_HEADER, secret)
                    .body(Body::empty())
                    .unwrap(),
            )
            .await
            .unwrap();
        assert_eq!(admitted.status(), StatusCode::OK);
    }

    #[tokio::test]
    async fn create_canonicalises_and_returns_the_workspace() {
        let app = app();
        let workspace = tempfile::tempdir().unwrap();
        let response = app
            .oneshot(
                Request::post("/v1/sessions")
                    .header("content-type", "application/json")
                    .body(Body::from(
                        json!({"agents":["a"], "workspace": workspace.path()}).to_string(),
                    ))
                    .unwrap(),
            )
            .await
            .unwrap();
        assert_eq!(response.status(), StatusCode::CREATED);
        let body = json_body(response).await;
        let expected = std::fs::canonicalize(workspace.path())
            .unwrap()
            .to_string_lossy()
            .into_owned();
        assert_eq!(body["workspace"].as_str(), Some(expected.as_str()));
    }

    #[tokio::test]
    async fn durable_agent_registrations_restore_with_api_confinement() {
        let events = MemoryStore::new();
        let spec = Spec {
            name: "stored".into(),
            base: "https://example.com".into(),
            acp: None,
            headers: BTreeMap::new(),
            vars: BTreeMap::new(),
            capabilities: Vec::new(),
            interests: Vec::new(),
            timeout: String::new(),
            open: None,
            turn: Step::default(),
            context: Vec::new(),
            origin: Origin::File,
        };
        events
            .append(
                SYSTEM_STREAM,
                "agent.registered",
                "admin",
                "",
                Some(json!({"spec": spec})),
            )
            .await
            .unwrap();
        let registry = Registry::new();
        assert_eq!(
            restore_agents(&events, &registry, Origin::Api)
                .await
                .unwrap(),
            1
        );
        assert_eq!(registry.get("stored").unwrap().origin, Origin::Api);
    }

    #[test]
    fn turn_limiter_charges_actual_agent_turns_and_reports_a_wait() {
        let limiter = Limiter::new(1).unwrap();
        assert!(limiter.allow("room").is_ok());
        limiter.charge("room", 2);
        assert!(limiter.allow("room").unwrap_err().as_secs() > 0);
        assert!(Limiter::new(0).is_none());
    }

    #[tokio::test]
    async fn context_conflicts_return_current_state_without_recording_a_write() {
        let app = app();
        let (id, secret) = create_room(&app).await;
        let path = format!("/v1/sessions/{id}/context/plan");
        let first = app
            .clone()
            .oneshot(
                Request::post(&path)
                    .header("content-type", "application/json")
                    .header(SESSION_HEADER, &secret)
                    .body(Body::from(r#"{"value":"one","author":"alice"}"#))
                    .unwrap(),
            )
            .await
            .unwrap();
        assert_eq!(first.status(), StatusCode::OK);

        let stale = app
            .clone()
            .oneshot(
                Request::post(&path)
                    .header("content-type", "application/json")
                    .header(SESSION_HEADER, &secret)
                    .header(header::IF_MATCH, "999")
                    .body(Body::from(r#"{"value":"two","author":"bob"}"#))
                    .unwrap(),
            )
            .await
            .unwrap();
        assert_eq!(stale.status(), StatusCode::CONFLICT);
        let conflict = json_body(stale).await;
        assert_eq!(conflict["key"], "plan");
        assert_eq!(conflict["current"], "one");
        assert_eq!(conflict["by"], "alice");

        let events = app
            .oneshot(
                Request::get(format!("/v1/sessions/{id}/events"))
                    .header(SESSION_HEADER, &secret)
                    .body(Body::empty())
                    .unwrap(),
            )
            .await
            .unwrap();
        let events = json_body(events).await;
        let kinds = events["events"]
            .as_array()
            .unwrap()
            .iter()
            .map(|event| event["kind"].as_str().unwrap())
            .collect::<Vec<_>>();
        assert_eq!(
            kinds.iter().filter(|kind| **kind == "context.set").count(),
            1
        );
        assert_eq!(
            kinds
                .iter()
                .filter(|kind| **kind == "conflict.rejected")
                .count(),
            1
        );
    }

    #[tokio::test]
    async fn reconnect_cursor_join_cookie_and_actor_query_match_the_contract() {
        let app = app();
        let (id, secret) = create_room(&app).await;
        let joined = app
            .clone()
            .oneshot(
                Request::post(format!("/v1/sessions/{id}/join"))
                    .header(SESSION_HEADER, &secret)
                    .body(Body::empty())
                    .unwrap(),
            )
            .await
            .unwrap();
        assert_eq!(joined.status(), StatusCode::OK);
        assert!(
            joined.headers()[header::SET_COOKIE]
                .to_str()
                .unwrap()
                .contains(&id)
        );

        let closed = app
            .clone()
            .oneshot(
                Request::post(format!("/v1/sessions/{id}/close?author=alice"))
                    .header(SESSION_HEADER, &secret)
                    .body(Body::empty())
                    .unwrap(),
            )
            .await
            .unwrap();
        assert_eq!(closed.status(), StatusCode::NO_CONTENT);

        let events = app
            .oneshot(
                Request::get(format!("/v1/sessions/{id}/events?since=0"))
                    .header(SESSION_HEADER, &secret)
                    .header("last-event-id", "1")
                    .body(Body::empty())
                    .unwrap(),
            )
            .await
            .unwrap();
        let events = json_body(events).await;
        let events = events["events"].as_array().unwrap();
        assert_eq!(events.len(), 1);
        assert_eq!(events[0]["kind"], "session.closed");
        assert_eq!(events[0]["actor"], "alice");
    }

    #[tokio::test]
    async fn auth_challenge_and_agent_delete_safety_are_enforced() {
        let registry = Registry::new();
        registry
            .put(Spec {
                name: "a".into(),
                base: "http://127.0.0.1:1".into(),
                acp: None,
                headers: BTreeMap::new(),
                vars: BTreeMap::new(),
                capabilities: Vec::new(),
                interests: Vec::new(),
                timeout: String::new(),
                open: None,
                turn: Step::default(),
                context: Vec::new(),
                origin: Origin::File,
            })
            .unwrap();
        let events: Arc<dyn EventStore> = Arc::new(MemoryStore::new());
        let executor = Executor::new();
        let manager = Manager::new(registry.clone(), executor.clone(), events.clone());
        let mut state = AppState::new(registry, executor, manager, events);
        state.token = "api-token".into();
        let app = router(state);

        let denied = app
            .clone()
            .oneshot(Request::get("/v1/agents").body(Body::empty()).unwrap())
            .await
            .unwrap();
        assert_eq!(denied.status(), StatusCode::UNAUTHORIZED);
        assert_eq!(
            denied.headers()[header::WWW_AUTHENTICATE],
            "Bearer realm=\"oryxa\""
        );
        let listed = app
            .clone()
            .oneshot(
                Request::get("/v1/agents")
                    .header(header::AUTHORIZATION, "bearer api-token")
                    .body(Body::empty())
                    .unwrap(),
            )
            .await
            .unwrap();
        assert_eq!(listed.status(), StatusCode::OK);

        let created = app
            .clone()
            .oneshot(
                Request::post("/v1/sessions")
                    .header(header::AUTHORIZATION, "Bearer api-token")
                    .header("content-type", "application/json")
                    .body(Body::from(r#"{"agents":["a"]}"#))
                    .unwrap(),
            )
            .await
            .unwrap();
        let id = json_body(created).await["id"].as_str().unwrap().to_owned();
        let refused = app
            .clone()
            .oneshot(
                Request::delete("/v1/agents/a")
                    .header(header::AUTHORIZATION, "Bearer api-token")
                    .body(Body::empty())
                    .unwrap(),
            )
            .await
            .unwrap();
        assert_eq!(refused.status(), StatusCode::CONFLICT);
        assert_eq!(json_body(refused).await["sessions"][0], id);
        let forced = app
            .oneshot(
                Request::delete("/v1/agents/a?force=true")
                    .header(header::AUTHORIZATION, "Bearer api-token")
                    .body(Body::empty())
                    .unwrap(),
            )
            .await
            .unwrap();
        assert_eq!(forced.status(), StatusCode::NO_CONTENT);
    }
}
