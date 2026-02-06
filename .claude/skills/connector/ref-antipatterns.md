# ref-antipatterns

What NOT to do when building connectors.

---

## Critical Antipatterns (Will Cause Production Issues)

### Logging Secrets

```go
// NEVER DO THIS
log.Printf("Authenticating with API key: %s", apiKey)
log.Printf("Request: %+v", req)  // May contain auth headers
```

**Why it's bad:** Credentials end up in logs, which end up in monitoring systems, which end up in security incidents.

**Do instead:**
```go
log.Printf("Authenticating with API key: %s...", apiKey[:8])  // Truncate
// Or better: don't log credentials at all
```

---

### Buffering All Data in Memory

```go
// NEVER DO THIS
func (u *userBuilder) List(ctx context.Context, ...) ([]*v2.Resource, ...) {
    allUsers := []User{}
    for page := 1; ; page++ {
        users, _ := client.ListUsers(page)
        allUsers = append(allUsers, users...)  // Memory grows unbounded
        if len(users) == 0 {
            break
        }
    }
    // Process all users
    return convertAll(allUsers), "", nil, nil
}
```

**Why it's bad:** For large organizations (100k+ users), this OOMs.

**Do instead:**
```go
func (u *userBuilder) List(ctx context.Context, parentID *v2.ResourceId,
    token *pagination.Token) ([]*v2.Resource, string, annotations.Annotations, error) {

    page := extractPage(token)
    users, nextPage, _ := client.ListUsers(page)

    resources := make([]*v2.Resource, 0, len(users))
    for _, user := range users {
        r, _ := convertUser(user)
        resources = append(resources, r)
    }

    return resources, nextPage, nil, nil  // SDK handles checkpointing
}
```

---

### Ignoring Context Cancellation

```go
// NEVER DO THIS
func (u *userBuilder) List(ctx context.Context, ...) {
    users, _ := client.ListUsers()  // ctx not passed
    for _, user := range users {
        // Long processing, ignores ctx.Done()
    }
}
```

**Why it's bad:** Cancelled context means "stop now." Ignoring it wastes resources and causes zombie operations.

**Do instead:**
```go
func (u *userBuilder) List(ctx context.Context, ...) {
    users, err := client.ListUsers(ctx)  // Pass ctx
    if err != nil {
        return nil, "", nil, err
    }

    for _, user := range users {
        select {
        case <-ctx.Done():
            return nil, "", nil, ctx.Err()
        default:
        }
        // Process user
    }
}
```

---

### Swallowing Errors

```go
// NEVER DO THIS
users, err := client.ListUsers(ctx)
if err != nil {
    log.Println("error:", err)
    // Continues with empty users - SILENT DATA LOSS
}
```

**Why it's bad:** Sync reports success but data is incomplete.

**Do instead:**
```go
users, err := client.ListUsers(ctx)
if err != nil {
    return nil, "", nil, fmt.Errorf("baton-service: failed to list users: %w", err)
}
```

---

## High-Risk Antipatterns

### Hardcoded URLs

```go
// BAD
const baseURL = "https://api.service.com"

func NewClient(apiKey string) *Client {
    return &Client{baseURL: baseURL, apiKey: apiKey}
}
```

**Why it's bad:** Can't test against mock servers.

**Do instead:**
```go
// GOOD - configurable base URL
func NewClient(baseURL, apiKey string) *Client {
    if baseURL == "" {
        baseURL = "https://api.service.com"
    }
    return &Client{baseURL: baseURL, apiKey: apiKey}
}
```

---

### Unchecked Type Assertions

```go
// BAD - panics if wrong type
userID := data["user_id"].(string)
count := data["count"].(int)
```

**Do instead:**
```go
// GOOD - safe extraction
userID, ok := data["user_id"].(string)
if !ok {
    return fmt.Errorf("user_id not a string")
}
```

---

### Infinite Pagination Loops

```go
// BAD - hardcoded token causes infinite loop
func (u *userBuilder) List(...) (...) {
    // ...
    return resources, "next", nil, nil  // Always returns "next"
}
```

