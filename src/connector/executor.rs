use std::{
    collections::BTreeMap,
    net::{IpAddr, Ipv4Addr, Ipv6Addr, SocketAddr},
    time::Instant,
};

use anyhow::{Context, Result, anyhow, bail};
use futures_util::StreamExt;
use reqwest::{Client, Method, Response, Url, header::HeaderMap};
use serde::{Deserialize, Serialize};
use serde_json::Value;

use tokio_util::sync::CancellationToken;

use super::{
    Origin, RenderContext, ResponseSpec, Spec, Step, acp::AcpExecutor, matches, select,
    select_first,
};

const OPEN_LIMIT: usize = 1 << 20;
const JSON_LIMIT: usize = 32 << 20;
const STREAM_ITEM_LIMIT: usize = 8 << 20;

#[derive(Clone, Debug, Deserialize, Serialize)]
pub struct Part {
    pub kind: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub text: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub raw: Option<Value>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub interaction: Option<String>,
}

#[derive(Clone)]
pub struct Executor {
    client: Client,
    acp: AcpExecutor,
}

impl Default for Executor {
    fn default() -> Self {
        Self::new()
    }
}

impl Executor {
    pub fn new() -> Self {
        Self {
            client: Client::builder()
                .redirect(reqwest::redirect::Policy::limited(10))
                .build()
                .expect("reqwest client"),
            acp: AcpExecutor::default(),
        }
    }

    pub fn with_client(client: Client) -> Self {
        Self {
            client,
            acp: AcpExecutor::default(),
        }
    }

    pub async fn open(
        &self,
        spec: &Spec,
        context: &RenderContext,
    ) -> Result<(String, BTreeMap<String, String>)> {
        if spec.is_acp() {
            return self.acp.open(spec, context).await;
        }
        let Some(step) = &spec.open else {
            return Ok((String::new(), BTreeMap::new()));
        };
        let response = self.send(spec, step, context).await?;
        let status = response.status();
        let bytes = response.bytes().await.context("read open response")?;
        if bytes.len() > OPEN_LIMIT {
            bail!("open response exceeded {OPEN_LIMIT} bytes");
        }
        if !status.is_success() {
            bail!("open returned {status}: {}", snippet(&bytes));
        }

        let mut captures = BTreeMap::new();
        let Ok(decoded) = serde_json::from_slice::<Value>(&bytes) else {
            return Ok((String::new(), captures));
        };
        for (name, path) in &step.capture {
            if let Some(value) = select(&decoded, path).into_iter().next() {
                captures.insert(name.clone(), value);
            }
        }
        let handle = captures.get("handle").cloned().unwrap_or_default();
        Ok((handle, captures))
    }

    pub async fn turn(
        &self,
        spec: &Spec,
        context: &RenderContext,
        emit: impl FnMut(Part) + Send,
    ) -> Result<()> {
        self.turn_cancellable(spec, context, CancellationToken::new(), emit)
            .await
    }

    pub async fn turn_cancellable(
        &self,
        spec: &Spec,
        context: &RenderContext,
        cancel: CancellationToken,
        mut emit: impl FnMut(Part) + Send,
    ) -> Result<()> {
        if spec.is_acp() {
            return self.acp.turn(spec, context, cancel, emit).await;
        }
        let response = self.send(spec, &spec.turn, context).await?;
        let status = response.status();
        if !status.is_success() {
            let bytes = response.bytes().await.unwrap_or_default();
            bail!("agent returned {status}: {}", snippet(&bytes));
        }

        let response_spec = spec.turn.response.as_ref();
        let format = response_spec
            .map(|response| response.format.as_str())
            .filter(|format| !format.is_empty())
            .unwrap_or("json");
        let mut spoken = false;
        let mut joined_emit = |mut part: Part| {
            if let Some(response_spec) = response_spec
                && !response_spec.join.is_empty()
                && part.kind == "text"
                && !part.text.is_empty()
            {
                if spoken {
                    part.text.insert_str(0, &response_spec.join);
                }
                spoken = true;
            }
            emit(part);
        };

        match format {
            "sse" => read_sse(response, response_spec, &mut joined_emit).await,
            "ndjson" => read_ndjson(response, response_spec, &mut joined_emit).await,
            _ => read_json(response, response_spec, &mut joined_emit).await,
        }
    }

