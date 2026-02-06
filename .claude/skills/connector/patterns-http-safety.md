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
        return fmt.Errorf("baton-myservice: unauthorized (check credentials)")
    case http.StatusForbidden:
        return fmt.Errorf("baton-myservice: forbidden (check permissions)")
    case http.StatusTooManyRequests:
        return fmt.Errorf("baton-myservice: rate limited")  // SDK retries
    default:
        if resp.StatusCode >= 500 {
            return fmt.Errorf("baton-myservice: server error %d", resp.StatusCode)
        }
        return fmt.Errorf("baton-myservice: unexpected status %d", resp.StatusCode)
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
