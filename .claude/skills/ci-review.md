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
retained OTEL error spans. These rules apply to logging the connector writes itself.
`uhttp.BaseHttpClient` classifies HTTP responses on its own, by status class — a description
of SDK behavior the connector does not control, not a rule to copy.

- L1: Upstream 4xx (401/403/404/409/429), OAuth refresh failures, and bad-config init
  failures → `Warn` **when the condition is actionable by the customer**: an expired or
  wrong credential, a missing scope, a misconfigured tenant — something they can go fix.
  They reflect customer config, not connector bugs, so `l.Error(...)` on any of these is
  wrong. Actionable conditions are *usually* one-shot — a bad credential is bad for the
  whole run, so the warning fires once — but not always: a 403 from a per-object ACL the
  customer can grant resource by resource is actionable AND per-resource. That case is
  what L1c is for.
- L1b: A 4xx the connector handles gracefully and the customer can neither act on nor
  care about → `Debug`, not `Warn`. Typical shapes: a 403 on an optional per-resource
  detail endpoint the connector falls back from, a 404 for a record deleted mid-sync, a
  429 the SDK retries for you. Not-actionable **and** per-resource is the expensive
  combination, and it is the one that belongs at Debug.

  Choosing `Debug` here is a decision about noise, never about whether to return the
  error — see section S, which is a blocking rule and outranks every level question in
  this section. In particular, of the shapes listed above only the 404 may be skipped
  inside a resource-producing loop; the 403 and the 429 are `Debug`-worthy when the
  connector has genuinely handled them, but S5 still requires returning them when the
  handling is a bare `continue`.
- L1c: What survives L1b and can still fire per-resource takes L7 sampling on top —
  `Warn` is the right level there, but not thousands of times.
- L1d: These rules govern the connector's own logging only. `uhttp.BaseHttpClient` already
  emits its own unsampled `Warn` for every 4xx response (`base-http-client: HTTP error
  status`), and no connector-side rule can suppress it. So for a connector whose calls go
  through `BaseHttpClient` — most of them — moving your line to `Debug` on a per-resource 4xx
  roughly halves the shipped volume rather than zeroing it. A connector calling a vendor SDK or
  a raw `http.Client` has no such companion line, and there `Debug` does take it to zero.
  Either way the guidance is the same and the SDK's own line is never a finding.
- L2: Upstream 5xx and genuine connector code bugs / impossible states → `Error`. This
  includes a 4xx the connector *caused*: a 400 on a request we built malformed is our bug,
  not customer config, and `l.Error` on it is correct. L1 covers the 4xx the upstream
  raises about the customer's data or credentials — do not match a status code against L1
  and flag a deliberate `Error` on a self-caused 400 as wrong.
- L3: Nil/zero/missing-but-expected values and gracefully-handled unknown enum variants
  → `Debug`, not Warn (e.g. "access key last-used date is nil").
- L4: Skip-and-continue (per-item graceful degradation) is NOT error swallowing — do not
  flag `log + continue` on a single skipped item under H4. Two limits on that suppression:
  it covers `continue` inside a loop only, never `log + return nil` from the method itself
  (that is the destructive shape section S forbids, and this carve-out never licenses it);
  and it covers only a `NotFound` on the item being fetched. A `continue` past a 401, a 403
  or an exhausted 429 is not suppressed — that error might be about every item, and the
  empty page it produces is S1 arriving by a slower route. See S5. It says nothing about *which* level
  to log at: take that from L1/L1b/L1c, which for most per-item degradation means `Debug`,
  not `Warn`. Two separate questions — "is dropping this item a bug?" and "does anyone
  need to see the line?" — and L4 only answers the first.
- L5: Context cancellation (`context.Canceled` / `DeadlineExceeded`) → `Debug`. The
  customer cannot act on a shutdown or a timeout, so L1's question answers this one
  outright; do not leave it as an either/or.
- L6: Do not log at `Error` AND return the same error — the SDK logs returned errors;
  double-logging is noise. A local line + return is fine, and its level comes from
  L1/L1b like any other: `Debug` for something only the developer reads, `Warn` only when
  the customer can act on it.
- L7: A warning that can fire per-resource or per-page MUST use logarithmic sampling
  (occurrences 1, 10, 100, then every 1000) with a `total_occurrences` field. This is not a
  judgement call, and the two halves bind different people: **MUST** for the author writing
  the line, **suggestion** severity for the reviewer flagging it. Flag every unsampled
  recurrent warn, even when the level itself is correct per L1. It is a suggestion because
  an unsampled warn breaks nothing — it only costs — so it should not block a merge, but it
  should never be waved through silently either. Warn-level output is ingested and retained downstream while debug
  is filtered, so an unsampled per-resource warn costs real money on a large sync for lines
  nobody reads. Bounding the volume is what makes L1 affordable. A one-shot warn (once per
  sync, once at connector init) needs no sampling.