**Do instead:**
```go
// GOOD - token from API response
if resp.HasMore {
    return resources, resp.NextCursor, nil, nil
}
return resources, "", nil, nil  // Empty token ends pagination
```

---

### Using Email as Resource ID

```go
// BAD - email can change
rs.NewUserResource(user.Name, userType, user.Email, opts)
```

**Why it's bad:** If user changes email, C1 sees it as a new user.

**Do instead:**
```go
// GOOD - stable ID
rs.NewUserResource(user.Name, userType, user.ID, opts)
```

---

## Medium-Risk Antipatterns

### Missing Error Context

```go
// BAD
return fmt.Errorf("failed")
return fmt.Errorf("error: %w", err)
```

**Do instead:**
```go
// GOOD
return fmt.Errorf("baton-service: failed to list users (page %d): %w", page, err)
```

---

### Breaking Error Chain

```go
// BAD - %v breaks errors.Is/As
return fmt.Errorf("failed: %v", err)
```

**Do instead:**
```go
// GOOD - %w preserves chain
return fmt.Errorf("failed: %w", err)
```

---

### defer Before Error Check

```go
// BAD - panics if resp is nil
resp, err := client.Do(req)
defer resp.Body.Close()
if err != nil {
    return err
}
```

**Do instead:**
```go
// GOOD
resp, err := client.Do(req)
if err != nil {
    return err
}
defer resp.Body.Close()
```

---

### Nil Pointer in Error Path

```go
// BAD - resp may be nil
resp, err := client.Do(req)
if err != nil {
    log.Printf("status: %d", resp.StatusCode)  // PANIC
    return err
}
```

**Do instead:**
```go
// GOOD
resp, err := client.Do(req)
if err != nil {
    if resp != nil {
        log.Printf("status: %d", resp.StatusCode)
    }
    return err
}
```

---

## Low-Risk Antipatterns (Code Quality)

### Over-Fetching API Data

```go
// BAD - fetching more than needed
users, _ := client.ListUsersWithAllDetails(ctx)  // Slow, unnecessary
```

**Do instead:**
```go
// GOOD - fetch only what's needed
users, _ := client.ListUsersBasic(ctx)
```

---

### Copy-Paste Resource Builders

```go
// BAD - duplicated code with slight variations
func (u *userBuilder) List(...) {
    // 100 lines of code
}
func (g *groupBuilder) List(...) {
    // 95 lines of nearly identical code
}
```

**Do instead:**
```go
// GOOD - extract common patterns
func (u *userBuilder) List(...) {
    return listResources(ctx, token, u.client.ListUsers, convertUser)
}
func (g *groupBuilder) List(...) {
    return listResources(ctx, token, g.client.ListGroups, convertGroup)
}
```

---

### Magic Numbers

```go
// BAD
client.ListUsers(100)  // What is 100?
time.Sleep(5 * time.Second)  // Why 5?
```

**Do instead:**
```go
// GOOD
const defaultPageSize = 100
client.ListUsers(defaultPageSize)

const rateLimitBackoff = 5 * time.Second
time.Sleep(rateLimitBackoff)
```

---

### Unused Imports and Variables

```go
// BAD - triggers linter warnings
import (
    "unused/package"
)

func foo() {
    x := computeSomething()  // x never used
}
```

Keep code clean. Run `go vet` and `golangci-lint`.

---

## Antipattern Detection Commands

```bash
# Logging secrets
grep -r 'log.*apiKey\|log.*password\|log.*secret\|log.*token' --include="*.go"

# Missing ctx pass-through
grep -r 'ListUsers()\|GetUser()' --include="*.go" | grep -v ctx

# Error swallowing
grep -rA2 'if err != nil' --include="*.go" | grep -B1 'log.Print' | grep -v return

# Hardcoded URLs
grep -r 'https://.*\.com' --include="*.go" | grep -v _test.go

# Direct type assertions
grep -rE '\.\([A-Za-z]+\)[^,]' --include="*.go" | grep -v ", ok"
```
