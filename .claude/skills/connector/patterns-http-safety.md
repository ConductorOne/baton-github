<!-- This file is managed by baton-admin. DO NOT EDIT. -->
# patterns-http-safety

HTTP response handling and nil pointer safety.

---

## The Problem

HTTP errors can leave `resp` as nil. Accessing `resp.Body` or `resp.StatusCode` in error paths causes panics.

This is the #3 bug pattern: 13 panic fixes across 12+ repos.

---

## Wrong Pattern

```go
resp, err := client.Do(req)
if err != nil {
    // PANIC: resp is nil on network errors
    log.Printf("Error: %v, Status: %d", err, resp.StatusCode)
    return err
}
defer resp.Body.Close()
```

---

## Correct Pattern

```go
resp, err := client.Do(req)
if err != nil {
    // resp MAY be nil - check before using
    if resp != nil {
        defer resp.Body.Close()
        body, _ := io.ReadAll(resp.Body)
        return fmt.Errorf("request failed (status %d): %s: %w", resp.StatusCode, body, err)
    }
    return fmt.Errorf("request failed: %w", err)
}
defer resp.Body.Close()
```

---

## Defer Placement

**Wrong - defer before error check:**
```go
resp, err := client.Do(req)
defer resp.Body.Close()  // PANIC if resp is nil
if err != nil {
    return err
}
```

**Correct - defer after error check:**
```go
resp, err := client.Do(req)
if err != nil {
    return err
}
defer resp.Body.Close()
```

---

## Map Type Assertions

**Wrong - direct assertion panics on missing key:**
```go
userID := data["user_id"].(string)  // PANIC if missing or wrong type
```

**Correct - two-value form:**
```go
userID, ok := data["user_id"].(string)
if !ok {
    return fmt.Errorf("user_id missing or not string")
}
```

---

## ParentResourceId Access

**Wrong - direct access without nil check:**
```go
parentID := resource.ParentResourceId.Resource  // PANIC if nil
```

**Correct - nil check first:**
```go
var parentID string
if resource.ParentResourceId != nil {
    parentID = resource.ParentResourceId.Resource
}
```

---

## Error Check Ordering

Always check error before using returned values:

```go
// WRONG
result, err := doSomething()
fmt.Println(result.Value)  // Use before check
if err != nil {
    return err
}

// CORRECT
result, err := doSomething()
if err != nil {
    return err
}
fmt.Println(result.Value)  // Use after check
```

---

## HTTP Status Handling

```go
func handleResponse(resp *http.Response) error {
    switch resp.StatusCode {
    case http.StatusOK, http.StatusCreated, http.StatusNoContent:
        return nil
    case http.StatusNotFound:
        return nil  // Often not an error - resource doesn't exist
    case http.StatusUnauthorized:
        return uhttp.WrapErrors(codes.Unauthenticated, "baton-myservice: unauthorized",
            fmt.Errorf("check credentials (status %d)", resp.StatusCode))
    case http.StatusForbidden:
        return uhttp.WrapErrors(codes.PermissionDenied, "baton-myservice: forbidden",
            fmt.Errorf("check permissions (status %d)", resp.StatusCode))
    case http.StatusTooManyRequests:
        // A bare fmt.Errorf carries no gRPC code, so status.Code() reads Unknown and
        // no retryer will ever pick it up. Wrap rate limits and 5xx as Unavailable —
        // what uhttp produces for them — see the wrapping table in
        // patterns-error-handling.md, which ships to every connector kind.
        // WrapErrorsWithRateLimitInfo keeps the response's rate-limit headers on the
        // error, so a retryer can size its backoff from them instead of guessing.
        return uhttp.WrapErrorsWithRateLimitInfo(codes.Unavailable, resp,
            fmt.Errorf("baton-myservice: rate limited (status %d)", resp.StatusCode))
    case http.StatusNotImplemented:
        // The one 5xx that is not transient: the vendor does not implement this
        // endpoint, so retrying never helps. uhttp maps it to Unimplemented too.
        return uhttp.WrapErrors(codes.Unimplemented, "baton-myservice: not implemented",
            fmt.Errorf("status %d", resp.StatusCode))
    default:
        if resp.StatusCode >= 500 {
            return uhttp.WrapErrors(codes.Unavailable, "baton-myservice: server error",
                fmt.Errorf("status %d", resp.StatusCode))
        }
        return uhttp.WrapErrors(uhttp.GrpcCodeFromHTTPStatus(resp.StatusCode),
            "baton-myservice: unexpected status", fmt.Errorf("status %d", resp.StatusCode))
    }
}
```

---

## JSON Unmarshaling Safety

**Wrong - API might return number as ID:**
```go
type User struct {
    ID string `json:"id"`  // Fails if API returns {"id": 12345}
}
```

**Correct - flexible type:**
```go
type User struct {
    ID json.Number `json:"id"`  // Handles both "12345" and 12345
}

// Usage
userID := user.ID.String()
```

---

## Detection in Code Review

**Red flags:**
1. `resp.Body` or `resp.StatusCode` in error path without nil check
2. `defer resp.Body.Close()` before error check
3. Direct type assertions `x.(type)` without ok check
4. Direct `.ParentResourceId.Resource` access
5. `ID string` for fields that might be numbers

**Questions to ask:**
- "What if resp is nil here?"
- "What if this key is missing from the map?"
- "What if ParentResourceId is nil?"
