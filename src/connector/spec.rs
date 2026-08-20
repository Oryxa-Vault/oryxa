use std::{
    collections::BTreeMap,
    fs,
    path::{Path, PathBuf},
    sync::{Arc, RwLock},
    time::Duration,
};

use serde::{Deserialize, Serialize};
use serde_json::Value;
use thiserror::Error;

pub const SOURCE_TEXT: &str = "$text";

/// Where a connector came from. Public API connectors are restricted to public
/// destinations; private API connectors may reach private networks, but only
/// file connectors are trusted to name local processes.
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub enum Origin {
    #[default]
    File,
    Api,
    ApiPrivate,
}

#[derive(Clone, Debug, Deserialize, Serialize)]
pub struct Spec {
    pub name: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub base: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub acp: Option<AcpSpec>,
    #[serde(default, skip_serializing_if = "BTreeMap::is_empty")]
    pub headers: BTreeMap<String, String>,
    #[serde(default, skip_serializing_if = "BTreeMap::is_empty")]
    pub vars: BTreeMap<String, String>,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub capabilities: Vec<String>,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub interests: Vec<String>,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub timeout: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub open: Option<Step>,
    #[serde(default)]
    pub turn: Step,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub context: Vec<ContextRule>,
    #[serde(skip)]
    pub origin: Origin,
}

/// Operator-controlled launch configuration for an ACP agent. Commands are
/// represented as an executable plus arguments, never as a shell string.
#[derive(Clone, Debug, Default, Deserialize, Serialize)]
pub struct AcpSpec {
    pub command: String,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub args: Vec<String>,
    #[serde(default, skip_serializing_if = "BTreeMap::is_empty")]
    pub env: BTreeMap<String, String>,
    pub cwd: String,
    /// What the agent is sent each turn.
    ///
    /// Empty means `{{input}}` — the message and nothing else, which is what
    /// an ACP lane sent before this existed. An HTTP connector decides this in
    /// its request body; an ACP one has no body to decide it in, and without
    /// this an ACP agent is in the room without being able to see it. Its own
    /// history it keeps: one ACP session lives for the life of the lane.
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub prompt: String,
}

impl AcpSpec {
    /// The template rendered for each turn, with the historical default.
    pub fn prompt(&self) -> &str {
        if self.prompt.trim().is_empty() {
            "{{input}}"
        } else {
            &self.prompt
        }
    }
}

#[derive(Clone, Debug, Default, Deserialize, Serialize)]
pub struct Step {
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub method: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub path: String,
    #[serde(default, skip_serializing_if = "BTreeMap::is_empty")]
    pub headers: BTreeMap<String, String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub body: Option<Value>,
    #[serde(default, skip_serializing_if = "BTreeMap::is_empty")]
    pub capture: BTreeMap<String, String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub response: Option<ResponseSpec>,
}

#[derive(Clone, Debug, Default, Deserialize, Serialize)]
pub struct ResponseSpec {
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub format: String,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub text: Vec<String>,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub done: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub error: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub when: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub join: String,
}

#[derive(Clone, Debug, Deserialize, Serialize)]
pub struct ContextRule {
    pub key: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub kind: String,
    pub from: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub when: String,
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    pub pin: bool,
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    pub last: bool,
}

impl ContextRule {
    pub fn from_text(&self) -> bool {
        self.from.trim() == SOURCE_TEXT
    }

    pub fn kind(&self) -> &str {
        if self.kind.is_empty() {
            "append"
        } else {
            &self.kind
        }
    }
}

#[derive(Debug, Error)]
pub enum SpecError {
    #[error("{0}")]
    Invalid(String),
    #[error("{path}: {source}")]
    File {
        path: PathBuf,
        #[source]
        source: Box<dyn std::error::Error + Send + Sync>,
    },
}