    pub async fn close(&self, spec: &Spec, context: &RenderContext) {
        if spec.is_acp() {
            self.acp.close(context).await;
        }
    }

    pub async fn pending_interactions(&self, session: &str) -> Vec<super::acp::PendingInteraction> {
        self.acp.pending(session).await
    }

    pub async fn interaction(
        &self,
        session: &str,
        interaction: &str,
    ) -> Option<super::acp::PendingInteraction> {
        self.acp.interaction(session, interaction).await
    }

    pub async fn resolve_interaction(
        &self,
        session: &str,
        interaction: &str,
        option_id: Option<&str>,
    ) -> Result<super::acp::PendingInteraction> {
        self.acp.resolve(session, interaction, option_id).await
    }

    async fn send(&self, spec: &Spec, step: &Step, context: &RenderContext) -> Result<Response> {
        let method = if step.method.is_empty() {
            Method::POST
        } else {
            Method::from_bytes(step.method.as_bytes())
                .with_context(|| format!("invalid method {:?}", step.method))?
        };
        let base = context.render_string(&spec.base);
        let url = join_url(&base, &context.render_string(&step.path))?;
        if !matches!(url.scheme(), "http" | "https") {
            bail!(
                "connector URL must use http or https, got {:?}",
                url.scheme()
            );
        }

        let client = if spec.origin == Origin::Api {
            restricted_client(&url).await?
        } else {
            self.client.clone()
        };
        let mut request = client.request(method.clone(), url.clone());
        request = request.timeout(spec.timeout_duration());
        request = request.header("Accept", "text/event-stream, application/json");
        if let Some(body) = &step.body {
            request = request.json(&context.render(body));
        }
        let mut headers = context.render_headers(&spec.headers);
        headers.extend(context.render_headers(&step.headers));
        request = request.headers(header_map(headers)?);

        request
            .send()
            .await
            .with_context(|| format!("{method} {url}"))
    }

    pub async fn check(&self, spec: &Spec, probe: &str) -> CheckResult {
        let mut result = CheckResult {
            agent: spec.name.clone(),
            capabilities: spec.capabilities.clone(),
            ..Default::default()
        };
        let mut context = RenderContext {
            input: probe.to_owned(),
            turn: format!("t_{}", short_id()),
            conversation: format!("oryxa_check_{}", short_id()),
            agent: spec.name.clone(),
            vars: spec.vars.clone(),
            ..Default::default()
        };

        if let Some(open) = &spec.open {
            match self.open(spec, &context).await {
                Ok((handle, captures)) => {
                    result.reachable = true;
                    result.open = Some(StepResult {
                        ok: true,
                        handle: handle.clone(),
                        error: String::new(),
                    });
                    context.handle = handle;
                    context.captures = captures;
                    if result
                        .open
                        .as_ref()
                        .is_some_and(|value| value.handle.is_empty())
                        && !open.capture.is_empty()
                    {
                        result.warnings.push(
                            "open succeeded but captured no handle; {{handle}} will fall back to the session id"
                                .into(),
                        );
                    }
                }
                Err(error) => {
                    let message = error.to_string();
                    result.open = Some(StepResult {
                        ok: false,
                        handle: String::new(),
                        error: message.clone(),
                    });
                    result.error = format!("open failed: {message}");
                    return result;
                }
            }
        }

        let started = Instant::now();
        let mut parts = Vec::new();
        let turn = self.turn(spec, &context, |part| parts.push(part)).await;
        let mut turn_result = TurnResult {
            ok: turn.is_ok(),
            ms: started.elapsed().as_millis() as i64,
            parts: parts.len(),
            ..Default::default()
        };
        result.reachable = result.reachable || turn.is_ok();
        if let Err(error) = turn {
            turn_result.error = error.to_string();
            result.error = format!("turn failed: {error}");
            result.turn = Some(turn_result);
            self.close(spec, &context).await;
            return result;
        }

        let mut output = String::new();
        for part in &parts {
            match part.kind.as_str() {
                "text" => output.push_str(&part.text),
                "error" if !part.text.trim().is_empty() => {
                    turn_result.errors.push(part.text.trim().to_owned())
                }
                _ => {}
            }
        }
        turn_result.text_len = output.len();
        turn_result.sample = truncate(output.trim(), 200);
        if !turn_result.errors.is_empty() && turn_result.text_len == 0 {
            turn_result.ok = false;
            result.error = format!("agent reported an error: {}", turn_result.errors[0]);
        } else if !turn_result.errors.is_empty() {
            result.warnings.push(format!(
                "agent reported an error alongside its answer: {}",
                turn_result.errors[0]
            ));
        }
        if turn_result.parts == 0 {
            result.warnings.push("agent returned no parts".into());
        }
        if turn_result.text_len == 0 && turn_result.parts > 0 {
            result.warnings.push(
                "no text selector matched; output arrived as opaque activity. Check turn.response.text"
                    .into(),
            );
        }
        if spec
            .turn
            .response
            .as_ref()
            .is_none_or(|response| response.format.is_empty())
        {
            result.warnings.push(
                "no response.format set; defaulting to json (set sse or ndjson if the agent streams)"
                    .into(),
            );
        }
        if let Some(repeated) = doubled(&output) {
            result.warnings.push(format!(
                "output looks emitted twice ({:?}...). The agent probably streams deltas and then a final aggregated message — gate it with `when:`",
                truncate(repeated, 40)
            ));
        }
        result.turn = Some(turn_result);
        result.ok = result.error.is_empty();
        self.close(spec, &context).await;
        result
    }
}

