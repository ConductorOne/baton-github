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

## Nested Bag Pagination (Bag-within-Bag)

When an outer function uses a Bag to dispatch to inner functions that manage their own Bag, the inner bag's serialized state becomes the `Token` field in the outer bag's `PageState`. This creates a two-layer pagination pattern that is easy to get wrong.

### How It Works

```go
// OUTER: dispatches between resource types
func (o *myType) Grants(ctx context.Context, resource *v2.Resource,
    attrs resource2.SyncOpAttrs) ([]*v2.Grant, *resource2.SyncOpResults, error) {

    bag := &pagination.Bag{}
    bag.Unmarshal(token.Token)
    if bag.Current() == nil {
        bag.Push(pagination.PageState{ResourceTypeID: "users"})
        bag.Push(pagination.PageState{ResourceTypeID: "groups"})
    }

    page := bag.PageToken() // This is the INNER bag's serialized state

    switch bag.ResourceTypeID() {
    case "users":
        rv, nextPage, annos, err = o.userGrants(ctx, resource, attrs, page)
    case "groups":
        rv, nextPage, annos, err = o.groupGrants(ctx, resource, attrs, page)
    }

    // nextPage is the inner bag's Marshal() output.
    // When nextPage == "", bag.Next("") pops current type, advances to next.
    // When nextPage != "", bag.Next(nextPage) keeps current type with updated token.
    bag.Next(nextPage)

    pageToken, _ := bag.Marshal()
    return rv, &resource2.SyncOpResults{NextPageToken: pageToken}, nil
}
```

### The Ghost State Bug

`pagination.Bag.Marshal()` returns `""` (empty) only when `currentState` is nil. A `PageState` with all zero-value fields (`{Token: "", ResourceID: ""}`) is **non-nil** and serializes to `{"states":[],"current_state":{}}`.

This non-empty string causes the outer bag to think there's still work to do, keeping the current resource type alive instead of advancing to the next one. The inner function then re-initializes from scratch, re-fetching from the beginning — an infinite loop.

```
Inner bag state: {Token: "", ResourceID: ""}  (all empty, but non-nil)
Inner Marshal():  {"states":[],"current_state":{}}  (non-empty string!)
Outer bag sees:   non-empty token → keep "users" type active
Next call:        inner Unmarshal → Current() != nil → skip init
                  ResourceID == "" → fetch API page with Token == "" → page 1 again
                  INFINITE LOOP
```

### How Ghost States Appear: Stale State in Stack

When you push per-item states on top of a page state, the page state gets buried in the stack. After all items are popped, the page state resurfaces:

```go
// WRONG — page state survives in the stack after all items are popped
for _, user := range users {
    if needsExtraProcessing(user) {
        bag.Push(pagination.PageState{  // Pushes ON TOP of the page state
            ResourceID: user.ID,
        })
    }
}

// This Pop removes the last pushed user, NOT the page state!
bag.Pop()
if nextPage != "" {
    bag.Push(pagination.PageState{Token: nextPage})
}
```

After all users are individually popped in later calls, the original page state `{Token: "", ResourceID: ""}` becomes current again → ghost state → infinite loop.

### Correct Pattern: Pop Page State Before Pushing Items

Pop the consumed page state first, then push the next page (if any), then push per-item states on top. This way the consumed state is gone, and items sit above the next page in the stack.

```go
// CORRECT — Pop consumed state BEFORE pushing anything
bag.Pop() // Remove the page state we just consumed

if nextPage != "" {
    bag.Push(pagination.PageState{
        Token:      nextPage,
        ResourceID: "", // Next API page, sits at bottom of stack
    })
}

// Now push per-item states on top (processed first, LIFO)
for _, user := range users {
    if needsExtraProcessing(user) {
        bag.Push(pagination.PageState{
            ResourceID: user.ID,
        })
    }
}

nextPageToken, err := bag.Marshal()
return rv, nextPageToken, annos, nil
```

This guarantees:
- The consumed page state is gone before any items are pushed
- Next page (if any) sits below items, processed after all items are done
- When all items are popped and no next page exists, the bag is truly empty → `Marshal()` returns `""` → outer bag advances

### Invariant: Never Leave All-Empty PageState in the Bag

Every `PageState` in the bag should have at least one non-empty field. If `Marshal()` can produce a non-empty string when there's no real work left, the outer bag will loop.

- Page states: must have non-empty `Token` (guarded by `if nextPage != ""`)
- Item states: must have non-empty `ResourceID` (from the item being processed)
- Initial states (`{Token: "", ResourceID: ""}`) must be popped before returning

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
