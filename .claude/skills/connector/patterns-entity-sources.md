# patterns-entity-sources

CRITICAL: Which entity provides which data in Grant/Revoke operations.

---

## The Problem

Grant and Revoke operations receive two entities:
- **Principal** - who is receiving/losing access
- **Entitlement** - what access is being granted/revoked

Confusion about which entity provides which data caused 3 production reverts and is the #1 high-impact bug pattern.

---

## The Rule

| Data Type | Source | Example |
|-----------|--------|---------|
| Context (workspace, org, tenant) | **Principal** | `principal.ParentResourceId.Resource` |
| User/account ID | **Principal** | `principal.Id.Resource` |
| Permission/role being granted | **Entitlement** | `entitlement.Resource.Id.Resource` |
| Group being added to | **Entitlement** | `entitlement.Resource.Id.Resource` |

**Principal = WHO. Entitlement = WHAT.**

---

## Correct Pattern

```go
func (g *groupBuilder) Grant(ctx context.Context, principal *v2.Resource,
    entitlement *v2.Entitlement) ([]*v2.Grant, annotations.Annotations, error) {

    // WHO is receiving the grant
    userID := principal.Id.Resource

    // WHAT is being granted
    groupID := entitlement.Resource.Id.Resource

    // Context comes from principal (the user's workspace)
    var workspaceID string
    if principal.ParentResourceId != nil {
        workspaceID = principal.ParentResourceId.Resource
    }

    // API call uses correct values
    err := g.client.AddMember(ctx, workspaceID, groupID, userID)
    // ...
}
```

---

## Wrong Pattern (Caused Reverts)

```go
func (g *groupBuilder) Grant(ctx context.Context, principal *v2.Resource,
    entitlement *v2.Entitlement) ([]*v2.Grant, annotations.Annotations, error) {

    // WRONG: Getting workspace from entitlement
    workspaceID := entitlement.Resource.ParentResourceId.Resource  // BUG!

    // This grants access in the WRONG workspace
    err := g.client.AddMember(ctx, workspaceID, groupID, userID)
}
```

This bug grants user access in the entitlement's workspace, not the user's workspace.

---

## Revoke Pattern

In Revoke, the grant contains both:

```go
func (g *groupBuilder) Revoke(ctx context.Context, grant *v2.Grant) (annotations.Annotations, error) {

    // Principal is in grant.Principal
    userID := grant.Principal.Id.Resource

    // Entitlement is in grant.Entitlement
    groupID := grant.Entitlement.Resource.Id.Resource

    // Context still comes from principal
    var workspaceID string
    if grant.Principal.ParentResourceId != nil {
        workspaceID = grant.Principal.ParentResourceId.Resource
    }

    err := g.client.RemoveMember(ctx, workspaceID, groupID, userID)
    // ...
}
```

---

## API Argument Order (6+ PRs with this bug)

Functions with multiple `string` parameters of the same type are easy to call incorrectly.

**The Problem**:
```go
// API client signature
func (c *Client) AddMember(groupID, userID string) error

// Connector code - which is which?
err := client.AddMember(principal.Id.Resource, entitlement.Resource.Id.Resource)
// Is this (userID, groupID) or (groupID, userID)? Easy to get wrong.
```

**WRONG - Arguments swapped**:
```go
// Grants userID to... userID? And groupID gets added to group groupID?
err := client.AddMember(userID, groupID)  // BACKWARDS!
```

**CORRECT - Match API signature**:
```go
// Check API docs: AddMember(groupID, userID)
err := client.AddMember(groupID, userID)
```

**Prevention - Use named variables**:
```go
func (g *groupBuilder) Grant(...) {
    // Name variables to match their PURPOSE
    targetGroupID := entitlement.Resource.Id.Resource  // WHAT
    userToAdd := principal.Id.Resource                  // WHO

    // Now the call is self-documenting
    err := g.client.AddMember(targetGroupID, userToAdd)
}
```

**Prevention - Verify against API docs**:
```go
// Before writing the call, check:
// 1. Open API documentation
// 2. Find the exact parameter order
// 3. Write a comment if order is non-obvious:
//    AddMember adds userID to groupID (not the other way around)
err := client.AddMember(groupID, userID)
```

**Code smell**: Multiple adjacent `string` parameters = high swap risk.

---

## Detection in Code Review

**Red flags:**
1. `entitlement.Resource.ParentResourceId` - probably should be `principal.ParentResourceId`
2. Multiple `string` parameters in API calls - verify order
3. Getting "context" (workspace/org/tenant) from entitlement

**Questions to ask:**
- "Where does the workspace ID come from?"
- "Is this getting data from the right entity?"
- "Does the argument order match the API signature?"

---

## Test Cases

Test these scenarios:
1. Principal in workspace A, entitlement in workspace B - should use workspace A
2. Arguments in correct order for API
3. Grant to correct user (not some other user)
4. Revoke from correct user

```go
func TestGrant_UsesCorrectWorkspace(t *testing.T) {
    // Principal is in workspace "ws-user"
    // Entitlement is in workspace "ws-entitlement"
    // Grant should happen in "ws-user"
}
```
