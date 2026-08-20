use axum::{
    Json, Router,
    response::{IntoResponse, Sse, sse::Event},
    routing::post,
};
use futures_util::stream;
use serde::Deserialize;
use serde_json::{Value, json};

#[derive(Deserialize)]
struct Invoke {
    #[serde(default)]
    prompt: String,
}

async fn invoke(Json(request): Json<Invoke>) -> Json<Value> {
    Json(json!({"output": format!("mock answer to: {}", request.prompt)}))
}

async fn open() -> Json<Value> {
    Json(json!({"id": uuid::Uuid::new_v4().to_string()}))
}

async fn run_sse(Json(body): Json<Value>) -> impl IntoResponse {
    let input = body
        .pointer("/new_message/parts/0/text")
        .and_then(Value::as_str)
        .unwrap_or_default()
        .to_owned();
    let events = [
        "mock ".to_owned(),
        "answer ".to_owned(),
        "to: ".to_owned(),
        input,
    ]
    .into_iter()
    .map(|text| {
        Ok::<_, std::convert::Infallible>(
            Event::default()
                .json_data(json!({"content": {"parts": [{"text": text}]}}))
                .unwrap(),
        )
    });
    Sse::new(stream::iter(events))
}

#[tokio::main]
async fn main() -> anyhow::Result<()> {
    let app = Router::new()
        .route("/invoke", post(invoke))
        .route("/apps/{app}/users/{user}/sessions/{session}", post(open))
        .route("/run_sse", post(run_sse));
    let listener = tokio::net::TcpListener::bind("127.0.0.1:9000").await?;
    println!("mockagent listening on http://127.0.0.1:9000");
    axum::serve(listener, app).await?;
    Ok(())
}
