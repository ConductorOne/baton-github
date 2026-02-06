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

## Retryable vs Fatal Errors

| Error Type | Retryable? | Action |
|------------|-----------|--------|
| Rate limit (429) | Yes | SDK retries automatically |
| Network timeout | Yes | SDK retries |
| Server error (5xx) | Yes | SDK retries |
| Bad request (400) | No | Log details, fail |
| Unauthorized (401) | No | Check credentials |
| Forbidden (403) | No | Check permissions |
| Not found (404) | Depends | Often skip, not error |

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

**Wrong - silent failure:**
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