The test before writing `l.Error`: is it a connector code bug? does the connector stop?
is it unexpected? If all three point away from Error, one more question picks between the
remaining two: can the customer act on it? If yes, `Warn` — sampled if it recurs. If no,
`Debug`.

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

## S. Sync Data-Loss on Empty Results (blocking)

This is not a log-level rule and it outranks section A. A full sync is authoritative current
state: C1 buckets anything the sync does not emit as **deleted**, for grants exactly as for
resources. Nothing exempts an emitted-empty result from that bucketing — the annotations below
work by keeping C1 from expecting the resource type at all, not by marking a sync partial.

- S1 (**blocking**): a resource-producing method — `List`, `Entitlements` or `Grants` — that
  swallows an error and returns an empty result is a data-loss defect, at any log level.
  An empty `Grants()` silently revokes every principal's access to that resource; an empty
  `List` deletes every resource of that type. Flag it as `blocking-correctness`. The correct
  shape at runtime is to **return the error**: the sync fails and C1 keeps prior state.
  `&v2.OptInRequired{}` is the other half, but it is a *design-time* decision — a static
  resource-type annotation read once at capability registration
  (`connectorbuilder.go` → `annos.Contains(&v2.OptInRequired{})`), for a feature not every
  customer has. A connector cannot reach for it once a sync is running and a call comes
  back 403, so never suggest it as the fix for a live failure.
- S2 (**detector**): in any `List`, `Entitlements` or `Grants` body, look for a `return` of an
  empty or nil collection with a `nil` error on a branch reached from an error check — the
  literal `return nil, "", nil, nil` after a logged `err` is the common form. Read the whole
  method, not the diff hunk: the log line and the return are often several lines apart.
- S3: skipping one item inside a loop with `continue` is the graceful shape and is NOT this
  defect — that is L4, and it stays a suppression, but only for a `NotFound` on that item.
  The distinction is the error, not the count: a vanished record is genuinely absent, while
  any other failure might be about every item on the page. See S5.
- S4: "skip if 403", "return empty if the account lacks permission" and "log a warning and
  continue past a failed page" are the same defect under other names. There is no
  partial-sync signal and no progressive-authorization fallback in C1's sync model.
- S5 (**blocking**): a per-item `continue` that every item takes is S1 wearing a disguise.
  When the cause is systemic — a missing scope, a revoked token, an endpoint that is gone, a
  retired API version, an exhausted rate limit — the loop skips every element, accumulates
  nothing, and returns an empty result, with the per-item log at `Debug` so nothing is
  emitted at all.

  The rule is an allowlist, not a counter: **inside a resource-producing loop, only a
  `NotFound` on the item being fetched may be skipped**, and only when the connector has
  established that this vendor returns 404 for absence. Every other error — 401, 403, an
  exhausted 429, a parse failure — might be about that one item or about all of them, and
  the loop cannot tell, so it must be returned. Flag a `continue` on a non-`NotFound` error
  in `List`/`Entitlements`/`Grants` as `blocking-correctness`.

- S6 (**blocking**): `NotFound` does not universally mean "the record is gone". GitHub,
  GitLab and several SCIM/admin APIs answer 404 for authorization failures so they do not
  leak whether a resource exists, and a retired API version 404s every call on that path. On
  those APIs the S5 allowlist would wave through the exact wipe it exists to block: every
  per-item fetch returns `NotFound`, every one gets skipped, the page comes back empty.

  No rule can settle this from inside the loop — it is a fact about the vendor, so it has to
  be established once and written where the next reader will find it: a doc comment on the
  client method that makes the call, next to the endpoint it documents. Require that note to
  exist, and require the loop's guard to match it: where 404 can mean "not permitted", treat
  it as a 403 and return it.

  **Absent that determination, the safe default is to return, not to skip.** An unproven
  `NotFound` is not skippable. Flag a `status.Code(err) != codes.NotFound` guard whose client
  method carries no doc comment establishing the vendor's 404 semantics — the same place the
  requirement above names, not `docs/connector.mdx` and not the PR description.

  Do **not** accept a skip counter compared against the page size as the bound. `Grants()`
  frequently runs over one or two items — the members of a small group, the bindings on one
  role — and there a single legitimate 404 is "100% of the page", so a ratio bound fails the
  sync on a record that is legitimately gone, which is L1b's own Debug-and-skip case. A
  counter carried on the builder across pages does not work either: in Lambda mode each page
  is a separate invocation, so the state does not survive.