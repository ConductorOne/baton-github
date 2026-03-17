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

| Error Type | Retryable? | Action | Log Level |
|------------|-----------|--------|-----------|
| Rate limit (429) | Yes | SDK retries automatically | Warn |
| Network timeout | Yes | SDK retries | Warn |
| Server error (5xx) | Yes | SDK retries | Error |
| Bad request (400) | No | Return error with context | Warn |
| Unauthorized (401) | No | Return error, check credentials | Warn |
| Forbidden (403) | No | Return error, check permissions | Warn |
| Not found (404) | Depends | Often skip, not error | Warn or Debug |

**Note:** All 4xx responses are logged at **Warn**, not Error — they reflect client/config issues, not connector bugs. See [Log Level Classification](#log-level-classification) for the full rules.

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

**Exception — intentional skip-and-continue:** When a non-fatal per-item error occurs and the connector intentionally skips that item, logging at Warn and returning nil is acceptable. This is *not* error swallowing — it is graceful degradation. See [Log Level Classification](#log-level-classification), Rule 4.

```go
roleResp, err := iamClient.GetRole(ctx, &iam.GetRoleInput{RoleName: &roleName})
if err != nil {
    l.Warn("failed to get role details, skipping grants for this role",
        zap.String("role_name", roleName), zap.Error(err))
    return nil, "", nil, nil  // intentional skip — not error swallowing
}
```

---

## Partial Success Handling

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

In production, misclassified ERROR logs generate alert noise, inflate OTEL error spans (which are retained permanently), and obscure genuine failures. The baton-sdk's `uhttp.BaseHttpClient` already classifies HTTP responses correctly (4xx → Warn, 5xx → Error via `GrpcCodeFromHTTPStatus()`). These rules apply to **your own logging** in connector code — anywhere you write `l.Error(...)`, `l.Warn(...)`, or `l.Debug(...)`.

---

### Classification Rules

#### Rule 1: Upstream client errors (4xx) → Warn

Upstream 400, 401, 403, 404, 409, 429 responses reflect client configuration, customer permissions, or expected conditions — not connector bugs.

```go
// WRONG — alerts on customer config issues
l.Error("failed to assume role", zap.String("account", accountID), zap.Error(err))

// CORRECT
l.Warn("failed to assume role", zap.String("account", accountID), zap.Error(err))
```

This includes: auth failures, permission denied, not found, rate limits, bad requests, OAuth token refresh failures, and connector initialization errors caused by invalid credentials or config.

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

#### Rule 4: Skip-and-continue (graceful degradation) → Warn

When a non-fatal error causes the connector to skip an item and continue syncing, log at Warn — not Error. The connector is working as designed; it just has incomplete data for one item.

```go
// WRONG — fires per-item, pollutes dashboards
l.Error("failed to get role details, skipping grants for this role",
    zap.String("role_name", roleName), zap.Error(err))
return nil, "", nil, nil

// CORRECT — visible but not alerting
l.Warn("failed to get role details, skipping grants for this role",
    zap.String("role_name", roleName), zap.Error(err))
return nil, "", nil, nil
```

#### Rule 5: Context cancellation → Debug or Warn

Context cancellation (`context.Canceled`, `context.DeadlineExceeded`) is normal during Temporal workflow shutdown, user cancellation, or timeout. It is not an error.

```go
if ctx.Err() != nil {
    l.Debug("context canceled, stopping sync", zap.Error(ctx.Err()))
    return nil, "", nil, ctx.Err()
}
```

#### Rule 6: Error already propagated to caller → avoid double-logging at Error

If you return an error to the SDK (which will log it), do not also log it at Error level yourself. This creates duplicate noise. If you want local visibility, use Warn or Debug.

```go
// WRONG — logged at Error here AND by the SDK when it receives the returned error
l.Error("failed getting metadata", zap.Error(err))
return nil, fmt.Errorf("baton-myservice: failed getting metadata: %w", err)

// CORRECT — return the error, let SDK handle logging
return nil, fmt.Errorf("baton-myservice: failed getting metadata: %w", err)

// ALSO OK — local Warn for debugging, SDK logs the returned error
l.Warn("failed getting metadata", zap.Error(err))
return nil, fmt.Errorf("baton-myservice: failed getting metadata: %w", err)
```

---

### Quick Reference Table

| Situation | Level | Rationale |
|-----------|-------|-----------|
| Upstream 401/403 (auth/permission) | **Warn** | Customer config, not a connector bug |
| Upstream 404 (not found) | **Warn** | Resource deleted or doesn't exist |
| Upstream 429 (rate limit) | **Warn** | Transient, SDK retries automatically |
| Upstream 400 (bad request) | **Warn** | Bad input data or config |
| Upstream 5xx (server error) | **Error** | Genuine upstream failure |
| OAuth token refresh failure | **Warn** | Customer credential issue |
| Connector init failure (bad config) | **Warn** | Operator config issue, not a code bug |
| Skip item + continue | **Warn** | Graceful degradation, partial data |
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
2. **Does the connector continue running?** If yes (skip + continue), use Warn or Debug.
3. **Is this expected in normal operation?** If yes (nil dates, unknown enums, duplicates), use Debug.

If all three answers point away from Error, use Warn or Debug.

---

### Pattern: logError Helper for Connectors with Custom HTTP Clients

If your connector makes HTTP calls outside of `uhttp.BaseHttpClient` (e.g., using a vendor SDK or raw `http.Client`), add a `logError` helper that classifies by gRPC status code. The baton-sdk exports `uhttp.GrpcCodeFromHTTPStatus()` to help with this.

```go
// logError logs at Warn for client-class gRPC errors, Error for server-class.
func logError(l *zap.Logger, err error, msg string, fields ...zap.Field) {
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
        l.Warn(msg, fields...)
    } else {
        l.Error(msg, fields...)
    }
}
```

For direct HTTP responses without gRPC wrapping, branch on status code:

```go
if resp.StatusCode >= 500 {
    l.Error("request failed", zap.Int("status", resp.StatusCode), zap.Error(err))
} else {
    l.Warn("request failed", zap.Int("status", resp.StatusCode), zap.Error(err))
}
```

---

### Pattern: Logarithmic Sampling for High-Volume Warnings

When a warning can fire thousands of times per sync (e.g., once per resource), use logarithmic sampling to avoid log flooding while preserving visibility:

```go
var getRoleErrorCount atomic.Int64

// In your Grants() method:
count := r.getRoleErrorCount.Add(1)
if count == 1 || count == 10 || count == 100 || count%1000 == 0 {
    l.Warn("failed to get role details, skipping grants",
        zap.String("role_name", roleName),
        zap.Int64("total_occurrences", count),
        zap.Error(err),
    )
}
```

Always include a `total_occurrences` field so operators can see the true scale even when most log lines are suppressed. Use this pattern when a single sync can produce 1000+ identical warnings.
