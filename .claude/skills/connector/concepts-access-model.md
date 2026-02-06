# concepts-access-model

What connectors sync: resources, entitlements, and grants.

---

## Purpose of Connectors

Connectors answer: **Who has access to what?**

This is for SOC2 compliance and access control, not operational data.

**Sync:** Users with admin roles, permission assignments, group memberships
**Don't sync:** Customer data, sales records, tickets, email contents

---

## Three Core Concepts

### Resources

Things that exist in the target system.

```go
resource, err := rs.NewUserResource(
    "Alice Smith",           // displayName
    userResourceType,        // type
    "user-123",              // objectID (stable!)
    []rs.UserTraitOption{
        rs.WithEmail("alice@example.com", true),
        rs.WithStatus(v2.UserTrait_Status_STATUS_ENABLED),
    },
)
```

### Entitlements

Permissions that can be granted.

```go
entitlement := sdkEntitlement.NewAssignmentEntitlement(
    resource,                // the resource offering this entitlement
    "member",                // slug (stable identifier)
    sdkEntitlement.WithDisplayName("Member"),
    sdkEntitlement.WithGrantableTo(userResourceType),
)
```

### Grants

Assignments of entitlements to principals.

```go
grant := sdkGrant.NewGrant(
    resource,                // the resource with the entitlement
    "member",                // entitlement slug
    &v2.ResourceId{          // principal receiving the grant
        ResourceType: "user",
        Resource:     "user-456",
    },
)
```

---

## Resource Traits

Traits tell the platform how to interpret resources.

| Trait | Use For | Platform Behavior |
|-------|---------|-------------------|
| `TRAIT_USER` | Human users | Identity correlation, access reviews |
| `TRAIT_GROUP` | Collections | Membership expansion |
| `TRAIT_ROLE` | Permissions | Permission aggregation |
| `TRAIT_APP` | Applications, service accounts | App catalog, machine identities |

**Common mistake:** Using TRAIT_USER for service accounts or AWS accounts. These are TRAIT_APP.

---

## Standard Resource Types

Every connector should have:

### Users (TRAIT_USER)
- Human users who receive grants
- Usually no entitlements (they don't grant to others)
- `Entitlements()` returns empty
- `Grants()` returns empty

### Groups (TRAIT_GROUP)
- Collections of users
- Entitlement: "member"
- `Grants()` returns who is a member

### Roles (TRAIT_ROLE)
- Permission definitions
- Entitlement: "assigned"
- `Grants()` returns who has the role

---

## Entitlement Patterns

**Assignment entitlements** - membership in something:
```go
sdkEntitlement.NewAssignmentEntitlement(resource, "member", ...)
```

**Permission entitlements** - capability grants:
```go
sdkEntitlement.NewPermissionEntitlement(resource, "admin", ...)
```

---

## Grant Expansion

When a group has a role, users in the group inherit the role.

```go
// Group "admins" has role "super-user"
// Mark the grant as expandable so C1 expands to group members
grantOptions := []grant.GrantOption{
    grant.WithAnnotation(&v2.GrantExpandable{
        EntitlementIds: []string{"group:member"},
        Shallow:        true,
    }),
}

grant := sdkGrant.NewGrant(resource, "assigned", groupResourceId, grantOptions...)
```

The SDK handles expanding this to individual users.

---

## What NOT to Sync

| Sync | Don't Sync |
|------|------------|
| Users with licenses | Customer records |
| Admin roles | Sales opportunities |
| Permission assignments | Email contents |
| Group memberships | Project tasks |
| API access levels | Audit logs (unless access-related) |

Focus on: Who can administer? Who has elevated privileges? Who can modify critical configs?