/// Build a no-redirect client whose DNS result is both checked and pinned.
/// This keeps API-registered connectors away from loopback, private networks,
/// link-local services and DNS-rebinding targets. Operator-owned file specs are
/// intentionally exempt so local development connectors keep working.
async fn restricted_client(url: &Url) -> Result<Client> {
    if !url.username().is_empty() || url.password().is_some() {
        bail!("API connector URLs must not contain credentials");
    }
    let host = url
        .host_str()
        .ok_or_else(|| anyhow!("connector URL has no host"))?;
    let port = url
        .port_or_known_default()
        .ok_or_else(|| anyhow!("connector URL has no port"))?;

    let addresses = if let Ok(ip) = host.parse::<IpAddr>() {
        ensure_public(ip)?;
        vec![SocketAddr::new(ip, port)]
    } else {
        let addresses = tokio::net::lookup_host((host, port))
            .await
            .with_context(|| format!("resolve connector host {host:?}"))?
            .collect::<Vec<_>>();
        if addresses.is_empty() {
            bail!("connector host {host:?} resolved to no addresses");
        }
        for address in &addresses {
            ensure_public(address.ip())?;
        }
        addresses
    };

    let mut builder = Client::builder().redirect(reqwest::redirect::Policy::none());
    if host.parse::<IpAddr>().is_err() {
        builder = builder.resolve_to_addrs(host, &addresses);
    }
    builder.build().context("build restricted HTTP client")
}

fn ensure_public(ip: IpAddr) -> Result<()> {
    let is_public = match ip {
        IpAddr::V4(ip) => public_v4(ip),
        IpAddr::V6(ip) => public_v6(ip),
    };
    if is_public {
        Ok(())
    } else {
        bail!("API connector destination {ip} is not a public address")
    }
}

fn public_v4(ip: Ipv4Addr) -> bool {
    let [a, b, c, _] = ip.octets();
    !matches!(
        (a, b, c),
        (0, _, _)
            | (10, _, _)
            | (100, 64..=127, _)
            | (127, _, _)
            | (169, 254, _)
            | (172, 16..=31, _)
            | (192, 0, 0)
            | (192, 0, 2)
            | (192, 88, 99)
            | (192, 168, _)
            | (198, 18..=19, _)
            | (198, 51, 100)
            | (203, 0, 113)
            | (224..=255, _, _)
    )
}

fn public_v6(ip: Ipv6Addr) -> bool {
    if let Some(ipv4) = ip.to_ipv4_mapped() {
        return public_v4(ipv4);
    }
    let segments = ip.segments();
    !(ip.is_unspecified()
        || ip.is_loopback()
        || ip.is_multicast()
        || segments[0] & 0xfe00 == 0xfc00
        || segments[0] & 0xffc0 == 0xfe80
        || segments[0] & 0xffc0 == 0xfec0
        || (segments[0] == 0x2001 && segments[1] == 0x0db8))
}

