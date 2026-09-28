<!-- This file is managed by baton-admin. DO NOT EDIT. -->
# patterns-error-handling

Error wrapping, prefixes, and distinguishing retryable from fatal errors.

---

## Error Prefix Convention

All errors must include connector name:

```go
return fmt.Errorf("baton-myservice: failed to list users: %w", err)
```

Pattern: `baton-{service}: {action}: %w`

**Why:** When errors surface in logs or UI, operators need to know which connector failed.

---

## Error Wrapping with %w

**Correct - preserves error chain:**
```go
if err != nil {
    return nil, fmt.Errorf("baton-myservice: failed to list users: %w", err)
}
```

**Wrong - breaks error chain:**
```go
if err != nil {
    return nil, fmt.Errorf("baton-myservice: failed to list users: %v", err)
}
```

**Why %w matters:** SDK uses `errors.Is()` and `errors.As()` to detect specific error types like rate limits. Without `%w`, detection fails.

---

## Wrapping Errors with uhttp.WrapErrors

Use `uhttp.WrapErrors` when returning errors from HTTP calls that did **not** go through `uhttp.BaseHttpClient` — for example, SDK library errors, raw `http.Client` calls, or errors you infer yourself (e.g., "status 200 but body indicates failure").

**Why it matters:** The SDK inspects the gRPC status code on errors to decide whether to retry, log, or surface the error to the operator. Without wrapping, the SDK treats all errors as opaque and cannot act appropriately.

**Signature:**
```go
func WrapErrors(preferredCode codes.Code, statusMsg string, errs ...error) error
```

- `preferredCode` — a gRPC status code (from `google.golang.org/grpc/codes`), not an HTTP status code
- `statusMsg` — human-readable message describing the error
- `errs` — original errors to join into the result

**Common gRPC code mappings:**

| Situation | gRPC code |
|-----------|-----------|
| Auth failure (401) | `codes.Unauthenticated` |
| Permission denied (403) | `codes.PermissionDenied` |
| Not found (404) | `codes.NotFound` |
| Rate limited (429) | `codes.ResourceExhausted` |
| Server error (5xx) | `codes.Internal` |

```go
// SDK library error — wrap with appropriate gRPC code:
if err != nil {
    return nil, uhttp.WrapErrors(codes.Internal, "baton-myservice: failed to list users", err)
}

// Developer-inferred error from response body or status code:
if resp.StatusCode == http.StatusForbidden {
    return nil, uhttp.WrapErrors(
        codes.PermissionDenied,
        fmt.Sprintf("baton-myservice: access denied to %s", endpoint),
        fmt.Errorf("HTTP %d", resp.StatusCode),
    )
}
```

**When NOT to use it:** If you're using `uhttp.BaseHttpClient` for all requests, uhttp handles wrapping automatically. Only wrap manually when bypassing uhttp.

---

## Retryable vs Fatal Errors

| Error Type | Retryable? | Action |
|------------|-----------|--------|
| Rate limit (429) | Yes | SDK retries automatically |
| Network timeout | Yes | SDK retries |
| Server error (5xx) | Yes | SDK retries |
| Bad request (400) | No | Return error with context |
| Unauthorized (401) | No | Return error, check credentials |
| Forbidden (403) | No | Return error, check permissions |
| Not found (404) | Depends | Skip only if this vendor's 404 means "gone" rather than "not permitted" — see Rule 4 |

