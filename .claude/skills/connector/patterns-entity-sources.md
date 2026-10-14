<!-- This file is managed by baton-admin. DO NOT EDIT. -->
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
| Context (workspace, org, tenant) | **Principal** (default) | `principal.ParentResourceId.Resource` |
| Context owned by the target resource itself | **Entitlement**, checked (see [Target-Owned Scope](#exception-target-owned-scope)) | `entitlement.Resource.ParentResourceId.Resource` after nil and parse checks |
| User/account ID | **Principal** | `principal.Id.Resource` |
| Permission/role being granted | **Entitlement** | `entitlement.Resource.Id.Resource` |
| Group being added to | **Entitlement** | `entitlement.Resource.Id.Resource` |

**Principal = WHO. Entitlement = WHAT.** WHERE comes from whoever owns the scope — almost always the principal.

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

## Exception: Target-Owned Scope

Ask who owns the scope:

- **Principal-scoped** — the context is a property of the user and can differ per user
  (the workspace a user belongs to). Take it from the principal. This is the default
  and covers the bug above.
- **Target-owned** — the target resource itself belongs to exactly one fixed scope,
  regardless of who the user is. A GitHub team belongs to one organization even when
  the user has no parent or belongs to several orgs. Here the principal's parent is
  not the right scope; the parent of the entitlement's resource (the team's org) is.

For target-owned scope, reading `entitlement.Resource.ParentResourceId` is correct
**only with checked routing**:

1. The parent was set by this connector's own sync (e.g. `List` sets every team's
   parent to the org it was listed under), not taken from elsewhere.
2. Nil-check the parent and return an error if it is missing.
3. Parse/validate the ID and return the error if it fails.
4. Call a vendor API scoped to both the scope and the target (e.g. org ID + team ID),
   so the call itself rejects a mismatched pair.

No extra membership lookup is needed. If you switch on the parent's `ResourceType`,
fail loudly on an unsupported type instead of falling through with a zero-value ID.

Simplified from baton-github `pkg/connector/team.go` at `d5fb8fdd` (Grant, lines 251-322):

```go
func (o *teamResourceType) Grant(ctx context.Context, principal *v2.Resource,
    entitlement *v2.Entitlement) (annotations.Annotations, error) {

    // WHAT: the team
    teamID, err := strconv.ParseInt(entitlement.Resource.Id.Resource, 10, 64)
    if err != nil {
        return nil, err
    }

    // WHERE: the team's own org. List() sets every team's parent to its org.
    if entitlement.GetResource().GetParentResourceId() == nil {
        return nil, fmt.Errorf("github-connectorv2: parent resource is required to grant team membership")
    }
    orgID, err := strconv.ParseInt(entitlement.Resource.ParentResourceId.Resource, 10, 64)
    if err != nil {
        return nil, err
    }

    // WHO: still the principal
    userID, err := strconv.ParseInt(principal.Id.Resource, 10, 64)
    if err != nil {
        return nil, err
    }
    user, resp, err := o.client.Users.GetByID(ctx, userID)
    // ...

    // org + team scoped call: GitHub rejects a team that is not in orgID
    _, resp, err = o.client.Teams.AddTeamMembershipByID(ctx, orgID, teamID, user.GetLogin(), opts)
    // ...
}
```

Revoke (lines 324-368) does the same through `grant.Entitlement`: nil-check, parse the
parent as the org ID, then `RemoveTeamMembershipByID(ctx, orgID, teamID, login)`.

The real Grant also switches on the parent's type, with a branch that reads the org ID
from the team's profile when the parent is a team. That is a leftover from before
baton-github flattened nested teams under their org (commit `ab59c2a`); it is not one
of the checks above.

**Still wrong:**
- A bare, unchecked `entitlement.Resource.ParentResourceId.Resource` read — for any type.
- Entitlement-resource-parent context in a principal-scoped (workspace-style) operation.
- **The mirror bug:** routing a target-owned operation through `principal.ParentResourceId`.
  A parentless or multi-org user gives no valid org for the team. This does not match
  the grep below, so reviewers must catch it by reading the code.

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
1. `entitlement.Resource.ParentResourceId` - usually should be `principal.ParentResourceId`; acceptable only for checked [target-owned scope](#exception-target-owned-scope). Each hit needs a human read — grep cannot tell the two apart.
2. Multiple `string` parameters in API calls - verify order
3. Getting "context" (workspace/org/tenant) from entitlement for a principal-scoped operation
4. Routing a target-owned operation (e.g. team membership) through the principal's parent

**Questions to ask:**
- "Where does the workspace ID come from?"
- "Does that scope belong to the user, or to the target resource?"
- "Is this getting data from the right entity?"
- "Does the argument order match the API signature?"

---

## Test Cases

Test these scenarios:
1. Principal in workspace A, entitlement in workspace B - should use workspace A
2. Arguments in correct order for API
3. Grant to correct user (not some other user)
4. Revoke from correct user
5. Target-owned scope: principal with no parent (or several orgs) still grants in the target's own org; missing or unparseable parent returns an error

```go
func TestGrant_UsesCorrectWorkspace(t *testing.T) {
    // Principal is in workspace "ws-user"
    // Entitlement is in workspace "ws-entitlement"
    // Grant should happen in "ws-user"
}
```
