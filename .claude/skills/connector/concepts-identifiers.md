# concepts-identifiers

How identifiers work in connectors. Four "ExternalId" concepts exist - only one matters.

---

## The Four ExternalId Concepts

| Name | Type | Used by C1? |
|------|------|-------------|
| `connector_v2.Resource.ExternalId` | proto field | **YES - provisioning** |
| `ConnectorResource.ExternalId` | v1 model string | YES - v1 sync only |
| `ConnectorV2Resource.ExternalID()` | method | YES - returns ResourceId |
| `SourceConnectorIds[conn_id]` | map value | YES - sync + provisioning |

**Key insight:** The SDK's `WithExternalID()` function sets the native system identifier that provisioning operations need to call the target API. See "WithExternalID - REQUIRED for Provisioning" section below.

---

## What Actually Matters: ResourceId

When creating resources, the `objectID` parameter becomes the matching key:

```go
// This is what matters for sync and provisioning
resource, err := rs.NewUserResource(
    user.DisplayName,    // displayName
    userResourceType,    // type -> "user"
    user.ID,             // objectID -> becomes ResourceId.Resource
    traitOptions,
)
```

The SDK serializes this as `"type::id"` (e.g., `"user::12345"`) and stores it in `SourceConnectorIds`.

---

## Sync Flow

```
Connector: NewUserResource(name, type, "12345")
    |
    v
Resource.Id = ResourceId{ResourceType: "user", Resource: "12345"}
    |
    v
SDK: ResourceIDToString(Resource.Id) = "user::12345"
    |
    v
C1 Database: AppResource.SourceConnectorIds["conn_id"] = "user::12345"
```

---

## Provisioning Flow

```
C1 Database: SourceConnectorIds["conn_id"] = "user::12345"
    |
    v
SDK: ParseV2ExternalID("user::12345")
    |
    v
ResourceId{ResourceType: "user", Resource: "12345"}
    |
    v
Connector.Grant() receives this ResourceId
```

---

## ID Stability Requirements

**IDs must be stable across syncs.** If you change how IDs are calculated:
- C1 sees old resources as deleted
- C1 sees new resources as created
- Grant history is lost
- Access reviews break

```go
// WRONG: ID changes based on data availability
id := user.Id
if id == "" {
    id = user.Email  // Different format!
}

// WRONG: Using mutable values
rs.NewUserResource(name, type, user.Email, ...)  // Email can change

// CORRECT: Use immutable system ID
rs.NewUserResource(name, type, user.Id, ...)
```

---

## WithExternalID - REQUIRED for Provisioning

```go
// ExternalId stores the native system identifier for provisioning
rs.WithExternalID(&v2.ExternalId{
    Id:   user.NativeAPIId,  // The ID the target API expects
    Link: fmt.Sprintf("https://admin.example.com/users/%s", user.NativeAPIId),
})
```

**Why it matters:** ConductorOne assigns its own resource IDs (`match_baton_id`) that differ from the target system's native IDs. During provisioning, the SDK passes `ExternalId` to Grant/Revoke operations so the connector can call the target API.

**During sync:** Set `WithExternalID()` with the native identifier
**During provisioning:** Retrieve via `principal.GetExternalId().Id`

```go
// In Grant operation
externalId := principal.GetExternalId()
if externalId == nil {
    return nil, nil, fmt.Errorf("baton-myservice: principal missing external ID")
}
nativeUserID := externalId.Id  // Use this to call target API
```

---

## match_baton_id (Advanced)

For pre-created manual resources that need to merge with connector-discovered resources:

```go
// Connector sets RawId annotation
rs.WithAnnotation(&v2.RawId{Id: user.Email})
```

C1 matches this against `AppResource.MatchBatonId` for manually-managed resources.

Most connectors don't need this. Only use when:
- External resources are provisioned outside C1
- HR systems create accounts before connectors discover them
- Pre-staging resources before first sync
