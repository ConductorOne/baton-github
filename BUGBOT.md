# Project review guidelines

## Excluded Paths

### The following directories and files should be excluded from review:
- `vendor/` - Third-party dependencies managed by Go modules

## Configuration

- Configuration fields in config.go should include a WithDisplayName
- Field relationships should be defined in the config.go file
- Secrets must use `field.WithIsSecret(true)`

## Resource Types

Resource types that do not list entitlements or grants should have the SkipEntitlementsAndGrants annotation in the ResourceType definition.

## Breaking Change Considerations

All connectors should be considered potentially in-use, and the data they expose should be considered a stable API.

### Resource Type Changes

- **NEVER remove a resource type**
- **NEVER change how resource IDs are calculated** - the ID of a given resource must remain stable across all versions of the connector
- **NEVER change resource type IDs** (e.g., changing `Id: "user"` to `Id: "account"`)
- **EXERCISE EXTREME CAUTION** when filtering out previously included resources
- **EXERCISE EXTREME CAUTION** when changing how any values associated with a resource are calculated

### Entitlement Changes

- **NEVER remove an entitlement**
- **NEVER change entitlement slugs** (e.g., `"member"` to `"membership"`)
- **NEVER change permission entitlement names**

### User Profile Changes

Resources implementing the `User` trait may have an associated user profile, typically set using `WithUserProfile`. All changes to user profiles must remain backwards compatible:

- **NEVER remove keys from user profiles**
- **NEVER change the type of a value in a user profile**
- **NEVER change how values are represented in a user profile** - eg `alice` should always be `alice`, not `Alice` or `alice@example.com`

## Critical Bug Patterns

### 1. Entity Source Confusion (High Severity)

In Grant/Revoke operations, context (workspace/org/tenant) must come from `principal`, not `entitlement`.

**Flag this pattern:**
```go
entitlement.Resource.ParentResourceId  // WRONG for context
```

**Correct pattern:**
```go
principal.ParentResourceId  // Context comes from principal
```

### 2. Pagination Termination (High Severity)

Two failure modes - both cause data loss or infinite loops:

**Early termination (stops too soon):**
```go
return resources, "", nil, nil  // Always empty token - misses pages
```

**Infinite loop (never stops):**
```go
return resources, "next", nil, nil  // Hardcoded token - runs forever
return resources, nextPage, nil, nil  // If nextPage never becomes ""
```

**Verify:**
- Token comes from API response, not hardcoded
- There's a path where token becomes "" (termination condition)
- API's "no more pages" signal is correctly detected

### 3. HTTP Response Nil Pointer (High Severity)

When `err != nil`, the response `resp` may be nil. Accessing `resp.Body` or `resp.StatusCode` without nil check causes panics.

**Flag this pattern:**
```go
resp, err := client.Do(req)
if err != nil {
    log.Printf("status: %d", resp.StatusCode)  // PANIC if resp nil
}
```

**Or this:**
```go
resp, err := client.Do(req)
defer resp.Body.Close()  // PANIC - defer before error check
if err != nil {
```

### 4. Type Assertion Panics (Medium Severity)

Direct type assertions without ok check can panic.

**Flag this pattern:**
```go
value := data["key"].(string)  // PANIC if wrong type or missing
```

**Correct pattern:**
```go
value, ok := data["key"].(string)
if !ok { ... }
```

### 5. Error Swallowing (Medium Severity)

Logging errors but continuing execution causes silent data loss.

**Flag this pattern:**
```go
if err != nil {
    log.Println("error:", err)
    // No return - continues with bad state
}
```

### 6. Error Wrapping (Low Severity)

Errors should use `%w` for wrapping to preserve error chain, and include connector prefix.

**Flag this pattern:**
```go
fmt.Errorf("failed: %v", err)  // %v breaks error chain
fmt.Errorf("failed: %w", err)  // Missing connector prefix
```

**Correct pattern:**
```go
fmt.Errorf("baton-service: failed to list users: %w", err)
```

