# review-common-bugs

Top bug patterns from production connector experience.

---

## #1: Entity Source Confusion (3 production reverts)

**The bug:** Getting workspace/org/tenant from entitlement instead of principal in Grant/Revoke.

```go
// WRONG - caused production reverts
func (g *groupBuilder) Grant(ctx context.Context, principal *v2.Resource,
    entitlement *v2.Entitlement) ([]*v2.Grant, annotations.Annotations, error) {

    // BUG: workspace from entitlement, not principal
    workspaceID := entitlement.Resource.ParentResourceId.Resource

    // This grants in the WRONG workspace
    err := g.client.AddMember(ctx, workspaceID, groupID, userID)
}
```

**The fix:**
```go
// CORRECT
workspaceID := principal.ParentResourceId.Resource  // From principal
```

**Why it happens:** Both principal and entitlement have `ParentResourceId`. Developers grab the wrong one.

**Detection:** Search for `entitlement.Resource.ParentResourceId` in Grant/Revoke methods.

---

## #2: Pagination Early Termination (most common)

**The bug:** Always returning empty next token, stopping after first page.

```go
// WRONG - common copy-paste error
func (u *userBuilder) List(ctx context.Context, parentID *v2.ResourceId,
    token *pagination.Token) ([]*v2.Resource, string, annotations.Annotations, error) {

    users, err := u.client.ListUsers(ctx)
    if err != nil {
        return nil, "", nil, err
    }

    // BUG: Always returns "", even when more pages exist
    return resources, "", nil, nil
}
```

**The fix:**
```go
// CORRECT - use API's pagination info
users, nextToken, err := u.client.ListUsers(ctx, currentToken)
// ...
return resources, nextToken, nil, nil  // Pass through API's token
```

**Why it happens:** Developers copy boilerplate and forget to wire up pagination.

**Detection:** Look for `return resources, "", nil, nil` with no conditional logic.

---

## #3: HTTP Response Nil Pointer (13 panic fixes)

**The bug:** Accessing `resp.Body` or `resp.StatusCode` when `resp` might be nil.

```go
// WRONG - panics on network errors
resp, err := client.Do(req)
if err != nil {
    log.Printf("Error: %v, Status: %d", err, resp.StatusCode)  // PANIC
    return err
}
defer resp.Body.Close()
```

**The fix:**
```go
// CORRECT
resp, err := client.Do(req)
if err != nil {
    if resp != nil {
        defer resp.Body.Close()
        log.Printf("Error: %v, Status: %d", err, resp.StatusCode)
    }
    return fmt.Errorf("request failed: %w", err)
}
defer resp.Body.Close()
```

**Why it happens:** On network errors (timeout, DNS failure), Go's http.Client returns error AND nil response.

**Detection:** Search for `resp.StatusCode` or `resp.Body` in error handling paths.

---

## #4: Type Assertion Panics

**The bug:** Direct type assertions without ok check.

```go
// WRONG - panics if missing or wrong type
userID := data["user_id"].(string)
```

**The fix:**
```go
// CORRECT
userID, ok := data["user_id"].(string)
if !ok {
    return fmt.Errorf("user_id missing or not string")
}
```

**Why it happens:** Quick coding, assuming API always returns expected shape.

**Detection:** Regex `\.\([A-Za-z]+\)` without `ok` on same line.

---

## #5: Error Swallowing

**The bug:** Logging errors but continuing execution.

```go
// WRONG - silent data loss
users, err := client.ListUsers(ctx)
if err != nil {
    log.Println("error listing users:", err)
    // Continues with empty users!
}
for _, user := range users {
    // ...
}
```

**The fix:**
```go
// CORRECT
users, err := client.ListUsers(ctx)
if err != nil {
    return nil, "", nil, fmt.Errorf("baton-myservice: failed to list users: %w", err)
}
```

**Why it happens:** Developers want to "handle" errors gracefully but create silent failures.

**Detection:** Look for `log.Print` followed by no return in error blocks.

