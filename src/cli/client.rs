//! An HTTP client for a running Oryxa.

use std::time::Duration;

use anyhow::{Result, anyhow, bail};
use async_stream::try_stream;
use bytes::BytesMut;
use futures_util::{Stream, StreamExt};
use reqwest::{Method, StatusCode};
use serde::{Serialize, de::DeserializeOwned};
use serde_json::Value;

use crate::{cli::rooms, events::Event};

pub const DEFAULT_SERVER: &str = "http://localhost:8080";

#[derive(Clone)]
pub struct Client {
    base: String,
    token: String,
    /// A room secret passed on the command line, which beats anything written
    /// down. Empty means "look the room up".
    secret: Option<String>,
    http: reqwest::Client,
    /// A second client for streams, with no timeout. The room view reconnects
    /// on a loop, and building a client per attempt throws away the connection
    /// pool each time.
    streaming: reqwest::Client,
}

impl Client {
    pub fn new(server: &str, token: &str, secret: Option<String>) -> Self {
        let base = match server.trim() {
            "" => std::env::var("ORYXA_URL").unwrap_or_else(|_| DEFAULT_SERVER.into()),
            value => value.to_string(),
        };
        let token = match token.trim() {
            "" => std::env::var("ORYXA_TOKEN").unwrap_or_default(),
            value => value.to_string(),
        };
        Self {
            base: base.trim_end_matches('/').to_string(),
            token,
            secret: secret.filter(|value| !value.trim().is_empty()),
            // A request is one round trip against a local or nearby server. The
            // stream below is deliberately not bounded this way.
            http: reqwest::Client::builder()
                .timeout(Duration::from_secs(60))
                .build()
                .expect("http client builds"),
            streaming: reqwest::Client::new(),
        }
    }

    pub fn base(&self) -> &str {
        &self.base
    }

    pub async fn get<T: DeserializeOwned>(&self, path: &str) -> Result<T> {
        self.send(Method::GET, path, None::<()>).await
    }

    pub async fn post<T: DeserializeOwned>(&self, path: &str, body: impl Serialize) -> Result<T> {
        self.send(Method::POST, path, Some(body)).await
    }

    pub async fn delete(&self, path: &str) -> Result<()> {
        let _: Value = self.send(Method::DELETE, path, None::<()>).await?;
        Ok(())
    }

    async fn send<T: DeserializeOwned>(
        &self,
        method: Method,
        path: &str,
        body: Option<impl Serialize>,
    ) -> Result<T> {
        let mut request = self.request(method, path);
        if let Some(body) = body {
            request = request.json(&body);
        }
        let response = request
            .send()
            .await
            .map_err(|error| self.unreachable(error))?;
        let status = response.status();
        let raw = response.bytes().await.unwrap_or_default();
        if status == StatusCode::UNAUTHORIZED {
            bail!("unauthorized — pass --token or set ORYXA_TOKEN");
        }
        if status.is_client_error() || status.is_server_error() {
            bail!("{}", server_error(status, &raw));
        }
        if raw.is_empty() {
            // A 204 body still has to satisfy the caller's type, and every
            // caller that accepts one asks for a `Value`.
            return Ok(serde_json::from_str("null")?);
        }
        Ok(serde_json::from_slice(&raw)?)
    }

    /// Follows a room's SSE stream. Returns once the server closes it.
    ///
    /// No timeout, unlike the requests above: a stream stays open for the life
    /// of the room, and being disconnected after a quiet minute is the bug this
    /// avoids rather than the safety it provides.
    pub async fn events(
        &self,
        session: &str,
        since: i64,
    ) -> Result<impl Stream<Item = Result<Event>>> {
        let path = format!("/v1/sessions/{session}/stream?since={since}");
        let response = self
            .streaming
            .execute(self.request(Method::GET, &path).build()?)
            .await
            .map_err(|error| self.unreachable(error))?;
        let status = response.status();
        if status == StatusCode::UNAUTHORIZED {
            bail!("unauthorized — pass --token or set ORYXA_TOKEN");
        }
        if status.is_client_error() || status.is_server_error() {
            let raw = response.bytes().await.unwrap_or_default();
            bail!("{}", server_error(status, &raw));
        }

        let mut body = response.bytes_stream();
        Ok(try_stream! {
            let mut decoder = Sse::default();
            while let Some(chunk) = body.next().await {
                decoder.push(&chunk?);
                while let Some(payload) = decoder.next_payload() {
                    // A frame the client does not understand is not a reason to
                    // drop the stream: the server may be newer than this binary.
                    if let Ok(event) = serde_json::from_str::<Event>(&payload) {
                        yield event;
                    }
                }
            }
        })
    }

