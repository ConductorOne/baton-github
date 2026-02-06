# build-provisioning

Implementing Grant, Revoke, and account operations.

---

## Grant Interface

```go
func (g *groupBuilder) Grant(ctx context.Context, principal *v2.Resource,
    entitlement *v2.Entitlement) ([]*v2.Grant, annotations.Annotations, error)
```

**Parameters:**
- `principal` - Who is receiving the grant (usually a user)
- `entitlement` - What permission is being granted

---

## Grant Implementation

```go
func (g *groupBuilder) Grant(ctx context.Context, principal *v2.Resource,
    entitlement *v2.Entitlement) ([]*v2.Grant, annotations.Annotations, error) {

    // 1. Validate principal type
    if principal.Id.ResourceType != "user" {
        return nil, nil, fmt.Errorf("baton-myservice: only users can be granted group membership")
    }

    // 2. Extract IDs - use ExternalId for the native system identifier
    groupID := entitlement.Resource.Id.Resource

    // Get native user ID from ExternalId (required for provisioning)
    externalId := principal.GetExternalId()
    if externalId == nil {
        return nil, nil, fmt.Errorf("baton-myservice: principal missing external ID")
    }
    nativeUserID := externalId.Id

    // 3. Call API to add membership using native ID
    err := g.client.AddGroupMember(ctx, groupID, nativeUserID)
    if err != nil {
        // 4. Handle "already exists" as success (idempotency)
        if isAlreadyExistsError(err) {
            grant := sdkGrant.NewGrant(entitlement.Resource, entitlement.Slug, principal.Id)
            return []*v2.Grant{grant}, annotations.New(&v2.GrantAlreadyExists{}), nil
        }
        return nil, nil, fmt.Errorf("baton-myservice: failed to add group member: %w", err)
    }

    // 5. Return the created grant
    grant := sdkGrant.NewGrant(entitlement.Resource, entitlement.Slug, principal.Id)
    return []*v2.Grant{grant}, nil, nil
}
```

**Note on ExternalId:** During sync, set `WithExternalID()` with the native system identifier. During provisioning, retrieve it via `GetExternalId()` to make API calls. See `concepts-identifiers.md` for details.

---

## Revoke Interface

```go
func (g *groupBuilder) Revoke(ctx context.Context, grant *v2.Grant) (annotations.Annotations, error)
```

**Parameters:**
- `grant` - Contains both principal and entitlement info

---

## Revoke Implementation

```go
func (g *groupBuilder) Revoke(ctx context.Context, grant *v2.Grant) (annotations.Annotations, error) {

    // 1. Extract IDs from grant - use ExternalId for native identifier
    groupID := grant.Entitlement.Resource.Id.Resource

    // Get native user ID from ExternalId
    externalId := grant.Principal.GetExternalId()
    if externalId == nil {
        return nil, fmt.Errorf("baton-myservice: principal missing external ID")
    }
    nativeUserID := externalId.Id

    // 2. Call API to remove membership using native ID
    err := g.client.RemoveGroupMember(ctx, groupID, nativeUserID)
    if err != nil {
        // 3. Handle "not found" as success (idempotency)
        if isNotFoundError(err) {
            return annotations.New(&v2.GrantAlreadyRevoked{}), nil
        }
        return nil, fmt.Errorf("baton-myservice: failed to remove group member: %w", err)
    }

    return nil, nil
}
```

---

## Idempotency Requirements

**Grant must handle "already exists":**
```go
if isAlreadyExistsError(err) {
    // Return success with the grant
    grant := sdkGrant.NewGrant(...)
    return []*v2.Grant{grant}, nil, nil
}
```

**Revoke must handle "not found":**
```go
if isNotFoundError(err) {
    // Return success - desired state achieved
    return nil, nil
}
```

**Rationale:** Operations may be retried. Failing on "already done" causes unnecessary retry storms.

---

## Entity Source Pattern (CRITICAL)

In Grant/Revoke, data comes from two sources. Use the right one.

**Principal** provides context (who):
```go
// Context (workspace, org, tenant) comes from principal
workspaceID := principal.ParentResourceId.Resource

// Native user ID for API calls comes from ExternalId
externalId := principal.GetExternalId()
nativeUserID := externalId.Id
```

**Entitlement** provides target (what):
```go
// The permission/role being granted comes from entitlement
roleID := entitlement.Resource.Id.Resource
groupID := entitlement.Resource.Id.Resource
```

**WRONG - caused 3 production reverts:**
```go
// Getting workspace from entitlement instead of principal
workspaceID := entitlement.Resource.ParentResourceId.Resource  // WRONG!
```

**ALSO WRONG - missing ExternalId:**
```go
// Using ResourceId instead of native ID may not work for API calls
userID := principal.Id.Resource  // May not be what target API expects
```

---

## AccountManager Interface

For creating/deleting user accounts:

```go
type AccountManager interface {
    CreateAccount(ctx context.Context, resource *v2.AccountInfo) (
        *v2.CreateAccountResponse, annotations.Annotations, error)
    DeleteResource(ctx context.Context, resourceId *v2.ResourceId) (
        annotations.Annotations, error)
}
```

---

## CreateAccount Implementation

```go
func (u *userBuilder) CreateAccount(ctx context.Context, accountInfo *v2.AccountInfo) (
    *v2.CreateAccountResponse, annotations.Annotations, error) {

    // 1. Extract account info
    email := accountInfo.Email
    login := accountInfo.Login

    // 2. Create user via API
    newUser, err := u.client.CreateUser(ctx, email, login)
    if err != nil {
        return nil, nil, fmt.Errorf("baton-myservice: failed to create user: %w", err)
    }

    // 3. Build resource for the new user
    resource, err := rs.NewUserResource(
        newUser.Name,
        userResourceType,
        newUser.ID,
        []rs.UserTraitOption{rs.WithEmail(email, true)},
    )
    if err != nil {
        return nil, nil, err
    }

    return &v2.CreateAccountResponse{
        Resource: resource,
    }, nil, nil
}
```

---

## Capability Declaration

Provisioning capabilities must be declared:

```go
func (c *Connector) Metadata(ctx context.Context) (*v2.ConnectorMetadata, error) {
    return &v2.ConnectorMetadata{
        DisplayName: "My Service",
        Capabilities: []v2.ConnectorCapability{
            v2.ConnectorCapability_CONNECTOR_CAPABILITY_SYNC,
            v2.ConnectorCapability_CONNECTOR_CAPABILITY_PROVISIONING,
        },
    }, nil
}
```

Resource types also declare capabilities in `baton_capabilities.json` (auto-generated).