### 7. Unstable Resource IDs (Medium Severity)

Resource IDs must be stable across syncs. Using mutable fields like email as ID causes duplicate resources.

**Flag this pattern:**
```go
rs.NewUserResource(name, userType, user.Email, ...)  // Email can change
```

**Correct pattern:**
```go
rs.NewUserResource(name, userType, user.ID, ...)  // Stable API ID
```

### 8. JSON Type Mismatch (Medium Severity)

APIs may return numbers where code expects strings (or vice versa). Causes unmarshaling failures.

**Flag this pattern:**
```go
type Group struct {
    ID string `json:"id"`  // Fails if API returns {"id": 12345}
}
```

**Correct pattern:**
```go
type Group struct {
    ID json.Number `json:"id"`  // Handles both "12345" and 12345
}
```

### 9. Wrong Trait Type (High Severity - Caused Revert)

AWS accounts, service accounts, and machine identities must use App trait, not User trait.

**Flag this pattern:**
```go
// Suspicious: "account" with User trait
rs.NewUserResource(name, accountType, ...)  // Should this be App?
```

**Ask**: Is this a human who logs in? If no, use App trait.

### 10. Pagination Bag Not Initialized (Critical Severity)

Pagination bag must be initialized on first call or it panics.

**Flag this pattern:**
```go
bag, _ := parsePageToken(pt.Token, &v2.ResourceId{})
token := bag.Current().Token  // PANIC if first call
```

**Correct pattern:**
```go
bag, _ := parsePageToken(pt.Token, &v2.ResourceId{})
if bag.Current() == nil {
    bag.Push(pagination.PageState{ResourceTypeID: resourceType.Id})
}
```

### 11. Resource Leak / Missing Close() (High Severity)

Connectors that create clients (HTTP, database, etc.) must implement `io.Closer`.

**Flag this pattern:**
```go
func (c *Connector) Close() error {
    return nil  // NOT closing client resources
}
```

**Correct pattern:**
```go
func (c *Connector) Close() error {
    if c.client != nil {
        return c.client.Close()
    }
    return nil
}
```

**Detection:**
- Check if connector creates clients in `New()`
- Verify `Close()` actually closes them

### 12. Grant Idempotency Missing (Medium Severity)

Grant/Revoke should succeed if the state already matches.

**Flag this pattern:**
```go
if err != nil && strings.Contains(err.Error(), "already exists") {
    return nil, nil, err  // WRONG - should succeed
}
```

**Correct pattern:**
```go
if err != nil && strings.Contains(err.Error(), "already exists") {
    return nil, annotations.New(&v2.GrantAlreadyExists{}), nil
}
```

### 14. Missing ExternalId (High Severity - Provisioning Failure)

Resources used in provisioning must have ExternalId set during sync.

**Flag this pattern:**
```go
// Sync creates resource without ExternalId
rs.NewUserResource(name, userType, id, traits)  // Missing WithExternalID
```

**Correct pattern:**
```go
rs.NewUserResource(name, userType, id, traits,
    rs.WithExternalID(&v2.ExternalId{Id: nativeAPIId}),
)
```

**Also flag in Grant/Revoke:**
```go
// Using ResourceId instead of ExternalId for API calls
userID := principal.Id.Resource  // May not be native ID
```

**Correct pattern:**
```go
externalId := principal.GetExternalId()
if externalId == nil {
    return nil, nil, fmt.Errorf("baton-myservice: principal missing external ID")
}
nativeUserID := externalId.Id  // Use for API calls
```

### 13. Provisioning Context (Medium Severity)

Account/entitlement provisioning must use correct context.

**Verify:**
- Provisioning operations use principal's context (org/workspace), not entitlement's
- De-provisioning checks if resource exists before attempting delete
- Role/group membership changes are idempotent

### 14. Missing ExternalId (High Severity - Provisioning Failure)

Resources used in provisioning must have ExternalId set during sync.

