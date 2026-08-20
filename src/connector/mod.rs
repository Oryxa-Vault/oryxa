//! Declarative descriptions of HTTP agents.

mod acp;
mod executor;
mod selector;
mod spec;
mod template;

pub use acp::{PendingInteraction, PermissionOption};
pub use executor::{CheckResult, Executor, Part, StepResult, TurnResult};
pub use selector::{matches, select, select_first, truthy};
pub use spec::{AcpSpec, ContextRule, Origin, Registry, ResponseSpec, Spec, SpecError, Step};
pub use template::{ContextView, RenderContext};