**Log level is deliberately not in this table.** Retryability and log level are different
questions, and answering them together is what produced the blanket "all 4xx → Warn" rule this
guide used to carry. A 5xx is retryable *and* an `Error`; a 429 is retryable and belongs at
`Debug` because the SDK handles it and the customer cannot act on it. See
[Log Level Classification](#log-level-classification) — Rule 1 decides the level for logging you
write, on whether the customer can act, and it is the only place in this file that does.

---

## Error Detection Pattern

```go
func (u *userBuilder) List(ctx context.Context, parentID *v2.ResourceId,
    token *pagination.Token) ([]*v2.Resource, string, annotations.Annotations, error) {

    users, err := u.client.ListUsers(ctx)
    if err != nil {
        // Check for specific error types
        if isRateLimitError(err) {
            // SDK handles retry - just return the error
            return nil, "", nil, err
        }
        if isAuthError(err) {
            // Fatal - clear message for operator
            return nil, "", nil, fmt.Errorf("baton-myservice: authentication failed (check credentials): %w", err)
        }
        // Generic error
        return nil, "", nil, fmt.Errorf("baton-myservice: failed to list users: %w", err)
    }
    // ...
}

func isRateLimitError(err error) bool {
    var httpErr *HTTPError
    if errors.As(err, &httpErr) {
        return httpErr.StatusCode == 429
    }
    return false
}
```

---

## Context Cancellation

Always respect context cancellation:

```go
func (u *userBuilder) List(ctx context.Context, parentID *v2.ResourceId,
    token *pagination.Token) ([]*v2.Resource, string, annotations.Annotations, error) {

    users, err := u.client.ListUsers(ctx)
    if err != nil {
        return nil, "", nil, err
    }

    var resources []*v2.Resource
    for _, user := range users {
        // Check for cancellation in loops
        select {
        case <-ctx.Done():
            return nil, "", nil, ctx.Err()
        default:
        }

        resource, err := createResource(user)
        if err != nil {
            return nil, "", nil, err
        }
        resources = append(resources, resource)
    }

    return resources, "", nil, nil
}
```

**Why:** Cancelled context means "stop now" - user cancelled, timeout reached. Ignoring it wastes quota and causes zombie requests.

---

## Don't Swallow Errors

**Wrong - silent failure with no return:**
```go
users, err := client.ListUsers(ctx)
if err != nil {
    log.Println("error listing users:", err)
    // Continues with empty users - silent data loss!
}
```

**Correct - propagate error:**
```go
users, err := client.ListUsers(ctx)
if err != nil {
    return nil, "", nil, fmt.Errorf("baton-myservice: failed to list users: %w", err)
}
```

**Exception — intentional skip-and-continue:** When a non-fatal per-item error occurs and the
connector intentionally skips *that item* and keeps going, it is not error swallowing — it is
graceful degradation. Skip the item with `continue`; do not abandon the method by returning an
empty result. See [Log Level Classification](#log-level-classification), Rule 4, for both the
level and the reason returning empty is destructive.

The shape is narrower than it looks: only a `NotFound` on the item being fetched may be
skipped. Every other error must be returned, because a systemic one — a missing scope, a
revoked token — skips every item and returns an empty page, which is a wipe rather than
degradation. Rule 4 below carries the one worked example; this section deliberately does
not repeat it.

---

## Partial Success Handling

> **Scope:** this is about a failure that makes the *page* untrustworthy — the list call
> itself failed, or the response is malformed. It is not a counter-instruction to Rule 4,
> which covers one item inside a page failing while the rest are fine. "The item was not
> found" is a `continue`; "the page failed", or any per-item error that is not a `NotFound`,
> is the fail-fast below.

**For sync (fail fast):**
```go
for _, item := range items {
    if err := process(item); err != nil {
        return err  // Stop on first error
    }
}
```

**For provisioning (collect errors):**
```go
var errs []error
for _, item := range items {
    if err := process(item); err != nil {
        errs = append(errs, fmt.Errorf("item %s: %w", item.ID, err))
    }
}
if len(errs) > 0 {
    return errors.Join(errs...)
}
```

---

## Error Message Quality

**Bad - no context:**
```go
return fmt.Errorf("failed")
```

**Bad - redundant "error":**
```go
return fmt.Errorf("error: failed to list users")
```

**Good - specific and actionable:**
```go
return fmt.Errorf("baton-myservice: failed to list users (page %d): %w", page, err)
```

Include:
- Connector name
- Action being performed
- Relevant IDs/context
- Original error via %w

---

## Log Level Classification

**The core rule:** `l.Error()` means "something is broken and needs human attention right now." If the connector can continue operating, it is almost certainly not an Error.

In production, misclassified ERROR logs generate alert noise, inflate OTEL error spans (which are retained permanently), and obscure genuine failures. `uhttp.BaseHttpClient` classifies HTTP responses on its own, by status class (4xx → Warn,
5xx → Error via `GrpcCodeFromHTTPStatus()`). That is a description of SDK behavior you do not
control, not a rule to copy: the rules below govern **your own logging** — anywhere you write
`l.Error(...)`, `l.Warn(...)`, or `l.Debug(...)` — and they cut on actionability instead.

One consequence worth knowing before you move a line to `Debug`: `BaseHttpClient` emits its own
unsampled `Warn` for every 4xx (`base-http-client: HTTP error status`) into the same stream, and
nothing on the connector side suppresses it. So if your calls go through `BaseHttpClient`, a
per-resource 4xx keeps shipping one line whatever you do, and moving yours to `Debug` roughly
halves the volume rather than zeroing it. If you call a vendor SDK or a raw `http.Client` there
is no companion line and `Debug` does take it to zero. Your line is the one you control either
way, so still move it — just do not expect the sync to go quiet on the uhttp path.

---

### Classification Rules

#### Rule 1: Upstream client errors (4xx) → Warn **only if the customer can act on it**

A 4xx that reflects the upstream's view of customer data or configuration is never an
`Error` — it is not a connector bug. (The exception is a 4xx we *caused*: a 400 on a request
the connector built malformed is our bug and belongs at `Error`. See the quick-reference row.)
But "not a connector bug" does not make it worth a retained log line. The level is decided by whether the customer can go fix
the condition:

- **Actionable** — an expired or wrong credential, a missing scope, a misconfigured tenant.
  `Warn`. These are one-shot in practice: the credential is bad for the whole run, so the
  line appears once.
- **Not actionable** — a 403 on an optional per-item call the connector already falls back
  from, a 404 for a record deleted mid-sync, a 429 the SDK retries for you. `Debug`. The
  customer can do nothing with it and it fires once per resource.

```go
// WRONG — alerts on a customer config issue
l.Error("failed to assume role", zap.String("account", accountID), zap.Error(err))

// CORRECT LEVEL — the customer can grant the role in that account, so they can act.
// Not yet complete: this fires once per account, so it also needs the logarithmic
// sampling from the pattern below. Copy that version, not this one.
l.Warn("failed to assume role", zap.String("account", accountID), zap.Error(err))
```

That example is also *per-account*, so it is actionable AND recurring — see the logarithmic
sampling pattern below, which is required for this shape, not optional.

Why the cut is drawn at actionability rather than at the status class: `log-level` defaults
to `info`, so a `Debug` line is not a cheaper log — it is **never emitted**, zero bytes
shipped, and `log-level-debug-expires-at` turns it back on for a bounded window when someone
actually needs to look. A `Warn` firing once per resource ships thousands of retained lines
per sync for something nobody reads. (The SDK's own per-4xx `Warn` rides on top of that and is
outside your control — see the note above.)

In plain `baton` repos this rule matches `ci-review.md` L1 / L1b / L1c, which the PR reviewer
applies to your code. `baton-http` repos get the same skill docs but a `ci-review.md` scoped to
declarative config, with no log-level rules — there, this file is the only statement of them.

#### Rule 2: Upstream server errors (5xx) → Error

A 5xx from the upstream API may indicate a genuine problem. Keep these at Error.

```go
l.Error("upstream server error", zap.Int("status", resp.StatusCode), zap.Error(err))
```

#### Rule 3: Expected/normal data states → Debug

Nil, zero, or missing values that are part of normal operation are not warnings. Unknown enum variants that the connector handles gracefully are not warnings either.

```go
// WRONG — fires for every unused access key
l.Error("access key last used date is nil or zero", zap.String("key_id", keyID))

// WRONG — still noisy
l.Warn("access key last used date is nil or zero", zap.String("key_id", keyID))

// CORRECT — expected for keys that have never been used
l.Debug("access key has no last-used date", zap.String("key_id", keyID))
```

Other examples: duplicate user entries across rotations, unknown permission scopes when an API adds new ones, optional fields missing from responses.

#### Rule 4: Skip-and-continue (graceful degradation) → usually Debug, and never by returning empty

Skipping one item and continuing is a legitimate shape, and it is **not** error swallowing —
do not treat a `log + continue` on a single item as a swallowed error. Two things about it
are easy to get wrong.

**The level.** Apply Rule 1. Most per-item degradation is something the customer cannot act
on, so it belongs at `Debug`, not `Warn`. Reserve `Warn` for the case they can actually fix,
and sample it if it recurs.

**What you return.** Skipping an *item* inside a loop is fine. Abandoning the *method* by
returning an empty result is not, and this is the mistake worth remembering:

```go
// CATASTROPHIC — not a logging problem. This swallows the error and reports
// "this role has no grants" to C1. The sync backend buckets grants exactly like
// resources: anything a full sync does not emit is treated as deleted. So every
// grant on that role becomes a deleted grant, and every principal holding it
// silently loses access.
l.Warn("failed to get role details, skipping grants for this role",
    zap.String("role_name", roleName), zap.Error(err))
return nil, "", nil, nil

// CORRECT — skip only the vanished record; return anything else
var grants []*v2.Grant
for _, role := range roles {
    details, err := c.GetRoleDetails(ctx, role.Name)
    if err != nil {
        // Only a vanished record is safely skippable: emitting without it IS the
        // correct state. Anything else — a missing scope, a revoked token, an
        // exhausted rate limit — could be systemic, and skipping it would return
        // an empty page that C1 reads as deletions.
        //
        // This guard is only valid because this vendor documents 404 as absence.
        // GitHub, GitLab and several SCIM APIs answer 404 for permission failures
        // instead, and a retired API version 404s every call — there, 404 is a 403
        // and belongs on the return path. Establish which it is before copying this,
        // and record it in a doc comment on the client method that makes the call.
        if status.Code(err) != codes.NotFound {
            return nil, "", nil, fmt.Errorf("baton-myservice: listing role grants: %w", err)
        }
        // Not actionable by the customer, and fires once per role → Debug.
        l.Debug("role not found, skipping", zap.String("role_name", role.Name))
        continue
    }
    grants = append(grants, buildGrants(details)...)
}
// ... nextPageToken comes from the pagination bag as usual
return grants, nextPageToken, nil, nil
```

The alternative, when the failure means the whole page is untrustworthy rather than one item:
return the error instead of a partial page. The sync fails, C1 keeps the state it already
has, and nothing is deleted.

```go
if err != nil {
    return nil, "", nil, fmt.Errorf("baton-myservice: listing role grants: %w", err)
}
```

The rule of thumb: dropping one item out of many is a judgement call about data quality;
returning empty for the whole method is a decision to delete data in C1. Only the first is
graceful degradation — and it stops being graceful once "many" is "all of them". A skip
whose cause is systemic rather than incidental fails every iteration and lands you in the
second case by a slower route.

Rather than measure that after the fact, decide it per error: **only a vanished record is
safely skippable — and only once you know that this vendor's 404 means "gone" rather than
"not permitted".** GitHub and GitLab return 404 for authorization failures by design, so on
those APIs a bare NotFound guard skips every item a missing scope touches and produces the
empty page it was written to prevent. Settle the vendor's 404 semantics at research time,
write it in the client layer, and match the guard to it; absent that, return rather than
skip. A 404 on the item you are fetching means emitting without it is the
correct state. A 401, a 403 or an exhausted 429 might be about that one item or might be
about every item, and you cannot tell from inside the loop — so return it. Counting skips
and comparing against the page size does not work: `Grants()` often runs over one or two
items, where a single legitimate 404 is "100% of the page".

#### Rule 5: Context cancellation → Debug

Context cancellation (`context.Canceled`, `context.DeadlineExceeded`) is normal during Temporal workflow shutdown, user cancellation, or timeout. It is not an error, and it is not actionable by the customer either — Rule 1's question settles it at `Debug`, so this is not an either/or.

```go
if ctx.Err() != nil {
    l.Debug("context canceled, stopping sync", zap.Error(ctx.Err()))
    return nil, "", nil, ctx.Err()
}
```

#### Rule 6: Error already propagated to caller → avoid double-logging at Error

If you return an error to the SDK (which will log it), do not also log it at Error level yourself. This creates duplicate noise. If you want local visibility, the level still follows Rule 1: `Debug` for a line only you will read, `Warn` only if the customer can act on it.

```go
// WRONG — logged at Error here AND by the SDK when it receives the returned error
l.Error("failed getting metadata", zap.Error(err))
return nil, fmt.Errorf("baton-myservice: failed getting metadata: %w", err)

// CORRECT — return the error, let SDK handle logging
return nil, fmt.Errorf("baton-myservice: failed getting metadata: %w", err)

// ALSO OK — a local line for your own debugging while the SDK logs the returned
// error. Nobody but you acts on it, so Debug (Rule 1), not Warn.
l.Debug("failed getting metadata", zap.Error(err))
return nil, fmt.Errorf("baton-myservice: failed getting metadata: %w", err)
```

---

### Quick Reference Table

| Situation | Level | Rationale |
|-----------|-------|-----------|
| Upstream 401/403 that blocks the sync (bad credential, missing scope) | **Warn** | Customer can fix it, and it fires once |
| Upstream 403 on an optional per-item call the connector falls back from | **Debug** | Customer cannot act; fires per resource |
| Upstream 403 on a per-object ACL the customer can grant individually | **Warn, sampled** | Actionable *and* recurring — see sampling below |
| Upstream 404 (record deleted mid-sync) | **Debug** | Nothing to act on |
| Upstream 429 (rate limit) | **Debug** | SDK retries it; the customer cannot do anything |
| Upstream 400 (bad request) | **Error** if the connector built the request, **Warn** if customer data caused it | A malformed request we constructed is our bug |
| Upstream 5xx (server error) | **Error** | Genuine upstream failure |
| OAuth token refresh failure | **Warn** | Customer credential issue, fires once |
| Connector init failure (bad config) | **Warn** | Operator config issue, not a code bug |
| Skip a `NotFound` item inside a loop + continue | **Debug** | Graceful degradation the customer cannot act on |
| `continue` past any other error in List/Entitlements/Grants | **never** | Could be systemic; an all-skipped page reads as deletions (Rule 4) |
| Return empty from List/Entitlements/Grants after an error | **never** | Not a log-level question — C1 reads it as deletions |
| Nil/zero/empty expected values | **Debug** | Normal case (e.g., unused key, optional field) |
| Unknown enum variant, handled gracefully | **Debug** | API added new values, connector skips |
| Duplicate entry, handled gracefully | **Debug** | Expected in multi-source data |
| Context canceled / deadline exceeded | **Debug** | Normal shutdown path |
| Connector code bug / impossible state | **Error** | Needs developer attention |
| Data corruption / invariant violation | **Error** | Needs developer attention |
| Upstream 5xx from direct HTTP client | **Error** | Server-side failure |

---

### The Test

Before writing `l.Error(...)`, ask these three questions:

1. **Is this a connector code bug?** If no (it's upstream, config, or expected), do not use Error.
2. **Does the connector continue running?** If yes (skip + continue), not Error — question 4
   decides which of the other two.
3. **Is this expected in normal operation?** If yes (nil dates, unknown enums, duplicates), use Debug.

If all three answers point away from Error, one more question decides between Warn and Debug:

4. **Can the customer act on it?** If yes, `Warn` — and if it can also fire per resource,
   sample it. If no, `Debug`: at the default `info` level that line is never emitted, and
   nobody was going to act on it anyway.

---

### Pattern: logError Helper for Connectors with Custom HTTP Clients

If your connector makes HTTP calls outside of `uhttp.BaseHttpClient` (e.g., using a vendor SDK or raw `http.Client`), add a `logError` helper that classifies by gRPC status code. The baton-sdk exports `uhttp.GrpcCodeFromHTTPStatus()` to help with this.

```go
// logError keeps server-class gRPC errors at Error. Client-class errors are never
// Error, but the level is the caller's call: pass Warn when the customer can act on
// it, Debug when they cannot (Rule 1). A helper cannot know which it is.
func logError(l *zap.Logger, err error, clientLevel zapcore.Level, msg string, fields ...zap.Field) {
    clientCodes := map[codes.Code]bool{
        codes.InvalidArgument:  true,
        codes.NotFound:         true,
        codes.AlreadyExists:    true,
        codes.PermissionDenied: true,
        codes.Unauthenticated:  true,
        codes.FailedPrecondition: true,
        codes.OutOfRange:       true,
        codes.Unimplemented:    true,
        codes.Canceled:         true,
        codes.ResourceExhausted: true,
    }

    fields = append(fields, zap.Error(err))
    if s, ok := status.FromError(err); ok && clientCodes[s.Code()] {
        l.Log(clientLevel, msg, fields...)
    } else {
        l.Error(msg, fields...)
    }
}

// Caller decides: a record that vanished mid-sync is not actionable.
// (Not a rate limit — outside BaseHttpClient nothing retries it for you.)
logError(l, err, zapcore.DebugLevel, "role not found, skipping")
// ...but a credential the customer must rotate is.
logError(l, err, zapcore.WarnLevel, "token refresh failed")
```

For direct HTTP responses without gRPC wrapping, branch on status code:

```go
// Same rule as the helper: the status class decides Error vs not, the caller
// decides Warn vs Debug. Keep it a function so the level is a real parameter.
func logStatus(l *zap.Logger, statusCode int, clientLevel zapcore.Level, msg string, err error) {
    fields := []zap.Field{zap.Int("status", statusCode), zap.Error(err)}
    if statusCode >= 500 {
        l.Error(msg, fields...)
        return
    }
    l.Log(clientLevel, msg, fields...)
}

// A record that vanished mid-sync is nothing the customer can act on — see Rule 1.
// (Not a 429: on this path there is no SDK retry to lean on.)
logStatus(l, resp.StatusCode, zapcore.DebugLevel, "record not found, skipping", err)
// A rejected credential is.
logStatus(l, resp.StatusCode, zapcore.WarnLevel, "request unauthorized", err)
```

**Both samples answer "is it Error?", not "is it Warn?".** A client-side status or code means
*not* `Error` — it does not mean the line is worth shipping. A 404 for a record that vanished
mid-sync is the usual case: client-class, and nothing the customer can act on. Note that this
section is for calls made *outside* `uhttp.BaseHttpClient`, so "the SDK retries it" — the reason
a 429 is `Debug` elsewhere in this file — does not apply here. That removes the reason, not the
answer: run a 429 on this path back through Rule 1 rather than through the retry. Sustained
throttling after your retries are exhausted *is* actionable — the operator can lower concurrency
or raise the vendor quota — so `Warn`, sampled if it recurs. If instead you return the error and
let the SDK log it, Rule 6 applies and the local line is `Debug`. That is why neither sample
hardcodes `Warn`; both take the non-`Error` level from the caller, who is the only one who knows
whether the customer can act.

---

### Pattern: Logarithmic Sampling for High-Volume Warnings

When a warning is **actionable** (Rule 1) but can still fire many times, use logarithmic
sampling to keep it visible without flooding. A failure the customer cannot act on does not get
sampled — it gets `Debug`.

The example below lives in the **client layer**, not in a resource-producing loop. That is
deliberate: inside `List`/`Entitlements`/`Grants` a repeated failure is not something to sample
and skip past — Rule 4 says return it — so the sampling pattern belongs where a warning can
recur without dropping synced data.

```go
type client struct {
    http              *uhttp.BaseHttpClient
    deprecatedAPICount atomic.Int64
}

// On each response carrying a deprecation header:
count := c.deprecatedAPICount.Add(1)
if count == 1 || count == 10 || count == 100 || count%1000 == 0 {
    l.Warn("vendor API version is deprecated; upgrade before the sunset date",
        zap.String("sunset", sunsetHeader),
        zap.Int64("total_occurrences", count),
    )
}
```

**The counter is per process, not per sync.** In Lambda mode each page is a separate
invocation, so it restarts with every page: sampling bounds the lines within one invocation,
not across the whole sync. That is still the difference between a handful of lines per page and
one per item, but do not read "1, 10, 100, every 1000" as a whole-sync total. It is also why a
skip counter cannot serve as a data-loss bound (in plain `baton` repos, `ci-review.md` S5; `baton-http` repos have no such rule, so this file is the statement of it).

Always include a `total_occurrences` field so operators can see the true scale even when most log lines are suppressed.

This pattern is **required**, not optional, for any warning that can fire per resource or per
page. In plain `baton` repos `ci-review.md` L7 makes it a review finding when it is missing,
even where Rule 1 makes `Warn` the correct level; `baton-http` repos have no such rule, so
treat this section as the requirement. Sampling takes ~5000 lines down to ~8. If the condition is not
actionable in the first place, do not sample it — move it to `Debug` and ship nothing.
