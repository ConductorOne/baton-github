# Change Type Detection and Guidance

When working on a connector, first identify what type of change you're making. Each type has specific patterns and pitfalls.

## Quick Reference

| Change Type | Detection Signal | Severity | Key Risk |
|-------------|-----------------|----------|----------|
| SDK Upgrade | `go.mod` baton-sdk version | Medium | Breaking changes |
| Pagination Fix | `List()`, page tokens | **High** | Data loss or infinite loop |
| Panic Fix | nil checks, type assertions | **High** | Production crashes |
| Provisioning Fix | `Grant()`, `Revoke()` | **High** | Wrong entity source |
| New Resource Type | new `*_builder.go` file | Medium | Breaking existing sync |
| New Provisioning | adding Grant/Revoke methods | Medium | Missing ExternalId |
| Linter Fix | style, formatting, lint errors | Low | None |
| CI/Release Fix | goreleaser, workflows | Low | None |
| Config Change | `config.go`, flags | Medium | Breaking existing deployments |

---

## SDK Upgrade

**Detection**: Changes to `go.mod` with `baton-sdk` version bump.

**Checklist**:
- [ ] Read SDK changelog for breaking changes
- [ ] Run full test suite (if exists)
- [ ] Verify pagination still terminates
- [ ] Check for deprecated function warnings
- [ ] Test sync output matches previous version

**Common Issues**:
- New required fields in ResourceBuilder
- Changed pagination token handling
- Renamed or moved interfaces

**Example**: Upgrading from v0.2.x to v0.4.x often requires:
```go
// Old: implicit resource type registration
// New: explicit registration in ResourceTypes()
func (c *Connector) ResourceTypes(ctx context.Context) ([]*v2.ResourceType, error) {
    return []*v2.ResourceType{userResourceType, groupResourceType}, nil
}
```

---

## Pagination Fix

**Detection**: Changes to `List()` methods, page token handling, or `pagination.Bag`.

**This is HIGH SEVERITY** - pagination bugs cause data loss (missing resources) or infinite loops (never-ending sync).

**Two Failure Modes**:

1. **Early Termination** - Sync stops too soon, missing pages:
```go
// WRONG - always returns empty token
return resources, "", nil, nil
```

2. **Infinite Loop** - Sync never stops:
```go
// WRONG - hardcoded token
return resources, "next", nil, nil

// WRONG - nextPage never becomes empty
return resources, resp.NextPage, nil, nil  // if API returns cursor even on last page
```

**Correct Pattern**:
```go
func (b *userBuilder) List(ctx context.Context, parentResourceID *v2.ResourceId, pt *pagination.Token) (
    []*v2.Resource, string, annotations.Annotations, error,
) {
    bag, page, err := parsePageToken(pt.Token, &v2.ResourceId{})
    if err != nil {
        return nil, "", nil, err
    }

    // CRITICAL: Initialize bag on first call
    if bag.Current() == nil {
        bag.Push(pagination.PageState{ResourceTypeID: resourceTypeUser.Id})
    }

    resp, err := c.client.ListUsers(ctx, page)
    if err != nil {
        return nil, "", nil, fmt.Errorf("baton-service: listing users: %w", err)
    }

    // ... build resources ...

    // CRITICAL: Only pass through API's token, detect termination
    if resp.NextPage == "" {
        return resources, "", nil, nil  // Done
    }
    return resources, resp.NextPage, nil, nil  // More pages
}
```

**Verification**:
- [ ] Token comes from API response, not hardcoded
- [ ] There exists a path where token becomes "" (termination)
- [ ] Bag is initialized on first call (`if bag.Current() == nil`)
- [ ] API's "no more pages" signal is correctly detected

---

## Panic Fix

**Detection**: Changes adding nil checks, fixing type assertions, or addressing runtime panics.

**This is HIGH SEVERITY** - panics crash production syncs.

**Common Panic Sources**:

