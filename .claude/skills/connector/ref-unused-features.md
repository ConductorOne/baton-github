# ref-unused-features

SDK features that C1 does not currently use. Don't invest effort here.

---

## Why This Matters

The baton-sdk exposes many fields and options. Some exist for:
- Future features not yet implemented
- Legacy compatibility
- Aspirational API design

Setting these fields wastes connector development time with no benefit.

---

## User Trait: Field Usage

| Field | SDK Function | C1 Status |
|-------|-------------|-----------|
| ExternalID | `WithExternalID()` | **REQUIRED for provisioning** |
| Email | `WithEmail()` | **Required** - identity matching |
| Login | `WithUserLogin()` | **Required** - identity matching |
| Status | `WithStatus()` | **Required** - lifecycle |
| Profile | `WithUserProfile()` | Used for display |
| AccountType | `WithAccountType()` | Rarely used |
| MfaEnabled/MfaStatus | (no helper) | IDP connectors only |
| SsoEnabled/SsoSource | (no helper) | IDP connectors only |
| StructuredName | (no helper) | Rarely used |
| Icon | `WithUserIcon()` | Rarely used |
| Managers | (no helper) | Rarely used |
| Sources | (no helper) | Rarely used |

**Critical:** `WithExternalID()` is required for provisioning to work. It stores the native system identifier that Grant/Revoke operations need to call the target API.

---

## ExternalID - Required for Provisioning

`WithExternalID()` stores the **native system identifier** needed for provisioning operations.

| Concept | Where | C1 Uses? |
|---------|-------|----------|
| `Resource.ExternalId` | Resource field | **YES - provisioning** |
| `match_baton_id` | C1 sync config | Yes, for resource matching |
| `ResourceId.Resource` | Primary identifier | **Yes** - sync matching |

**The flow:**
1. During sync: Set `WithExternalID(&v2.ExternalId{Id: nativeID})`
2. C1 stores it with the resource
3. During provisioning: SDK passes it to Grant/Revoke
4. Connector calls `principal.GetExternalId().Id` to get the native ID for API calls

**Recommendation:** Always set `WithExternalID()` for resources that may be involved in provisioning.

---

## Group Trait: Unused Fields

| Field | C1 Status |
|-------|-----------|
| Icon | **Ignored** |
| GroupSources | **Ignored** |

**What C1 reads:**
- DisplayName
- Profile (for display)

---

## Entitlement: Unused Options

| Option | SDK Function | C1 Status |
|--------|-------------|-----------|
| Purpose | `WithPurpose()` | **Ignored** |
| RiskLevel | `WithRiskLevel()` | **Ignored** |
| Compliance | `WithComplianceFramework()` | **Ignored** |

**What C1 reads:**
- Slug (for grant matching)
- DisplayName
- Description
- GrantableTo (for UI filtering)

---

## Grant: Options

| Option | SDK Function | C1 Status |
|--------|-------------|-----------|
| Principal | (required) | **Yes** |
| Entitlement | (required) | **Yes** |
| ID | (auto-generated) | **Yes** - for revocation |

**Note:** Grant ExternalId annotation is different from Resource ExternalId. Grant annotations are rarely needed.

---

## Annotations: Limited Use

Most annotations are for internal SDK use:

| Annotation | Purpose | C1 Use |
|------------|---------|--------|
| RateLimitDescription | Rate limit metadata | SDK internal |
| RequestId | Request tracking | SDK internal |
| NextPage | Pagination hint | SDK internal |
| Profile | Generic metadata | **Sometimes** |
| ExternalId | External reference | **Ignored** |

**Recommendation:** Don't add custom annotations expecting C1 to read them.

---

## Metadata Capabilities

The `baton_capabilities.json` file is auto-generated. Manual edits are overwritten.

Capabilities that are declared but have limited C1 support:
- Custom resource types beyond User/Group/Role/App
- Complex permission hierarchies
- Multi-level delegation

---

## Features That Look Useful But Aren't

### "Rich" User Profiles

```go
// Tempting but wasted effort
rs.WithUserProfile(map[string]interface{}{
    "department":  user.Department,
    "title":       user.Title,
    "manager":     user.Manager,
    "costCenter":  user.CostCenter,
    "location":    user.Location,
    "employeeId":  user.EmployeeId,
    "startDate":   user.StartDate,
    "badge_photo": user.PhotoURL,
    // ... 20 more fields
})
```

C1 displays these in the UI but doesn't use them for access decisions.

**Recommendation:** Include fields useful for human identification (name, department, title). Skip everything else.

### Structured Names

```go
// The SDK has this, but C1 ignores it
type StructuredName struct {
    GivenName  string
    FamilyName string
    Formatted  string
}
```

Just use the display name string.

### Icon/Photo URLs

C1 doesn't fetch or display custom icons. Default icons are used.

---

## What TO Focus On

These fields matter for C1 sync and provisioning:

### Users
- `Email` - Identity matching
- `Login` - Identity matching
- `Status` - Lifecycle management
- `ResourceId` - Stable, unique identifier

### Groups
- `DisplayName` - UI display
- `ResourceId` - Stable identifier
- Member entitlement - Grant/revoke

### Roles
- `DisplayName` - UI display
- `ResourceId` - Stable identifier
- Assignment entitlement - Grant/revoke

### Grants
- `Principal` - Who has access
- `Entitlement` - What access
- Correct entity sources (principal vs entitlement)

---

## How to Know If a Feature Is Used

1. **Check C1 UI** - If you can't see it in the app, it's not used
2. **Ask C1 team** - "Does C1 use X field for anything?"
3. **Check this document** - Updated based on analysis

When in doubt, implement the minimum and iterate based on actual C1 needs.

---

## Historical Note

Some unused features exist because:
- Planned for future roadmap items
- Added for specific customer requests that didn't ship
- Legacy compatibility with older SDK versions
- Aspirational "complete" API design

The SDK API is broader than what C1 currently consumes.