---

## #6: Missing Error Prefix

**The bug:** Errors without connector name prefix.

```go
// WRONG - hard to trace in logs
return fmt.Errorf("failed to list users: %w", err)
```

**The fix:**
```go
// CORRECT
return fmt.Errorf("baton-myservice: failed to list users: %w", err)
```

**Why it happens:** Copy-paste without updating prefix.

**Detection:** Grep for `fmt.Errorf("failed` without `baton-` prefix.

---

## #7: Wrong Error Verb (%v vs %w)

**The bug:** Using `%v` instead of `%w` breaks error chain.

```go
// WRONG - breaks errors.Is() and errors.As()
return fmt.Errorf("baton-myservice: failed: %v", err)
```

**The fix:**
```go
// CORRECT - preserves error chain
return fmt.Errorf("baton-myservice: failed: %w", err)
```

**Why it happens:** `%v` is common for logging, `%w` is specific to error wrapping.

**Detection:** Search for `fmt.Errorf.*%v.*err` patterns.

---

## #8: Defer Before Error Check

**The bug:** Placing defer before checking if value is valid.

```go
// WRONG - panics if resp is nil
resp, err := client.Do(req)
defer resp.Body.Close()  // PANIC on error
if err != nil {
    return err
}
```

**The fix:**
```go
// CORRECT
resp, err := client.Do(req)
if err != nil {
    return err
}
defer resp.Body.Close()  // After error check
```

**Why it happens:** Muscle memory from other languages, or copy-paste errors.

**Detection:** Look for `defer .*Close()` before `if err != nil`.

---

## #9: Unstable Resource IDs

**The bug:** Using mutable fields as resource ID.

```go
// WRONG - email can change
rs.NewUserResource(user.Name, userType, user.Email, ...)
```

**The fix:**
```go
// CORRECT - use stable API ID
rs.NewUserResource(user.Name, userType, user.ID, ...)
```

**Why it happens:** Email seems like a good unique identifier, but it's mutable.

**Detection:** Look for `.Email` as third argument to `NewUserResource`.

---

## #10: Hardcoded API URLs

**The bug:** Hardcoding base URL prevents testing.

```go
// WRONG - can't point at mock server
const baseURL = "https://api.service.com"
```

**The fix:**
```go
// CORRECT - configurable for testing
var BaseURLField = field.StringField(
    "base-url",
    field.WithDescription("Override API base URL (for testing)"),
    field.WithDefaultValue("https://api.service.com"),
)
```

**Why it happens:** Developers focus on production, forget testing.

**Detection:** Look for `const.*URL` or `http://` / `https://` literals in client code.

---

## Bug Frequency Summary

| Bug | Frequency | Severity | Detection Difficulty |
|-----|-----------|----------|---------------------|
| Entity source confusion | Medium | Critical | Hard (logic error) |
| Pagination termination | High | High | Easy (pattern match) |
| HTTP nil pointer | High | Critical | Medium |
| Type assertion panic | Medium | High | Easy (regex) |
| Error swallowing | Medium | High | Medium |
| Missing error prefix | High | Low | Easy |
| Wrong error verb | Medium | Medium | Easy |
| Defer before check | Low | Critical | Easy |
| Unstable IDs | Low | High | Medium |
| Hardcoded URLs | Medium | Low | Easy |

---

## Automated Detection

Add these to CI or code review automation:

```bash
# Pagination termination
grep -r 'return.*"",.*nil,.*nil' --include="*.go"

# HTTP nil pointer risk
grep -r 'resp\.StatusCode\|resp\.Body' --include="*.go" | grep -v "if resp != nil"

# Missing error prefix
grep -r 'fmt\.Errorf("failed' --include="*.go" | grep -v "baton-"

# Wrong error verb
grep -r 'fmt\.Errorf.*%v.*err' --include="*.go"

# Type assertion without ok
grep -rE '\.\([A-Za-z]+\)[^,]' --include="*.go" | grep -v ", ok :="
```