async fn read_json(
    response: Response,
    response_spec: Option<&ResponseSpec>,
    emit: &mut impl FnMut(Part),
) -> Result<()> {
    let bytes = response.bytes().await?;
    if bytes.len() > JSON_LIMIT {
        bail!("agent response exceeded {JSON_LIMIT} bytes");
    }
    if !bytes.iter().any(|byte| !byte.is_ascii_whitespace()) {
        return Ok(());
    }
    emit_payload(
        String::from_utf8_lossy(&bytes).as_ref(),
        response_spec,
        emit,
    );
    Ok(())
}

async fn read_ndjson(
    response: Response,
    response_spec: Option<&ResponseSpec>,
    emit: &mut impl FnMut(Part),
) -> Result<()> {
    let mut stream = response.bytes_stream();
    let mut buffer = Vec::new();
    while let Some(chunk) = stream.next().await {
        buffer.extend_from_slice(&chunk?);
        while let Some(index) = buffer.iter().position(|byte| *byte == b'\n') {
            let line = buffer.drain(..=index).collect::<Vec<_>>();
            let line = String::from_utf8_lossy(&line);
            let line = line.trim();
            if !line.is_empty() {
                emit_payload(line, response_spec, emit);
            }
        }
        if buffer.len() > STREAM_ITEM_LIMIT {
            bail!("NDJSON line exceeded {STREAM_ITEM_LIMIT} bytes");
        }
    }
    let line = String::from_utf8_lossy(&buffer);
    if !line.trim().is_empty() {
        emit_payload(line.trim(), response_spec, emit);
    }
    Ok(())
}

async fn read_sse(
    response: Response,
    response_spec: Option<&ResponseSpec>,
    emit: &mut impl FnMut(Part),
) -> Result<()> {
    let mut stream = response.bytes_stream();
    let mut buffer = Vec::new();
    let mut data = Vec::<String>::new();
    while let Some(chunk) = stream.next().await {
        buffer.extend_from_slice(&chunk?);
        while let Some(index) = buffer.iter().position(|byte| *byte == b'\n') {
            let line = buffer.drain(..=index).collect::<Vec<_>>();
            let line = String::from_utf8_lossy(&line);
            let line = line.trim_end_matches(['\r', '\n']);
            if line.is_empty() {
                flush_sse(&mut data, response_spec, emit);
            } else if let Some(value) = line.strip_prefix("data:") {
                data.push(value.strip_prefix(' ').unwrap_or(value).to_owned());
            }
        }
        if buffer.len() > STREAM_ITEM_LIMIT {
            bail!("SSE line exceeded {STREAM_ITEM_LIMIT} bytes");
        }
    }
    flush_sse(&mut data, response_spec, emit);
    Ok(())
}

fn flush_sse(
    data: &mut Vec<String>,
    response_spec: Option<&ResponseSpec>,
    emit: &mut impl FnMut(Part),
) {
    if data.is_empty() {
        return;
    }
    let payload = data.join("\n");
    data.clear();
    if !payload.is_empty() && payload != "[DONE]" {
        emit_payload(&payload, response_spec, emit);
    }
}

fn emit_payload(payload: &str, response_spec: Option<&ResponseSpec>, emit: &mut impl FnMut(Part)) {
    let Ok(decoded) = serde_json::from_str::<Value>(payload) else {
        emit(Part {
            kind: "text".into(),
            text: payload.into(),
            raw: None,
            interaction: None,
        });
        return;
    };

    if let Some(response_spec) = response_spec {
        if !response_spec.error.is_empty() {
            let errors = select(&decoded, &response_spec.error);
            if !errors.is_empty() {
                emit(Part {
                    kind: "error".into(),
                    text: errors.join(" "),
                    raw: Some(decoded),
                    interaction: None,
                });
                return;
            }
        }
        if !response_spec.when.is_empty() && !matches(&decoded, &response_spec.when) {
            emit(Part {
                kind: "activity".into(),
                text: String::new(),
                raw: Some(decoded),
                interaction: None,
            });
            return;
        }
    }

    let texts = response_spec.map_or_else(
        || stringify_default(&decoded),
        |response_spec| {
            if response_spec.text.is_empty() {
                stringify_default(&decoded)
            } else {
                select_first(&decoded, &response_spec.text)
            }
        },
    );
    if texts.is_empty() {
        emit(Part {
            kind: "activity".into(),
            text: String::new(),
            raw: Some(decoded),
            interaction: None,
        });
        return;
    }
    for text in texts {
        emit(Part {
            kind: "text".into(),
            text,
            raw: Some(decoded.clone()),
            interaction: None,
        });
    }
}