    fn request(&self, method: Method, path: &str) -> reqwest::RequestBuilder {
        let mut request = self.http.request(method, format!("{}{path}", self.base));
        if !self.token.is_empty() {
            request = request.bearer_auth(&self.token);
        }
        // The token says you may talk to this server; the room secret says which
        // rooms are yours. Looked up per request from the path, so no command has
        // to thread it through by hand.
        if let Some(session) = rooms::session_from_path(path)
            && let Some(secret) = rooms::secret(session, self.secret.as_deref())
        {
            request = request.header("X-Oryxa-Session", secret);
        }
        request
    }

    /// The first thing a fresh install runs into, so it says what to do rather
    /// than only what went wrong.
    fn unreachable(&self, error: reqwest::Error) -> anyhow::Error {
        anyhow!(
            "cannot reach {}: {error}\n  \
             `oryxa serve` starts one, `oryxa` opens a room view that brings its own,\n  \
             and --server (or ORYXA_URL) points at one somewhere else",
            self.base
        )
    }
}

/// Whether something is answering as an Oryxa server.
///
/// Deliberately impatient: this runs before the interface is on screen, and a
/// server that is not there should cost a moment rather than the request
/// timeout above.
pub async fn reachable(base: &str) -> bool {
    let Ok(http) = reqwest::Client::builder()
        .timeout(Duration::from_millis(1500))
        .build()
    else {
        return false;
    };
    http.get(format!("{base}/health"))
        .send()
        .await
        .is_ok_and(|response| response.status().is_success())
}

fn server_error(status: StatusCode, raw: &[u8]) -> String {
    #[derive(serde::Deserialize)]
    struct Body {
        error: String,
    }
    if let Ok(body) = serde_json::from_slice::<Body>(raw)
        && !body.error.is_empty()
    {
        return body.error;
    }
    let text = String::from_utf8_lossy(raw);
    let text = text.trim();
    if text.is_empty() {
        return status.to_string();
    }
    format!("{status}: {text}")
}

/// Pulls one data payload at a time out of an SSE stream.
///
/// Small on purpose: the wire format is a handful of line prefixes, and the one
/// property that matters here is that a multi-line `data` field is rejoined
/// rather than delivered as fragments.
#[derive(Default)]
struct Sse {
    buffer: BytesMut,
    data: Vec<String>,
}

impl Sse {
    fn push(&mut self, chunk: &[u8]) {
        self.buffer.extend_from_slice(chunk);
    }

    fn next_payload(&mut self) -> Option<String> {
        while let Some(index) = self.buffer.iter().position(|byte| *byte == b'\n') {
            let line = self.buffer.split_to(index + 1);
            let line = String::from_utf8_lossy(&line[..index]);
            let line = line.trim_end_matches('\r');
            if line.is_empty() {
                if self.data.is_empty() {
                    continue; // keep-alive, or a comment-only frame
                }
                return Some(std::mem::take(&mut self.data).join("\n"));
            }
            if let Some(rest) = line.strip_prefix("data:") {
                self.data
                    .push(rest.strip_prefix(' ').unwrap_or(rest).to_string());
            }
        }
        None
    }
}

#[cfg(test)]
mod tests {
    use super::Sse;

    #[test]
    fn rejoins_a_multi_line_payload_and_skips_keep_alives() {
        let mut sse = Sse::default();
        sse.push(b": keep-alive\n\nid: 4\ndata: {\"a\":1,\n");
        assert_eq!(sse.next_payload(), None);
        sse.push(b"data: \"b\":2}\n\n");
        assert_eq!(sse.next_payload().as_deref(), Some("{\"a\":1,\n\"b\":2}"));
        assert_eq!(sse.next_payload(), None);
    }

    #[test]
    fn survives_a_frame_split_mid_line() {
        let mut sse = Sse::default();
        sse.push(b"data: hel");
        assert_eq!(sse.next_payload(), None);
        sse.push(b"lo\n\n");
        assert_eq!(sse.next_payload().as_deref(), Some("hello"));
    }
}