impl Spec {
    pub fn validate(&self) -> Result<(), SpecError> {
        if self.name.trim().is_empty() {
            return Err(SpecError::Invalid("name is required".into()));
        }
        match (&self.acp, self.base.trim()) {
            (None, "") => return Err(SpecError::Invalid("base or acp is required".into())),
            (Some(_), base) if !base.is_empty() => {
                return Err(SpecError::Invalid(
                    "base and acp are mutually exclusive transports".into(),
                ));
            }
            (None, base)
                if !base.contains("{{")
                    && !base.starts_with("http://")
                    && !base.starts_with("https://") =>
            {
                return Err(SpecError::Invalid(format!(
                    "base must be an http(s) URL, got {:?}",
                    self.base
                )));
            }
            _ => {}
        }
        if let Some(acp) = &self.acp {
            if self.origin != Origin::File {
                return Err(SpecError::Invalid(
                    "ACP commands are only allowed in operator-controlled connector files".into(),
                ));
            }
            if acp.command.trim().is_empty() {
                return Err(SpecError::Invalid("acp.command is required".into()));
            }
            if acp.cwd.trim().is_empty() {
                return Err(SpecError::Invalid("acp.cwd is required".into()));
            }
            if !acp.cwd.contains("{{") && !Path::new(&acp.cwd).is_absolute() {
                return Err(SpecError::Invalid(
                    "acp.cwd must be an absolute path".into(),
                ));
            }
        }
        if let Some(response) = &self.turn.response {
            match response.format.as_str() {
                "" | "json" | "sse" | "ndjson" => {}
                other => {
                    return Err(SpecError::Invalid(format!(
                        "turn.response.format must be sse, ndjson or json, got {other:?}"
                    )));
                }
            }
        }
        if !self.timeout.is_empty() {
            parse_duration(&self.timeout).map_err(SpecError::Invalid)?;
        }
        let mut seen = std::collections::BTreeSet::new();
        for (index, rule) in self.context.iter().enumerate() {
            let at = format!("context[{index}]");
            if rule.key.trim().is_empty() {
                return Err(SpecError::Invalid(format!("{at}: key is required")));
            }
            if rule.from.trim().is_empty() {
                return Err(SpecError::Invalid(format!(
                    "{at}: from is required ($text or a selector)"
                )));
            }
            if !matches!(rule.kind(), "append" | "value") {
                return Err(SpecError::Invalid(format!(
                    "{at}: kind must be append or value, got {:?}",
                    rule.kind
                )));
            }
            if !seen.insert(rule.key.as_str()) {
                return Err(SpecError::Invalid(format!(
                    "{at}: duplicate rule for key {:?}",
                    rule.key
                )));
            }
        }
        Ok(())
    }

    pub fn timeout_duration(&self) -> Duration {
        if self.timeout.is_empty() {
            Duration::from_secs(5 * 60)
        } else {
            parse_duration(&self.timeout).unwrap_or(Duration::from_secs(5 * 60))
        }
    }

    pub fn has(&self, capability: &str) -> bool {
        self.capabilities
            .iter()
            .any(|candidate| candidate == capability)
    }

    /// Which shared-context keys this connector's turn refers to.
    ///
    /// An ACP connector says so in its prompt template and an HTTP one in its
    /// request; asking the spec rather than the step is what keeps a room's
    /// context working the same on both transports.
    pub fn context_refs(&self) -> Vec<String> {
        match &self.acp {
            Some(acp) => crate::connector::template::refs_in(acp.prompt()),
            None => self.turn.context_refs(),
        }
    }

    /// The turn template for an ACP connector, or the bare message for
    /// anything that is not one.
    pub fn acp_prompt(&self) -> &str {
        match &self.acp {
            Some(acp) => acp.prompt(),
            None => "{{input}}",
        }
    }

    pub fn is_acp(&self) -> bool {
        self.acp.is_some()
    }

    pub fn from_yaml(bytes: &[u8]) -> Result<Self, SpecError> {
        let spec: Self =
            serde_yaml::from_slice(bytes).map_err(|error| SpecError::Invalid(error.to_string()))?;
        spec.validate()?;
        Ok(spec)
    }

    pub fn from_json(bytes: &[u8]) -> Result<Self, SpecError> {
        let spec: Self =
            serde_json::from_slice(bytes).map_err(|error| SpecError::Invalid(error.to_string()))?;
        spec.validate()?;
        Ok(spec)
    }
}

fn parse_duration(input: &str) -> Result<Duration, String> {
    let input = input.trim();
    let split = input
        .find(|character: char| !character.is_ascii_digit() && character != '.')
        .ok_or_else(|| format!("timeout {input:?} is not a duration"))?;
    let amount: f64 = input[..split]
        .parse()
        .map_err(|_| format!("timeout {input:?} is not a duration"))?;
    let seconds = match &input[split..] {
        "ms" => amount / 1000.0,
        "s" => amount,
        "m" => amount * 60.0,
        "h" => amount * 3600.0,
        _ => return Err(format!("timeout {input:?} is not a duration")),
    };
    if !seconds.is_finite() || seconds < 0.0 {
        return Err(format!("timeout {input:?} is not a duration"));
    }
    Ok(Duration::from_secs_f64(seconds))
}

/// Thread-safe runtime registry. A clone shares the same live set.
#[derive(Clone, Default)]
pub struct Registry {
    specs: Arc<RwLock<BTreeMap<String, Arc<Spec>>>>,
}

impl Registry {
    pub fn new() -> Self {
        Self::default()
    }