1. **HTTP Response nil on error**:
```go
// WRONG - resp may be nil when err != nil
resp, err := client.Do(req)
if err != nil {
    log.Printf("status: %d", resp.StatusCode)  // PANIC
}

// CORRECT
resp, err := client.Do(req)
if err != nil {
    if resp != nil {
        defer resp.Body.Close()
    }
    return fmt.Errorf("baton-service: request failed: %w", err)
}
defer resp.Body.Close()
```

2. **Type assertion without ok check**:
```go
// WRONG - panics if wrong type
value := data["key"].(string)

// CORRECT
value, ok := data["key"].(string)
if !ok {
    return fmt.Errorf("baton-service: expected string for key")
}
```

3. **Pagination bag not initialized**:
```go
// WRONG - panics on first call
token := bag.Current().Token

// CORRECT
if bag.Current() == nil {
    bag.Push(pagination.PageState{ResourceTypeID: resourceType.Id})
}
```

4. **Nil ParentResourceId**:
```go
// WRONG - may panic
orgID := resource.ParentResourceId.Resource

// CORRECT
if resource.ParentResourceId == nil {
    return fmt.Errorf("baton-service: resource has no parent")
}
orgID := resource.ParentResourceId.Resource
```

---

## Provisioning Fix

**Detection**: Changes to `Grant()`, `Revoke()`, or entity source logic.

**This is HIGH SEVERITY** - wrong provisioning can grant/revoke wrong access.

**The Entity Source Rule**:
- **WHO** (principal): The user/service being granted access
- **WHAT** (entitlement): The permission being granted
- **WHERE** (context): The org/workspace scope - **ALWAYS from principal**

```go
func (g *groupBuilder) Grant(ctx context.Context, principal *v2.Resource,
    entitlement *v2.Entitlement) ([]*v2.Grant, annotations.Annotations, error) {

    // WHO - get native ID from ExternalId
    externalId := principal.GetExternalId()
    if externalId == nil {
        return nil, nil, fmt.Errorf("baton-service: principal missing external ID")
    }
    nativeUserID := externalId.Id

    // WHAT - from entitlement
    groupID := entitlement.Resource.Id.Resource

    // WHERE - CRITICAL: from principal, NOT entitlement
    workspaceID := principal.ParentResourceId.Resource  // CORRECT
    // workspaceID := entitlement.Resource.ParentResourceId.Resource  // WRONG

    // Make API call
    err := c.client.AddMember(ctx, workspaceID, groupID, nativeUserID)
    // ...
}
```

**Idempotency**:
```go
// "Already exists" is SUCCESS, not error
if isAlreadyExistsError(err) {
    return nil, annotations.New(&v2.GrantAlreadyExists{}), nil
}

// "Not found" on revoke is SUCCESS
if isNotFoundError(err) {
    return annotations.New(&v2.GrantAlreadyRevoked{}), nil
}
```

**Checklist**:
- [ ] Context (workspace/org) comes from principal, not entitlement
- [ ] ExternalId is used for API calls, not ResourceId
- [ ] Already-exists handled as success
- [ ] Not-found on revoke handled as success
- [ ] Error messages include connector prefix

---

## New Resource Type

**Detection**: New `*_builder.go` file or new entry in `ResourceTypes()`.

**Breaking Change Risk**: Adding resource types is safe. Removing or renaming is NEVER safe.

**Checklist**:
- [ ] Resource ID is stable (uses API ID, not mutable fields like email)
- [ ] ResourceType is registered in `ResourceTypes()`
- [ ] If no entitlements/grants, add `SkipEntitlementsAndGrants` annotation
- [ ] ExternalId set if resource will be used in provisioning

