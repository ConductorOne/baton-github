# build-pagination

Pagination is critical. Enterprise environments have tens of thousands of users.

---

## Why Pagination Matters

- SDK checkpoints every 10 seconds
- Without pagination, interrupted syncs restart from zero
- Memory exhaustion on large datasets
- API rate limits easier to handle page-by-page

**Always implement pagination, even for small datasets in testing.**

---

## pagination.Bag Pattern

```go
func (u *userBuilder) List(ctx context.Context, parentID *v2.ResourceId,
    token *pagination.Token) ([]*v2.Resource, string, annotations.Annotations, error) {

    // 1. Unmarshal the bag
    bag := &pagination.Bag{}
    if err := bag.Unmarshal(token.Token); err != nil {
        return nil, "", nil, err
    }

    // 2. Initialize on first call
    if bag.Current() == nil {
        bag.Push(pagination.PageState{
            ResourceTypeID: userResourceType.Id,
        })
    }

    // 3. Get current page token
    pageToken := bag.PageToken()

    // 4. Fetch one page from API
    users, nextCursor, err := u.client.ListUsers(ctx, pageToken, 100)
    if err != nil {
        return nil, "", nil, err
    }

    // 5. Process results
    var resources []*v2.Resource
    for _, user := range users {
        // ... create resources
    }

    // 6. Create next token
    nextPage, err := bag.NextToken(nextCursor)
    if err != nil {
        return nil, "", nil, err
    }

    return resources, nextPage, nil, nil
}
```

---

## Two Failure Modes (Both Critical)

### 1. Early Termination - Misses Data

```go
// WRONG - always stops after first page
func (u *userBuilder) List(...) (...) {
    users, _, _ := client.ListUsers(ctx, pageToken, 100)
    // ... process users ...
    return resources, "", nil, nil  // Always empty token!
}
```

**Result**: Only first page synced. Silent data loss.

### 2. Infinite Loop - Never Stops

```go
// WRONG - hardcoded token
return resources, "next", nil, nil  // Always returns "next"

// WRONG - result count termination
for {
    results, _ := client.List(offset)
    if len(results) < pageSize {
        break  // Empty page doesn't trigger break!
    }
    offset += pageSize  // Runs forever
}
```

**Result**: Sync hangs. Resource exhaustion. 5 production fixes for this pattern.

### Correct Pattern - Token Passthrough

```go
// CORRECT - pass through API's token
func (u *userBuilder) List(...) (...) {
    users, nextCursor, err := client.ListUsers(ctx, pageToken, 100)
    if err != nil {
        return nil, "", nil, err
    }

    // ... process users ...

    // Pass through exactly what API returned
    nextPage, err := bag.NextToken(nextCursor)
    if err != nil {
        return nil, "", nil, err
    }

    return resources, nextPage, nil, nil
}
```

**Key**: `nextCursor` comes from API response. When API has no more pages, it returns empty string. You pass that through.

---

## Choosing Pagination Strategy

**Read the API docs first.** Using wrong strategy = bugs.

| API Signal | Strategy | Notes |
|------------|----------|-------|
| Returns `next_cursor`, `cursor`, `page_token` | Cursor | Preferred. Opaque token. |
| Returns `Link` header with `rel="next"` | Link header | GitHub, some REST APIs |
| Returns `total_count` and supports `offset` | Offset | Requires math. Error-prone. |
| Returns `has_more` boolean | Cursor variant | Use with cursor or offset |
| No pagination info returned | Check docs! | API may have undocumented pagination |

### Cursor-based (preferred)
```go
// API: /users?cursor=abc123
resp, err := client.ListUsers(ctx, cursor, pageSize)
nextCursor := resp.NextCursor  // Use directly - opaque token
```

### Offset-based (error-prone)
```go
// API: /users?offset=100&limit=50
// Must track offset yourself
type offsetToken struct {
    Offset int `json:"offset"`
}

// DANGER: If items added/removed during sync, you skip or duplicate
// Only use if API doesn't support cursor
```

### Link header (GitHub style)
```go
// Parse Link header for "next" URL
linkHeader := resp.Header.Get("Link")
nextURL := parseLinkHeader(linkHeader, "next")
// Extract cursor from URL query params
```

### Page number-based (least preferred)
```go
// API: /users?page=3&per_page=50
// Same problems as offset - avoid if possible
```

**When in doubt, use cursor if API supports it.**

---

## SDK Validation

The SDK validates pagination:

```go
// SDK checks that page tokens change between calls
// If same token returned twice, SDK fails with:
// "next page token is the same as current - connector bug"
```

This catches infinite loops caused by incorrect termination.

---

## Multi-Resource Pagination

For Grants() that traverse multiple resource types:

```go
func (g *groupBuilder) Grants(ctx context.Context, resource *v2.Resource,
    token *pagination.Token) ([]*v2.Grant, string, annotations.Annotations, error) {

    bag := &pagination.Bag{}
    if err := bag.Unmarshal(token.Token); err != nil {
        return nil, "", nil, err
    }

    // Initialize with multiple pages to traverse
    if bag.Current() == nil {
        bag.Push(pagination.PageState{ResourceTypeID: "direct_members"})
        bag.Push(pagination.PageState{ResourceTypeID: "nested_groups"})
    }

    switch bag.ResourceTypeID() {
    case "direct_members":
        // Handle direct members
        // When done, bag.Next() pops to "nested_groups"
    case "nested_groups":
        // Handle nested groups
    }

    // ...
}
```

---

## Page Size Selection

| API Limit | Recommendation |
|-----------|----------------|
| No limit | Use 100-200 |
| Has limit | Use slightly under limit |
| Rate limited | Smaller pages, more frequent checkpoints |

**Don't hardcode arbitrary sizes:**
```go
// WRONG
const pageSize = 10    // Too small, many API calls
const pageSize = 10000 // Too large, memory pressure

// CORRECT - check API docs
const pageSize = 200   // Google Workspace limit
const pageSize = 100   // GitHub API recommendation
```
