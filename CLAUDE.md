# CLAUDE.md

Instructions for AI assistants working with this Baton connector.

## Additional Documentation

Detailed connector documentation is in `.claude/skills/connector/`:
- `INDEX.md` - Skills overview and selection guide
- `concepts-identifiers.md` - ResourceId vs ExternalId (CRITICAL for provisioning)
- `build-provisioning.md` - Grant/Revoke implementation patterns
- `build-pagination.md` - Pagination strategies
- `ref-unused-features.md` - SDK feature usage notes

**Change-Type Specific Guidance**: See `CHANGE_TYPES.md` for guidance based on what type of change you're making (SDK upgrade, pagination fix, panic fix, provisioning, etc.).

## What This Is

A ConductorOne Baton connector that syncs identity and access data from a downstream service. Connectors implement the `ResourceSyncer` interface to expose users, groups, roles, and their relationships.

## Build & Test

```bash
go build ./cmd/baton-*        # Build connector
go test ./...                 # Run tests
go test -v ./... -count=1     # Verbose, no cache
```

## Architecture

**SDK Inversion of Control:** The connector implements interfaces; the SDK orchestrates execution.

**Four Sync Phases (SDK-driven):**
1. `ResourceType()` - Declare what resource types exist
2. `List()` - Fetch all resources of each type
3. `Entitlements()` - Fetch available permissions per resource
4. `Grants()` - Fetch who has what access

**Key Interfaces:**
- `ResourceSyncer` - Main interface for sync (List, Entitlements, Grants)
- `ResourceBuilder` - Creates resources with traits
- `ConnectorBuilder` - Wires everything together

## Critical Patterns

### Pagination Termination

```go
// WRONG - stops after first page
return resources, "", nil, nil

// CORRECT - pass through API's token
if resp.NextPage == "" {
    return resources, "", nil, nil
}
return resources, resp.NextPage, nil, nil
```

### Entity Sources in Grant/Revoke

```go
func (g *groupBuilder) Grant(ctx context.Context, principal *v2.Resource,
    entitlement *v2.Entitlement) ([]*v2.Grant, annotations.Annotations, error) {

    // WHO - get native ID from ExternalId (required for API calls)
    externalId := principal.GetExternalId()
    if externalId == nil {
        return nil, nil, fmt.Errorf("baton-service: principal missing external ID")
    }
    nativeUserID := externalId.Id  // Use this for API calls

    // Fallback: principal.Id.Resource if you set ExternalId to same value during sync

    // WHAT - from entitlement
    groupID := entitlement.Resource.Id.Resource

    // CONTEXT (workspace/org) - from principal, NOT entitlement
    workspaceID := principal.ParentResourceId.Resource  // CORRECT
    // workspaceID := entitlement.Resource.ParentResourceId.Resource  // WRONG
}
```

**Note on ExternalId:** During sync, set `WithExternalID()` with the native system identifier. During Grant/Revoke, retrieve it via `GetExternalId()` to make API calls. ConductorOne assigns its own resource IDs that differ from the target system's native IDs.

### HTTP Response Safety

```go
resp, err := client.Do(req)
if err != nil {
    if resp != nil {  // resp may be nil on network errors
        defer resp.Body.Close()
    }
    return fmt.Errorf("baton-service: request failed: %w", err)
}
defer resp.Body.Close()  // After error check
```

### Error Handling

```go
// Always include connector prefix and use %w
return fmt.Errorf("baton-service: failed to list users: %w", err)

// Never swallow errors
if err != nil {
    log.Println(err)  // WRONG - continues with bad state
    return nil, "", nil, err  // CORRECT - propagate
}
```

### JSON Type Safety

```go
// WRONG - fails if API returns {"id": 12345}
type Group struct {
    ID string `json:"id"`
}

// CORRECT - handles both string and number
type Group struct {
    ID json.Number `json:"id"`
}

// Usage
groupID := group.ID.String()
```

For complex cases, use a custom unmarshaler:

