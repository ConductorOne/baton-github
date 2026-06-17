<!-- This file is managed by baton-admin. DO NOT EDIT. -->
# Baton Connector Repo-Local Review Criteria (`ci-review.md` for kind: baton)

Repo-local PR-review criteria for plain `baton` connector repositories. This file is
consumed as DATA by the CI review prompt, layered on top of two things it does NOT repeat:

1. `base-pr-review.md` — generic security, correctness, SDK-compat, test/doc criteria,
   severity rubric, and the post/verdict procedure.
2. `mixins/connector.md` — generic baton-connector hygiene: file-context map, Client
   (C1-C8), Resource (R1-R13), Connector (N1-N4), HTTP Safety (H1-H5), Provisioning
   (P1-P6), Breaking Changes (B1-B9), Forbidden Patterns (F1-F3), Config (G1-G4),
   Documentation Staleness (D1-D4), Known Safe Patterns, Top Bug Detection, Dependency
   Checks.

**Do not restate base or connector.md criteria.** Everything below is ADDITIVE: the
operational depth that connector.md states only as one-liners (or omits), drawn from the
production experience captured in baton-admin's `connector/` reference docs. Apply these
only when the relevant files actually changed.

---

## A. Log-Level Classification (own logging only)

connector.md H4/H5 covers "no error swallowing, no secrets in logs" but not log *level*.
Misclassified `l.Error(...)` in connector code creates alert noise and permanently
retained OTEL error spans. These rules apply to logging the connector writes itself —
`uhttp.BaseHttpClient` already classifies HTTP responses correctly.

- L1: Upstream 4xx (401/403/404/409/429), OAuth refresh failures, and bad-config init
  failures → `Warn`. They reflect customer config or expected conditions, not connector
  bugs. Flag `l.Error(...)` on any of these.
- L2: Upstream 5xx and genuine connector code bugs / impossible states → `Error`.
- L3: Nil/zero/missing-but-expected values and gracefully-handled unknown enum variants
  → `Debug`, not Warn (e.g. "access key last-used date is nil").
- L4: Skip-and-continue (per-item graceful degradation) → `Warn` + `return nil` is correct
  and is NOT error swallowing. Do not flag it as swallowing under H4.
- L5: Context cancellation (`context.Canceled` / `DeadlineExceeded`) → `Debug`/`Warn`.
- L6: Do not log at `Error` AND return the same error — the SDK logs returned errors;
  double-logging is noise. Local `Warn`/`Debug` + return is fine.
- L7: A warning that can fire per-resource (1000+/sync) should use logarithmic sampling
  (1, 10, 100, every 1000) with a `total_occurrences` field.

The test before writing `l.Error`: is it a connector code bug? does the connector stop?
is it unexpected? If all three point away from Error, use Warn or Debug.

## B. Error Wrapping Beyond `%w` (R4 depth)

connector.md R4 says "use `%w` and `uhttp.WrapErrors` where appropriate." Detail on *when*
and *which code*:

- E1: `uhttp.WrapErrors(preferredCode, msg, errs...)` is for errors that did NOT go through
  `uhttp.BaseHttpClient` — vendor SDK calls, raw `http.Client`, or developer-inferred
  failures ("HTTP 200 but body says failed"). If all requests use `uhttp.BaseHttpClient`,
  uhttp wraps automatically — do not require manual wrapping.
- E2: `preferredCode` is a gRPC `codes.Code`, not an HTTP status. Expected mapping:
  401→`Unauthenticated`, 403→`PermissionDenied`, 404→`NotFound`, 429→`ResourceExhausted`,
  5xx→`Internal`. The SDK reads this code to decide retry vs surface.
- E3: Provisioning errors (P4) should carry a gRPC status code so Grant/Revoke failures
  surface correctly.

## C. Span / Tracing Safety

connector.md does not cover spans. Apply only when the connector creates spans manually
(vendor-SDK calls, local batch processing); pure `uhttp.BaseHttpClient` connectors usually
need none.

- T1: Every `tracer.Start(...)` is followed by `defer span.End()` immediately — no End()
  only on the success path.
- T2: `span.RecordError(err)` alone leaves span status OK in APM. Require a paired
  `span.SetStatus(otelcodes.Error, ...)` (or `ctxotel.RecordError`).
- T3: No secrets or PII in span attributes (API keys, tokens, emails, names, request
  bodies). Stable IDs only — `user.id`, not `user.email`. This extends H5 to spans.
- T4: Span names are static low-cardinality snake_case; dynamic values go in attributes.
- T5: Per-resource API calls in a loop that can exceed ~100 iterations should break the
  trace with `trace.WithNewRoot()` + a link to the parent, to avoid span explosion.

## D. JSON Type Safety (API response structs)

Not in connector.md. Inconsistent upstream APIs cause `cannot unmarshal number into Go
struct field ... of type string` failures that abort a sync.

- J1: `ID string` on an API-response struct is a red flag when the API may return the id as
  a number — prefer `json.Number` or a `FlexibleID` unmarshaler. Flag as suggestion unless
  the diff shows the API actually varies.
- J2: Booleans that the API may send as `"true"`/`1` need a flexible unmarshaler.
- J3: Optional fields that may be `null` use pointer types (`*string`), with a nil check at
  use. Non-pointer optional fields are a red flag.
- J4: New custom unmarshalers should have a table-driven `_test.go` covering string,
  number, and null inputs (ties into base "tests for new behavior").

## E. Breaking-Change Process Gate (B1-B9 depth)

connector.md B1-B9 lists *what* is breaking and says breaking changes "should be gated,
called out, and paired with docs." The full gate, when a breaking change is present:

- BP1: Breaking behavior is opt-in behind a config flag — never default-on.
- BP2: PR description explicitly states what breaks and why.
- BP3: `docs/connector.mdx` updated for new scopes / auth / behavior (overlaps D1-D4).
- BP4: A reviewer can answer: does this change an identifier C1 matches on (resource type
  id, entitlement slug, resource id derivation)? what happens to existing grants/resources
  on deploy? is there a migration path (dual-emit, re-sync)?
- BP5: If gated behind a flag, downgrade the finding from blocking to suggestion (it is
  opt-in). An ungated breaking change is blocking-correctness.

## F. Provisioning Depth (P1-P6 reinforcement)

connector.md P1-P6 already states the entity-source rules and idempotency. Reinforce the
two highest-cost mistakes (entity-source confusion has caused 3 production reverts):

- PR1: When `*_actions.go`/`actions.go` or any Grant/Revoke method changed, read the FULL
  file, not just the diff — entity-source correctness needs the whole flow.
- PR2: Context (workspace/org/tenant) comes from `principal.ParentResourceId.Resource`
  (Revoke: `grant.Principal.ParentResourceId.Resource`), NEVER from
  `entitlement.Resource.ParentResourceId`. Grep the diff for
  `entitlement.Resource.ParentResourceId` in Grant/Revoke as a direct detector.
- PR3: P5 — when an API call takes multiple string params, verify argument order against
  the function signature; swapped IDs are easy to miss and silently grant the wrong thing.

## G. ID Stability Nuance (R10 / B3 calibration)

- I1: Email (or other mutable field) as the resource ID is a real problem ONLY when a stable
  immutable API ID exists and is being ignored. If the API offers no stable id, email is
  acceptable — flag as suggestion, not blocking. Changing an existing id derivation from a
  stable field to a mutable one is breaking (B3) and blocking.