    pub fn put(&self, spec: Spec) -> Result<(), SpecError> {
        spec.validate()?;
        self.specs
            .write()
            .expect("connector registry poisoned")
            .insert(spec.name.clone(), Arc::new(spec));
        Ok(())
    }

    pub fn get(&self, name: &str) -> Option<Arc<Spec>> {
        self.specs
            .read()
            .expect("connector registry poisoned")
            .get(name)
            .cloned()
    }

    pub fn delete(&self, name: &str) -> bool {
        self.specs
            .write()
            .expect("connector registry poisoned")
            .remove(name)
            .is_some()
    }

    pub fn list(&self) -> Vec<Arc<Spec>> {
        self.specs
            .read()
            .expect("connector registry poisoned")
            .values()
            .cloned()
            .collect()
    }

    pub fn load_dir(&self, directory: impl AsRef<Path>) -> Result<usize, SpecError> {
        let directory = directory.as_ref();
        let entries = match fs::read_dir(directory) {
            Ok(entries) => entries,
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => return Ok(0),
            Err(error) => {
                return Err(SpecError::File {
                    path: directory.to_path_buf(),
                    source: Box::new(error),
                });
            }
        };
        let mut paths = entries
            .filter_map(Result::ok)
            .map(|entry| entry.path())
            .filter(|path| path.is_file())
            .collect::<Vec<_>>();
        paths.sort();

        let mut loaded = 0;
        for path in paths {
            let extension = path
                .extension()
                .and_then(|part| part.to_str())
                .unwrap_or_default()
                .to_ascii_lowercase();
            if !matches!(extension.as_str(), "yaml" | "yml" | "json") {
                continue;
            }
            let bytes = fs::read(&path).map_err(|error| SpecError::File {
                path: path.clone(),
                source: Box::new(error),
            })?;
            let mut spec = if extension == "json" {
                Spec::from_json(&bytes)
            } else {
                Spec::from_yaml(&bytes)
            }
            .map_err(|error| SpecError::File {
                path: path.clone(),
                source: Box::new(error),
            })?;
            spec.origin = Origin::File;
            self.put(spec)?;
            loaded += 1;
        }
        Ok(loaded)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// How many connector files a directory ships, so the assertions below are
    /// about every file being loaded rather than about a number that has to be
    /// edited each time someone adds one.
    fn yaml_files(directory: &str) -> usize {
        std::fs::read_dir(directory)
            .unwrap()
            .filter_map(Result::ok)
            .filter(|entry| entry.path().is_file())
            .filter(|entry| {
                matches!(
                    entry.path().extension().and_then(|part| part.to_str()),
                    Some("yaml" | "yml")
                )
            })
            .count()
    }

    #[test]
    fn every_shipped_connector_parses() {
        let registry = Registry::new();
        let count = registry.load_dir("connectors").unwrap();
        assert_eq!(count, yaml_files("connectors"));
        assert!(count > 0, "the connector directory is empty");
        assert!(registry.get("mock-json").is_some());
        // Both coding agents are reachable over ACP, which is the transport an
        // editor launches them with too.
        for agent in ["codex-local", "claude-code-local"] {
            assert!(
                registry.get(agent).is_some_and(|spec| spec.is_acp()),
                "{agent} should be an ACP connector"
            );
        }
        assert_eq!(
            registry
                .get("codex")
                .unwrap()
                .turn
                .response
                .as_ref()
                .unwrap()
                .join,
            "\n\n"
        );
    }

    #[test]
    fn templates_parse() {
        let registry = Registry::new();
        assert_eq!(
            registry.load_dir("connectors/templates").unwrap(),
            yaml_files("connectors/templates")
        );
    }

    #[test]
    fn validation_rejects_duplicate_context_rules() {
        let error = Spec::from_yaml(
            br#"
name: x
base: http://localhost
turn: {path: /run}
context:
  - {key: findings, from: $text}
  - {key: findings, from: $.facts}
"#,
        )
        .unwrap_err();
        assert!(error.to_string().contains("duplicate rule"));
    }

    #[test]
    fn acp_requires_an_operator_controlled_absolute_workspace() {
        let spec = Spec::from_yaml(
            br#"
name: local-agent
acp:
  command: agent
  args: [acp]
  cwd: /tmp/workspace
capabilities: [streaming]
"#,
        )
        .unwrap();
        assert!(spec.is_acp());
        assert!(spec.base.is_empty());

        let mut from_api = spec;
        from_api.origin = Origin::Api;
        assert!(
            from_api
                .validate()
                .unwrap_err()
                .to_string()
                .contains("operator-controlled")
        );
        from_api.origin = Origin::ApiPrivate;
        assert!(
            from_api
                .validate()
                .unwrap_err()
                .to_string()
                .contains("operator-controlled")
        );
    }
}