**Template**:
```go
var resourceTypeWidget = &v2.ResourceType{
    Id:          "widget",
    DisplayName: "Widget",
    Traits:      []v2.ResourceType_Trait{v2.ResourceType_TRAIT_GROUP},
    Annotations: annotationsForResourceType("widget"),
}

type widgetBuilder struct {
    client *Client
}

func (b *widgetBuilder) ResourceType(ctx context.Context) *v2.ResourceType {
    return resourceTypeWidget
}

func (b *widgetBuilder) List(ctx context.Context, parentResourceID *v2.ResourceId,
    pt *pagination.Token) ([]*v2.Resource, string, annotations.Annotations, error) {
    // Implementation
}

func (b *widgetBuilder) Entitlements(ctx context.Context, resource *v2.Resource,
    pt *pagination.Token) ([]*v2.Entitlement, string, annotations.Annotations, error) {
    // Implementation or return nil, "", nil, nil if skipped
}

func (b *widgetBuilder) Grants(ctx context.Context, resource *v2.Resource,
    pt *pagination.Token) ([]*v2.Grant, string, annotations.Annotations, error) {
    // Implementation or return nil, "", nil, nil if skipped
}
```

---

## New Provisioning

**Detection**: Adding `Grant()` or `Revoke()` methods to existing builder.

**Checklist**:
- [ ] ExternalId is set during sync (required for provisioning to work)
- [ ] Context comes from principal
- [ ] Idempotency handled (already-exists, already-revoked)
- [ ] capabilities.json updated if needed

**Critical**: If `WithExternalID()` wasn't set during sync, provisioning will fail:
```go
// During sync - REQUIRED for provisioning
resource, err := rs.NewUserResource(
    user.Name,
    resourceTypeUser,
    user.ID,
    []rs.UserTraitOption{rs.WithEmail(user.Email, true)},
    rs.WithExternalID(&v2.ExternalId{Id: user.NativeID}),  // CRITICAL
)
```

---

## Linter Fix

**Detection**: Changes only to formatting, imports, or style.

**This is LOW SEVERITY** - but verify no logic changes snuck in.

**Common Linter Issues**:
- Unused variables/imports
- Error return not checked
- Deprecated functions
- Line length

**Checklist**:
- [ ] Only formatting/style changes, no logic changes
- [ ] Tests still pass (if any)

---

## CI/Release Fix

**Detection**: Changes to `.goreleaser.yml`, `.github/workflows/`, or build configuration.

**This is LOW SEVERITY** for connector logic, but can break releases.

**Common Issues**:
- Go version mismatch between workflow and goreleaser
- Missing environment variables
- Changed artifact paths

**Checklist**:
- [ ] Go version consistent across all configs
- [ ] Release workflow triggers on correct events
- [ ] Artifact names haven't changed (breaks downstream)

---

## Config Change

**Detection**: Changes to `config.go`, command-line flags, or environment variables.

**Breaking Change Risk**: Removing or renaming config fields breaks existing deployments.

**Checklist**:
- [ ] New fields have sensible defaults (don't break existing configs)
- [ ] Removed fields are deprecated first (warn, then remove)
- [ ] Field names are consistent (`WithDisplayName` for all fields)
- [ ] Validation added for required fields
- [ ] Documentation updated

**Example - Adding a field safely**:
```go
type Config struct {
    Token     string `mapstructure:"token"`
    BaseURL   string `mapstructure:"base_url"`
    // New field with default - won't break existing configs
    PageSize  int    `mapstructure:"page_size"`
}

func (c *Config) Validate() error {
    if c.Token == "" {
        return fmt.Errorf("token is required")
    }
    if c.PageSize == 0 {
        c.PageSize = 100  // Sensible default
    }
    return nil
}
```

---

## Detection Workflow

When starting work, identify the change type:

1. **Look at the files changed**:
   - `go.mod` -> SDK Upgrade
   - `*_builder.go` List methods -> Pagination
   - `*_builder.go` Grant/Revoke -> Provisioning
   - `config.go` -> Config Change
   - `.goreleaser.yml` -> CI/Release

2. **Look at the symptoms**:
   - "Sync hangs" -> Pagination (infinite loop)
   - "Missing resources" -> Pagination (early termination)
   - "Panic in production" -> Nil pointer or type assertion
   - "Wrong user got access" -> Entity source confusion
   - "Provisioning fails" -> Missing ExternalId

3. **Apply the relevant checklist** from this document.

4. **Test the specific risk** for that change type.