fn stringify_default(value: &Value) -> Vec<String> {
    match value {
        Value::Null => Vec::new(),
        Value::String(value) if value.is_empty() => Vec::new(),
        Value::String(value) => vec![value.clone()],
        Value::Number(value) => vec![value.to_string()],
        Value::Bool(value) => vec![value.to_string()],
        Value::Array(values) => values.iter().flat_map(stringify_default).collect(),
        value => serde_json::to_string(value).into_iter().collect(),
    }
}

fn join_url(base: &str, path: &str) -> Result<Url> {
    let joined = if path.is_empty() {
        base.trim_end_matches('/').to_owned()
    } else {
        format!(
            "{}/{}",
            base.trim_end_matches('/'),
            path.trim_start_matches('/')
        )
    };
    Url::parse(&joined).map_err(|error| anyhow!("bad URL {joined:?}: {error}"))
}

fn header_map(headers: BTreeMap<String, String>) -> Result<HeaderMap> {
    let mut output = HeaderMap::new();
    for (key, value) in headers {
        output.insert(
            key.parse::<reqwest::header::HeaderName>()?,
            value.parse::<reqwest::header::HeaderValue>()?,
        );
    }
    Ok(output)
}

fn snippet(bytes: &[u8]) -> String {
    truncate(String::from_utf8_lossy(bytes).trim(), 300)
}

fn truncate(input: &str, length: usize) -> String {
    if input.len() <= length {
        input.to_owned()
    } else {
        format!("{}…", &input[..length])
    }
}

fn doubled(input: &str) -> Option<&str> {
    let input = input.trim();
    if input.len() < 16 || !input.len().is_multiple_of(2) {
        return None;
    }
    let half = input.len() / 2;
    (input[..half] == input[half..]).then_some(&input[..half])
}

fn short_id() -> String {
    uuid::Uuid::new_v4().simple().to_string()[..12].to_owned()
}

#[derive(Clone, Debug, Default, Deserialize, Serialize)]
pub struct CheckResult {
    pub agent: String,
    pub ok: bool,
    pub reachable: bool,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub open: Option<StepResult>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub turn: Option<TurnResult>,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub capabilities: Vec<String>,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub warnings: Vec<String>,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub error: String,
}

#[derive(Clone, Debug, Default, Deserialize, Serialize)]
pub struct StepResult {
    pub ok: bool,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub handle: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub error: String,
}

#[derive(Clone, Debug, Default, Deserialize, Serialize)]
pub struct TurnResult {
    pub ok: bool,
    pub ms: i64,
    pub parts: usize,
    pub text_len: usize,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub sample: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub error: String,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub errors: Vec<String>,
}

#[cfg(test)]
mod tests {
    use axum::{Json, Router, response::IntoResponse, routing::post};
    use serde_json::json;
    use tokio::net::TcpListener;

    use super::*;

    async fn test_server() -> String {
        async fn open() -> Json<Value> {
            Json(json!({"id": "remote-1"}))
        }
        async fn json_turn() -> Json<Value> {
            Json(json!({"output": "hello"}))
        }
        async fn sse_turn() -> impl IntoResponse {
            (
                [("content-type", "text/event-stream")],
                "data: {\"delta\":\"hel\"}\n\ndata: {\"delta\":\"lo\"}\n\n",
            )
        }
        async fn ndjson_turn() -> impl IntoResponse {
            (
                [("content-type", "application/x-ndjson")],
                "{\"message\":\"one\"}\n{\"message\":\"two\"}\n",
            )
        }
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let address = listener.local_addr().unwrap();
        let router = Router::new()
            .route("/open", post(open))
            .route("/json", post(json_turn))
            .route("/sse", post(sse_turn))
            .route("/ndjson", post(ndjson_turn));
        tokio::spawn(async move { axum::serve(listener, router).await.unwrap() });
        format!("http://{address}")
    }