**Flag this pattern:**
```go
// Sync creates resource without ExternalId
rs.NewUserResource(name, userType, id, traits)  // Missing WithExternalID
```

**Correct pattern:**
```go
rs.NewUserResource(name, userType, id, traits,
    rs.WithExternalID(&v2.ExternalId{Id: nativeAPIId}),
)
```

**Also flag in Grant/Revoke:**
```go
// Using ResourceId instead of ExternalId for API calls
userID := principal.Id.Resource  // May not be native ID
```

**Correct pattern:**
```go
externalId := principal.GetExternalId()
if externalId == nil {
    return nil, nil, fmt.Errorf("baton-myservice: principal missing external ID")
}
nativeUserID := externalId.Id  // Use for API calls
```

### 15. Scientific Notation in Resource IDs (High Severity)

Large numeric IDs can be formatted as scientific notation (e.g., `1.23456789e+15`), breaking resource matching.

**Flag this pattern:**
```go
// WRONG - fmt default formatting may use scientific notation for large numbers
id := fmt.Sprintf("%v", numericID)
id := fmt.Sprintf("%g", float64(numericID))
```

**Correct pattern:**
```go
// Use %d for integers, %s for strings, or strconv
id := fmt.Sprintf("%d", numericID)
id := strconv.FormatInt(numericID, 10)
id := strconv.FormatUint(uint64(numericID), 10)
```

**Also watch for:** JSON unmarshaling large numbers into `float64` which loses precision and may format as scientific notation.

## Detection Patterns

### Search for potential issues:

```
# Entity confusion
entitlement\.Resource\.ParentResourceId

# Always-empty pagination
return .*, "", nil, nil

# Nil pointer risk
resp\.StatusCode|resp\.Body.*err

# Defer before check
defer.*Close\(\).*\n.*if err

# Type assertion without ok
\.\([A-Za-z]+\)[^,]

# Error swallowing
log\.Print.*err.*\n[^r]*$

# Wrong error verb
fmt\.Errorf.*%v.*err

# Resource leak (Close returns nil without closing)
func \(.*\) Close\(\).*error.*\{[\s]*return nil

# Scientific notation risk - %v or %g with numeric IDs
fmt\.Sprintf\("%[vg]".*[Ii][Dd]
```

## Review Checklist

Before approving:

### Breaking Changes
- [ ] No changes to resource type IDs
- [ ] No changes to entitlement slugs
- [ ] No changes to resource ID derivation

### Entity Sources (CRITICAL)
- [ ] Context (workspace/org) comes from **principal**, not entitlement
- [ ] In Grant: principal = WHO, entitlement = WHAT
- [ ] API arguments in correct order (check API docs for multi-string params)

### Pagination
- [ ] Token comes from API response, not hardcoded
- [ ] Has path where token becomes "" (termination)
- [ ] Bag initialized on first call (`if bag.Current() == nil`)

### Nil Safety
- [ ] HTTP response nil checks in error paths
- [ ] All type assertions use two-value form (`x, ok := ...`)
- [ ] ParentResourceId checked for nil before access

### Error Handling
- [ ] Errors returned, not just logged
- [ ] Error messages include connector prefix (`baton-service:`)
- [ ] Uses `%w` not `%v` for error wrapping

### Types
- [ ] API response IDs use `json.Number` not `string` (if API inconsistent)
- [ ] Service accounts/machine identities use App trait, not User
- [ ] Resource IDs use stable API identifiers (not email)
- [ ] Large numeric IDs use `%d` or `strconv`, not `%v` (avoid scientific notation)

### Security
- [ ] Secrets not logged
- [ ] Context passed to all API calls

### Resource Management
- [ ] Close() properly closes all client connections created in New()
- [ ] Grant/Revoke handles "already exists" as success (idempotent)

### Regression Risk
- [ ] If fixing a bug, does the fix preserve existing behavior for unaffected cases?
- [ ] If changing API mapping, are all downstream consumers considered?
