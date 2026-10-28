# Deterministic progress signals

This pure Go module identifies bounded repetition and unresolved uncertainty. It
does not detect truth or establish whether an output is a hallucination. It has
no storage, network, model calls, retries, goal changes or billing side effects.

`FingerprintArguments` creates a versioned semantic fingerprint of a JSON
object. Object order, insignificant whitespace, equivalent JSON escapes and
exact decimal spellings are normalized. Large integers retain their exact
values. Arrays retain order; strings retain case, whitespace and Unicode
normalization. Numbers are represented as coefficient and decimal exponent,
without floating point or exponent expansion. Approved raw argument bytes are
never rewritten.

`IntentFingerprint` binds stable tool identity to those arguments. Call IDs,
operation IDs, retries, grant renewal and configuration versions do not change
the semantic identity. Installed profile authorization and the exact approved
bytes remain independent caller obligations.

`Evaluate` accepts a validated policy and complete causal history. It returns
attributable signals for unresolved unknown outcomes, a repeated successful
write/destructive intent, repeated intents, repeated failed outcomes, alternating
A-B-A-B intents, and identical unverified model outputs. Suspicion stops automatic
work for human review; it never proves a semantic loop or invents a successful
effect. Errors also prohibit automatic dispatch.

The caller must:

- Supply one current durable projection per logical action/output, spanning prior
  runs and retries. Resolve an unknown action in its existing projection after
  reconciliation; do not count lifecycle events as new actions.
- Supply independently checked cumulative progress state. A fresh identifier,
  model plan, claim of completion, elapsed time or repeated receipt is not new
  progress. A changed progress digest resets soft suffix detectors only.
- Persist chosen signal evidence append-only in the transaction that refuses
  automatic continuation/dispatch. This module does not persist the decision.
- Separately reject semantic duplicate proposals inside an automatic batch
  before preparation/dispatch. A pending proposal is not evidence of an effect;
  the successful-effect detector deliberately does not pretend otherwise.
- Retain root time, money, step, agent and recovery limits outside this module.
  A false positive requires review, never a stronger model or a changed goal.

Resource bounds are 32 KiB JSON, depth 32, 4,096 nodes, 1,024 bytes per numeric
token, a signed 32-bit parsed exponent (with subsequent arithmetic in int64), and
512 history observations. Overflow is rejected; history must never be silently
truncated. Context cancellation is checked during parsing/hashing. Per-policy
soft thresholds are bounded between 2 and 16. The limits bound work and memory;
they are not a measured latency service-level objective.

Identical read polling currently stops at its configured repeat threshold unless
independent progress changes. Legitimate waiting must use trusted bounded wait
state or a separate reviewed policy in the future. Merely waiting for an already
running worker does not create a new logical observation. Different pagination
cursors and new errors are distinct; neither a semantic paraphrase nor the truth
of a source is inferred.

Author evidence is recorded in `author-evidence.json`. Passing this standalone
module does not establish transactional integration, automatic batch safety,
dispatch safety, or complete acceptance of ADR-0019/ADR-0020.