    fn spec(base: String, path: &str, format: &str, selector: &str) -> Spec {
        Spec {
            name: "test".into(),
            base,
            acp: None,
            headers: BTreeMap::new(),
            vars: BTreeMap::new(),
            capabilities: Vec::new(),
            interests: Vec::new(),
            timeout: String::new(),
            open: None,
            turn: Step {
                path: path.into(),
                response: Some(ResponseSpec {
                    format: format.into(),
                    text: vec![selector.into()],
                    ..Default::default()
                }),
                ..Default::default()
            },
            context: Vec::new(),
            origin: super::super::Origin::File,
        }
    }

    #[tokio::test]
    async fn reads_all_three_response_shapes() {
        let base = test_server().await;
        let executor = Executor::new();
        for (path, format, selector, wanted) in [
            ("/json", "json", "$.output", "hello"),
            ("/sse", "sse", "$.delta", "hello"),
            ("/ndjson", "ndjson", "$.message", "onetwo"),
        ] {
            let mut output = String::new();
            executor
                .turn(
                    &spec(base.clone(), path, format, selector),
                    &RenderContext::default(),
                    |part| {
                        if part.kind == "text" {
                            output.push_str(&part.text);
                        }
                    },
                )
                .await
                .unwrap();
            assert_eq!(output, wanted);
        }
    }

    /// A stream of whole messages needs a separator; a stream of deltas must
    /// not have one.
    ///
    /// Without `join`, two complete sentences arrive concatenated mid-word —
    /// which is what an agent that sends messages rather than tokens produces.
    /// With it applied to a delta stream, every few characters would be pushed
    /// apart instead. The rule is only ever between parts, never before the
    /// first, so a reply does not open with a blank line.
    #[tokio::test]
    async fn join_separates_whole_messages_and_never_leads() {
        let base = test_server().await;
        let executor = Executor::new();
        let mut joined = spec(base.clone(), "/ndjson", "ndjson", "$.message");
        joined.turn.response.as_mut().unwrap().join = "\n\n".into();

        let mut output = String::new();
        executor
            .turn(&joined, &RenderContext::default(), |part| {
                if part.kind == "text" {
                    output.push_str(&part.text);
                }
            })
            .await
            .unwrap();
        assert_eq!(output, "one\n\ntwo");
        assert!(!output.starts_with('\n'), "the first part is not preceded");
    }

    #[tokio::test]
    async fn open_captures_a_handle() {
        let base = test_server().await;
        let mut spec = spec(base, "/json", "json", "$.output");
        spec.open = Some(Step {
            path: "/open".into(),
            capture: BTreeMap::from([("handle".into(), "$.id".into())]),
            ..Default::default()
        });
        let (handle, captures) = Executor::new()
            .open(&spec, &RenderContext::default())
            .await
            .unwrap();
        assert_eq!(handle, "remote-1");
        assert_eq!(captures["handle"], "remote-1");
    }

    #[tokio::test]
    async fn api_connectors_cannot_reach_loopback() {
        let base = test_server().await;
        let mut spec = spec(base, "/json", "json", "$.output");
        spec.origin = Origin::Api;
        let error = Executor::new()
            .turn(&spec, &RenderContext::default(), |_| {})
            .await
            .unwrap_err();
        assert!(error.to_string().contains("not a public address"));
    }

    #[test]
    fn public_address_classification_blocks_special_ranges() {
        for address in [
            "0.0.0.0",
            "10.0.0.1",
            "100.64.0.1",
            "127.0.0.1",
            "169.254.169.254",
            "172.16.0.1",
            "192.168.0.1",
            "198.51.100.1",
            "224.0.0.1",
            "::1",
            "fc00::1",
            "fe80::1",
            "2001:db8::1",
            "::ffff:127.0.0.1",
        ] {
            assert!(
                ensure_public(address.parse().unwrap()).is_err(),
                "{address}"
            );
        }
        for address in ["1.1.1.1", "8.8.8.8", "2606:4700:4700::1111"] {
            assert!(ensure_public(address.parse().unwrap()).is_ok(), "{address}");
        }
    }
}
