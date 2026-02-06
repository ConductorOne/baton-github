# ref-traits

Trait selection guide for resource types.

---

## Available Traits

| Trait | Purpose | Typical Resources |
|-------|---------|-------------------|
| User | Human identity | Users, accounts, members |
| Group | Collection of users | Groups, teams, departments |
| Role | Named permission set | Roles, permission groups |
| App | Non-human identity | Service accounts, API keys, apps |

---

## CRITICAL: User vs App (Caused Production Revert)

**The Rule:**
- **User** = Human being who logs in
- **App** = Everything else (systems, accounts, services, machines)

**Common Mistake**: Using User trait for "accounts" that aren't humans.

| Resource | WRONG | CORRECT | Why |
|----------|-------|---------|-----|
| AWS Account | User | **App** | AWS account is a container, not a person |
| Service Account | User | **App** | Machine identity, not human |
| API Key | User | **App** | Credential, not person |
| Bot User | User | **App** | Automated, not human |
| OAuth Client | User | **App** | Application identity |
| IAM Role | User | **App** | Assumed by services |

**The Revert (baton-aws #84)**: AWS Account IAM was modeled as User. Had to revert to App. The word "account" is ambiguous - AWS accounts are not user accounts.

**Ask**: "Can this entity log into a web browser and click things?" If no, it's probably an App.

---

## User Trait

For human identities that can authenticate.

```go
rs.NewUserResource(
    displayName,
    userResourceType,
    userID,
    []rs.UserTraitOption{
        rs.WithEmail(email, isPrimary),      // Email address
        rs.WithUserLogin(login),              // Login/username
        rs.WithStatus(v2.UserTrait_Status_STATUS_ENABLED),
        rs.WithUserProfile(map[string]interface{}{
            "first_name": firstName,
            "last_name":  lastName,
        }),
    },
)
```

**Required:** At least one of `WithEmail` or `WithUserLogin`

**Status values:**
- `STATUS_ENABLED` - Active user
- `STATUS_DISABLED` - Suspended/inactive
- `STATUS_DELETED` - Soft deleted

---

## Group Trait

For collections of users. Groups are grantable (users can be members).

```go
rs.NewGroupResource(
    displayName,
    groupResourceType,
    groupID,
    []rs.GroupTraitOption{
        rs.WithGroupProfile(map[string]interface{}{
            "description": description,
        }),
    },
    rs.WithParentResourceID(parentID),  // Optional hierarchy
)
```

**Entitlement pattern for groups:**
```go
func (g *groupBuilder) Entitlements(ctx context.Context, resource *v2.Resource,
    token *pagination.Token) ([]*v2.Entitlement, string, annotations.Annotations, error) {

    // Groups typically have a "member" entitlement
    return []*v2.Entitlement{
        sdkEntitlement.NewAssignmentEntitlement(
            resource,
            "member",
            sdkEntitlement.WithGrantableTo(userResourceType),
            sdkEntitlement.WithDisplayName("Member"),
            sdkEntitlement.WithDescription("Member of the group"),
        ),
    }, "", nil, nil
}
```

---

## Role Trait

For named permission sets. Roles define what actions are allowed.

```go
rs.NewRoleResource(
    displayName,
    roleResourceType,
    roleID,
    []rs.RoleTraitOption{
        rs.WithRoleProfile(map[string]interface{}{
            "permissions": permissions,
        }),
    },
)
```

**Entitlement pattern for roles:**
```go
func (r *roleBuilder) Entitlements(ctx context.Context, resource *v2.Resource,
    token *pagination.Token) ([]*v2.Entitlement, string, annotations.Annotations, error) {

    // Roles typically have an "assigned" entitlement
    return []*v2.Entitlement{
        sdkEntitlement.NewAssignmentEntitlement(
            resource,
            "assigned",
            sdkEntitlement.WithGrantableTo(userResourceType),
            sdkEntitlement.WithDisplayName("Assigned"),
            sdkEntitlement.WithDescription("Has this role assigned"),
        ),
    }, "", nil, nil
}
```

---

## App Trait

For non-human identities: service accounts, API keys, applications.

```go
rs.NewAppResource(
    displayName,
    appResourceType,
    appID,
    []rs.AppTraitOption{
        rs.WithAppProfile(map[string]interface{}{
            "client_id": clientID,
        }),
    },
)
```

**When to use App vs User:**
- **App:** API keys, service accounts, OAuth clients, integrations
- **User:** Human accounts, even if they authenticate via API key

---

## Trait Selection Decision Tree

```
Is it a human identity?
├─ Yes → User trait
│   └─ Can it be disabled/suspended? → WithStatus
└─ No
    ├─ Is it a collection of users? → Group trait
    ├─ Is it a permission set? → Role trait
    └─ Is it a service/app identity? → App trait
```

---

## Parent Resources

Use `WithParentResourceID` for hierarchy:

```go
// User belongs to an organization
rs.NewUserResource(name, userType, id, opts,
    rs.WithParentResourceID(&v2.ResourceId{
        ResourceType: "organization",
        Resource:     orgID,
    }),
)

// Group belongs to a workspace
rs.NewGroupResource(name, groupType, id, opts,
    rs.WithParentResourceID(&v2.ResourceId{
        ResourceType: "workspace",
        Resource:     wsID,
    }),
)
```

**When to use parents:**
- Multi-tenant systems (user belongs to org)
- Nested structures (group in workspace in org)
- Scoped permissions (role within project)

---

## Common Patterns

### SaaS with Organizations

```
Organization (no trait, or custom)
├── User (User trait, parent=org)
├── Group (Group trait, parent=org)
└── Role (Role trait, parent=org)
```

### DevOps Tool (GitHub-like)

```
Organization (no trait)
├── Team (Group trait, parent=org)
├── Repository (no trait, parent=org)
│   └── Collaborator role (Role trait, parent=repo)
└── Member (User trait, parent=org)
```

### Cloud Provider (AWS-like)

```
Account (no trait)
├── IAM User (User trait, parent=account)
├── IAM Group (Group trait, parent=account)
├── IAM Role (Role trait, parent=account)
└── Service Account (App trait, parent=account)
```

---

## Trait Options Reference

### User Trait Options

| Option | Purpose | C1 Usage |
|--------|---------|----------|
| `WithEmail(email, isPrimary)` | Email address | Identity matching |
| `WithUserLogin(login)` | Username/login | Identity matching |
| `WithStatus(status)` | Account status | Lifecycle management |
| `WithUserProfile(map)` | Additional fields | Display in UI |

### Group Trait Options

| Option | Purpose | C1 Usage |
|--------|---------|----------|
| `WithGroupProfile(map)` | Additional fields | Display in UI |

### Role Trait Options

| Option | Purpose | C1 Usage |
|--------|---------|----------|
| `WithRoleProfile(map)` | Additional fields | Display in UI |

### App Trait Options

| Option | Purpose | C1 Usage |
|--------|---------|----------|
| `WithAppProfile(map)` | Additional fields | Display in UI |

---

## What NOT to Sync

Not everything needs to be a resource:

- **Settings/preferences** - Not access-related
- **Audit logs** - Read-only, not grantable
- **Temporary tokens** - Ephemeral, not managed
- **System accounts** - Built-in, can't be modified

Ask: "Can C1 grant or revoke access to this?" If no, don't sync it.
