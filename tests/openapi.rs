//! `openapi.yaml` is the contract every non-browser client is written against,
//! and it drifts silently: adding a route is a one-line change, documenting it
//! is a separate file, and nothing fails when only one of them happens.
//!
//! This is the check the Go server used to carry. It moved here when Rust
//! became the only implementation that ships.

use std::{collections::BTreeSet, fs};

/// Every route the router registers, as `method path` with the parameter names
/// removed — `/v1/sessions/{id}` and `/v1/sessions/{session}` are the same
/// route, and only one of the two files has to name it well.
fn registered() -> BTreeSet<String> {
    let source = fs::read_to_string("src/api/mod.rs").expect("the api module is readable");
    let mut routes = BTreeSet::new();
    let bytes = source.as_bytes();
    let mut at = 0;

    while let Some(found) = source[at..].find(".route(") {
        let start = at + found + ".route(".len();
        // Take the whole call, however it is wrapped: a long path and its
        // handlers are split across lines by rustfmt.
        let mut depth = 1;
        let mut end = start;
        while end < bytes.len() && depth > 0 {
            match bytes[end] {
                b'(' => depth += 1,
                b')' => depth -= 1,
                _ => {}
            }
            end += 1;
        }
        let call = &source[start..end - 1];
        at = end;

        let Some(path) = call
            .split_once('"')
            .and_then(|(_, rest)| rest.split_once('"'))
            .map(|(path, _)| path)
        else {
            continue;
        };
        for method in ["get", "post", "delete", "put", "patch"] {
            if call.contains(&format!("{method}(")) {
                routes.insert(format!("{method} {}", normalise(path)));
            }
        }
    }
    assert!(
        routes.len() > 10,
        "found {} routes, so the source scan is broken rather than the API",
        routes.len()
    );
    routes
}

/// Every route the specification promises.
fn documented() -> BTreeSet<String> {
    let raw = fs::read_to_string("openapi.yaml").expect("the specification is readable");
    let spec: serde_yaml::Value = serde_yaml::from_str(&raw).expect("the specification parses");
    let paths = spec
        .get("paths")
        .and_then(serde_yaml::Value::as_mapping)
        .expect("the specification has paths");

    let mut routes = BTreeSet::new();
    for (path, operations) in paths {
        let path = path.as_str().unwrap_or_default();
        let Some(operations) = operations.as_mapping() else {
            continue;
        };
        for (method, _) in operations {
            let method = method.as_str().unwrap_or_default();
            if ["get", "post", "delete", "put", "patch"].contains(&method) {
                routes.insert(format!("{method} {}", normalise(path)));
            }
        }
    }
    routes
}

fn normalise(path: &str) -> String {
    let mut out = String::new();
    let mut in_parameter = false;
    for ch in path.chars() {
        match ch {
            '{' => {
                in_parameter = true;
                out.push_str("{}");
            }
            '}' => in_parameter = false,
            _ if in_parameter => {}
            _ => out.push(ch),
        }
    }
    out
}

#[test]
fn the_specification_and_the_router_describe_the_same_api() {
    let registered = registered();
    let documented = documented();

    let undocumented = registered.difference(&documented).collect::<Vec<_>>();
    let phantom = documented.difference(&registered).collect::<Vec<_>>();

    assert!(
        undocumented.is_empty(),
        "routes with no entry in openapi.yaml:\n  {}",
        undocumented
            .iter()
            .map(ToString::to_string)
            .collect::<Vec<_>>()
            .join("\n  ")
    );
    assert!(
        phantom.is_empty(),
        "openapi.yaml promises routes that do not exist:\n  {}",
        phantom
            .iter()
            .map(ToString::to_string)
            .collect::<Vec<_>>()
            .join("\n  ")
    );
}

/// A dangling `$ref` breaks a generated client on the generator's own error
/// rather than on anything to do with the API.
#[test]
fn every_referenced_schema_is_defined() {
    let raw = fs::read_to_string("openapi.yaml").expect("the specification is readable");
    let spec: serde_yaml::Value = serde_yaml::from_str(&raw).expect("the specification parses");
    let defined = spec
        .get("components")
        .and_then(|components| components.get("schemas"))
        .and_then(serde_yaml::Value::as_mapping)
        .map(|schemas| {
            schemas
                .keys()
                .filter_map(serde_yaml::Value::as_str)
                .map(str::to_string)
                .collect::<BTreeSet<_>>()
        })
        .unwrap_or_default();

    let mut missing = BTreeSet::new();
    let mut at = 0;
    while let Some(found) = raw[at..].find("#/components/schemas/") {
        let start = at + found + "#/components/schemas/".len();
        let end = raw[start..]
            .find(|ch: char| !ch.is_alphanumeric() && ch != '_' && ch != '-')
            .map(|offset| start + offset)
            .unwrap_or(raw.len());
        let name = &raw[start..end];
        if !defined.contains(name) {
            missing.insert(name.to_string());
        }
        at = end;
    }
    assert!(
        missing.is_empty(),
        "referenced but never defined: {}",
        missing.into_iter().collect::<Vec<_>>().join(", ")
    );
}
