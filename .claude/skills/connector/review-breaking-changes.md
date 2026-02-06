# review-breaking-changes

Breaking change detection rules for connector reviews.

---

## What Breaks Downstream

When a connector changes, these break C1 sync:

| Change | Impact | Severity |
|--------|--------|----------|
| Resource type name change | Existing grants orphaned | Critical |
| Entitlement slug change | Grant matching fails | Critical |
| Resource ID format change | Duplicate resources created | Critical |
| Removed resource type | Grants disappear | High |
| Changed parent hierarchy | Relationships break | High |
| Removed entitlement | Grants can't be revoked | High |
| Changed trait type | C1 UI breaks | Medium |

---

## Resource Type Changes

**Breaking - type name:**
```go
// BEFORE
var userResourceType = &v2.ResourceType{
    Id:          "user",  // This is the identity
    DisplayName: "User",
}

// AFTER - BREAKING
var userResourceType = &v2.ResourceType{
    Id:          "account",  // Changed! All grants orphaned
    DisplayName: "User Account",
}
```

**Safe - display name only:**
```go
// BEFORE
var userResourceType = &v2.ResourceType{
    Id:          "user",
    DisplayName: "User",
}

// AFTER - Safe
var userResourceType = &v2.ResourceType{
    Id:          "user",  // Same
    DisplayName: "User Account",  // Display change is fine
}
```

---

## Entitlement Changes

**Breaking - slug change:**
```go
// BEFORE
sdkEntitlement.NewAssignmentEntitlement(resource, "member", ...)

// AFTER - BREAKING
sdkEntitlement.NewAssignmentEntitlement(resource, "membership", ...)
```

The slug `"member"` vs `"membership"` breaks grant matching.

**Breaking - permission entitlement change:**
```go
// BEFORE
sdkEntitlement.NewPermissionEntitlement(resource, "admin", ...)

// AFTER - BREAKING
sdkEntitlement.NewPermissionEntitlement(resource, "administrator", ...)
```

---

## Resource ID Changes

**Breaking - ID derivation:**
```go
// BEFORE
rs.NewUserResource(user.Name, userType, user.ID, ...)

// AFTER - BREAKING
rs.NewUserResource(user.Name, userType, user.Email, ...)
```

IDs must be stable. Changing how they're derived creates duplicates.

**Breaking - ID format:**
```go
// BEFORE
resourceID := user.ID

// AFTER - BREAKING
resourceID := fmt.Sprintf("user:%s", user.ID)
```

---

## Hierarchy Changes

**Breaking - parent resource change:**
```go
// BEFORE - users under organization
rs.NewUserResource(name, userType, id, []rs.UserTraitOption{},
    rs.WithParentResourceID(&v2.ResourceId{
        ResourceType: "organization",
        Resource:     orgID,
    }),
)

// AFTER - BREAKING - users under workspace
rs.NewUserResource(name, userType, id, []rs.UserTraitOption{},
    rs.WithParentResourceID(&v2.ResourceId{
        ResourceType: "workspace",  // Changed parent type
        Resource:     wsID,
    }),
)
```

---

## Trait Changes

**Breaking - trait type change:**
```go
// BEFORE - User trait
rs.NewUserResource(name, userType, id, []rs.UserTraitOption{
    rs.WithEmail(email, true),
})

// AFTER - BREAKING - App trait
rs.NewAppResource(name, appType, id, []rs.AppTraitOption{})
```

**Safe - adding trait options:**
```go
// BEFORE
rs.NewUserResource(name, userType, id, []rs.UserTraitOption{
    rs.WithEmail(email, true),
})

// AFTER - Safe
rs.NewUserResource(name, userType, id, []rs.UserTraitOption{
    rs.WithEmail(email, true),
    rs.WithUserLogin(login),  // Adding is safe
})
```

---

## Detection Patterns

**Search for these in diffs:**

```
# Resource type ID changes
-    Id:          "
+    Id:          "

# Entitlement slug changes
- NewAssignmentEntitlement(resource, "
+ NewAssignmentEntitlement(resource, "

- NewPermissionEntitlement(resource, "
+ NewPermissionEntitlement(resource, "

# Resource ID derivation
- rs.NewUserResource(*, *, user.ID,
+ rs.NewUserResource(*, *, user.Email,

# Parent resource changes
- ResourceType: "organization",
+ ResourceType: "workspace",
```

---

## CodeRabbit/BUGBOT Rules

For automated detection, configure these patterns:

```yaml
# .coderabbit.yaml or BUGBOT.md
reviews:
  path_filters:
    - "pkg/**/*.go"
    - "cmd/**/*.go"

  high_severity_patterns:
    - pattern: 'Id:\s*"[^"]+"\s*,\s*//.*change'
      message: "Resource type ID change detected - this breaks existing grants"

    - pattern: 'NewAssignmentEntitlement\([^,]+,\s*"[^"]+"'
      message: "Entitlement slug change - verify this doesn't break grant matching"

    - pattern: 'NewPermissionEntitlement\([^,]+,\s*"[^"]+"'
      message: "Permission entitlement change - verify slug stability"
```

---

## Review Questions

When reviewing changes to resource types or entitlements:

1. **"Does this change any identifier that C1 uses for matching?"**
   - Resource type ID
   - Entitlement slug
   - Resource ID derivation

2. **"If this deploys, what happens to existing data?"**
   - Existing grants matched by old identifiers
   - Users/groups with old resource IDs

3. **"Is there a migration path?"**
   - Can old and new coexist temporarily?
   - Is there a way to map old IDs to new?

---

## Safe Changes

These are NOT breaking:

- Display name changes (human-readable only)
- Description changes
- Adding new resource types
- Adding new entitlements
- Adding new trait options to existing resources
- Adding new fields to API responses
- Improving error messages
- Performance optimizations
- Adding pagination to unpaginated endpoints

---

## Migration Guidance

If a breaking change is necessary:

1. **Document the break** in PR description
2. **Coordinate with C1 team** for sync timing
3. **Consider dual-emit** temporarily (old + new IDs)
4. **Plan grant re-sync** after deployment
5. **Update any hardcoded references** in C1 config