```go
type FlexibleID string

func (f *FlexibleID) UnmarshalJSON(data []byte) error {
    var s string
    if json.Unmarshal(data, &s) == nil {
        *f = FlexibleID(s)
        return nil
    }
    var n int64
    if json.Unmarshal(data, &n) == nil {
        *f = FlexibleID(strconv.FormatInt(n, 10))
        return nil
    }
    return fmt.Errorf("id must be string or number")
}
```

### Grant Idempotency

```go
// Grant "already exists" = success, not error
if isAlreadyExistsError(err) {
    return nil, annotations.New(&v2.GrantAlreadyExists{}), nil
}

// Revoke "not found" = success, not error
if isNotFoundError(err) {
    return annotations.New(&v2.GrantAlreadyRevoked{}), nil
}
```

**Key point:** "Already exists" and "already revoked" are NOT errors - return `nil` error with the annotation.

### Resource Cleanup

```go
// Connectors that create clients MUST close them
func (c *Connector) Close() error {
    if c.client != nil {
        return c.client.Close()
    }
    return nil
}
```

## Common Mistakes

1. **Pagination bugs** - Infinite loop (hardcoded token) or early termination (always empty token)
2. **Entity confusion** - Getting workspace from entitlement instead of principal
3. **Swapped arguments** - Multiple string params in wrong order (check API docs!)
4. **Nil pointer panic** - Accessing resp.Body when resp is nil
5. **Error swallowing** - Logging but not returning errors
6. **Unstable IDs** - Using email instead of stable API ID
7. **JSON type mismatch** - API returns number, Go expects string (use json.Number)
8. **Wrong trait type** - Using User for service accounts (use App)
9. **Pagination bag not init** - Forgetting to initialize bag on first call
10. **Resource leak** - Close() returns nil without closing client connections
11. **Non-idempotent grants** - Returning error on "already exists" instead of success
12. **Missing ExternalId** - Not setting WithExternalID() during sync; provisioning then fails
13. **Scientific notation** - Using `%v` with large numeric IDs produces `1.23e+15` instead of `1234567890123456`

## Resource Types

| Type | Trait | Typical Use |
|------|-------|-------------|
| User | `TRAIT_USER` | Human identities |
| Group | `TRAIT_GROUP` | Collections with membership |
| Role | `TRAIT_ROLE` | Permission sets |
| App | `TRAIT_APP` | Service accounts, API keys |

## What NOT to Do

- Don't buffer all pages in memory (OOM risk)
- Don't ignore context cancellation
- Don't log secrets
- Don't hardcode API URLs (breaks testing)
- Don't use `%v` for error wrapping (use `%w`)
- Don't return nil from Close() if you created clients
- Don't return error on "already exists" for Grant operations
- Don't use `%v` or `%g` with large numeric IDs (use `%d` or `strconv` to avoid scientific notation)

## SDK Features: Usage Notes

**Required for provisioning:**
- `WithExternalID()` - REQUIRED for Grant/Revoke to work. Stores the native system ID that provisioning operations need to call the target API. During Grant, retrieve via `principal.GetExternalId().Id`.

**Rarely used (but valid):**
- `WithMFAStatus()`, `WithSSOStatus()` - Only relevant for IDP connectors
- `WithStructuredName()` - Rarely needed; DisplayName usually sufficient
- Complex user profile fields beyond basics - Only if downstream needs them

## Testing

Connectors should support:
- `--base-url` flag for mock server testing
- `--insecure` flag for self-signed certs in tests

## File Structure

```
cmd/baton-*/main.go           # Entry point
pkg/connector/connector.go    # ConnectorBuilder implementation
pkg/connector/*_builder.go    # ResourceSyncer implementations
pkg/client/client.go          # API client
```

## Debugging

```bash
# Run with debug logging
LOG_LEVEL=debug ./baton-* --config-file=config.yaml

# Output to specific file
./baton-* --file=sync.c1z

# Inspect output
baton resources --file=sync.c1z
baton grants --file=sync.c1z
```
