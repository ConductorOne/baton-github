# patterns-json-safety

JSON unmarshaling pitfalls and type flexibility patterns.

---

## The Problem (8+ PRs)

APIs are inconsistent about types. The same field might be:
- String in one endpoint: `{"id": "12345"}`
- Number in another: `{"id": 12345}`
- Sometimes null: `{"id": null}`

Go's strict typing causes runtime errors:

```
json: cannot unmarshal number into Go struct field Group.id of type string
```

---

## JSON Type Mismatch Patterns

### ID Fields (Most Common)

**WRONG - assumes string:**
```go
type Group struct {
    ID string `json:"id"`  // Fails if API returns 12345 (number)
}
```

**CORRECT - use json.Number:**
```go
type Group struct {
    ID json.Number `json:"id"`  // Handles both "12345" and 12345
}

// Usage
groupID := group.ID.String()  // Always get string
```

### Boolean Fields

**WRONG - assumes bool:**
```go
type User struct {
    Active bool `json:"active"`  // Fails if API returns "true" or 1
}
```

**CORRECT - flexible bool:**
```go
type User struct {
    Active FlexibleBool `json:"active"`
}

type FlexibleBool bool

func (f *FlexibleBool) UnmarshalJSON(data []byte) error {
    // Handle: true, false, "true", "false", 1, 0
    var b bool
    if err := json.Unmarshal(data, &b); err == nil {
        *f = FlexibleBool(b)
        return nil
    }
    var s string
    if err := json.Unmarshal(data, &s); err == nil {
        *f = FlexibleBool(s == "true" || s == "1")
        return nil
    }
    var n int
    if err := json.Unmarshal(data, &n); err == nil {
        *f = FlexibleBool(n != 0)
        return nil
    }
    return fmt.Errorf("cannot unmarshal %s into bool", data)
}
```

### Nullable Fields

**WRONG - no null handling:**
```go
type User struct {
    Email string `json:"email"`  // Fails if API returns null
}
```

**CORRECT - use pointer:**
```go
type User struct {
    Email *string `json:"email"`  // nil for null
}

// Usage
var email string
if user.Email != nil {
    email = *user.Email
}
```

---

## FlexibleID Pattern

For IDs that might be string or number:

```go
type FlexibleID string

func (f *FlexibleID) UnmarshalJSON(data []byte) error {
    // Try string first (most common)
    var s string
    if err := json.Unmarshal(data, &s); err == nil {
        *f = FlexibleID(s)
        return nil
    }

    // Try number
    var n json.Number
    if err := json.Unmarshal(data, &n); err == nil {
        *f = FlexibleID(n.String())
        return nil
    }

    return fmt.Errorf("id must be string or number, got: %s", data)
}

func (f FlexibleID) String() string {
    return string(f)
}
```

**Usage:**
```go
type Group struct {
    ID   FlexibleID `json:"id"`
    Name string     `json:"name"`
}

// Works for both:
// {"id": "abc123", "name": "Admins"}
// {"id": 12345, "name": "Admins"}

resourceID := group.ID.String()  // Always string
```

---

## API Response Variations

Watch for these API inconsistencies:

| Field | Variation 1 | Variation 2 | Solution |
|-------|-------------|-------------|----------|
| ID | `"12345"` | `12345` | `json.Number` or `FlexibleID` |
| Active | `true` | `"true"` or `1` | `FlexibleBool` |
| Count | `0` | `null` | `*int` |
| Email | `"a@b.com"` | `null` | `*string` |
| Timestamp | `"2024-01-01"` | `1704067200` | Custom unmarshaler |

---

## Empty vs Null vs Missing

Different meanings, different handling:

```go
type User struct {
    // Missing key and null both become nil
    Email *string `json:"email,omitempty"`

    // Distinguish missing from null
    Name NullableString `json:"name"`
}

type NullableString struct {
    Value   string
    IsNull  bool
    Present bool
}

func (n *NullableString) UnmarshalJSON(data []byte) error {
    n.Present = true
    if string(data) == "null" {
        n.IsNull = true
        return nil
    }
    return json.Unmarshal(data, &n.Value)
}
```

---

## Array vs Single Object

Some APIs return single item as object, multiple as array:

```go
// API might return:
// {"users": {"id": "1"}}        - single user
// {"users": [{"id": "1"}, ...]} - multiple users

type Response struct {
    Users FlexibleArray[User] `json:"users"`
}

type FlexibleArray[T any] []T

func (f *FlexibleArray[T]) UnmarshalJSON(data []byte) error {
    // Try array first
    var arr []T
    if err := json.Unmarshal(data, &arr); err == nil {
        *f = arr
        return nil
    }

    // Try single object
    var single T
    if err := json.Unmarshal(data, &single); err == nil {
        *f = []T{single}
        return nil
    }

    return fmt.Errorf("expected array or object")
}
```

---

## Detection in Code Review

**Red flags:**
1. `ID string` for API response structs - should be `json.Number` or `FlexibleID`
2. `bool` for API fields without checking API consistency
3. Non-pointer types for optional fields
4. No custom unmarshalers for inconsistent APIs

**Questions to ask:**
- "What types does this API actually return? Did you check multiple endpoints?"
- "What happens if this field is null?"
- "Does the API always return this as a string, or sometimes a number?"

---

## Testing JSON Handling

```go
func TestFlexibleID_Unmarshal(t *testing.T) {
    tests := []struct {
        input    string
        expected string
    }{
        {`{"id": "abc123"}`, "abc123"},
        {`{"id": 12345}`, "12345"},
        {`{"id": 0}`, "0"},
    }

    for _, tt := range tests {
        var obj struct {
            ID FlexibleID `json:"id"`
        }
        err := json.Unmarshal([]byte(tt.input), &obj)
        if err != nil {
            t.Errorf("failed to unmarshal %s: %v", tt.input, err)
        }
        if obj.ID.String() != tt.expected {
            t.Errorf("got %s, want %s", obj.ID, tt.expected)
        }
    }
}
```
