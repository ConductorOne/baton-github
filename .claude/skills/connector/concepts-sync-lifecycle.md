# concepts-sync-lifecycle

How connectors sync data to ConductorOne.

---

## SDK Orchestration

The SDK uses inversion of control. Connectors implement interfaces; SDK orchestrates execution.

```
SDK calls connector methods in phases:
1. ResourceType() - once per type, learn metadata
2. List() - paginated, fetch all resources
3. Entitlements() - once per resource, fetch permissions
4. Grants() - once per resource, fetch assignments
```

The connector never controls flow. SDK batches operations, builds access graphs, handles checkpointing.

---

## Four Sync Phases

### Phase 1: Resource Types

```go
func (u *userBuilder) ResourceType(ctx context.Context) *v2.ResourceType {
    return &v2.ResourceType{
        Id:          "user",
        DisplayName: "User",
        Traits:      []v2.ResourceType_Trait{v2.ResourceType_TRAIT_USER},
    }
}
```

Called once per resource type. Returns metadata including traits.

### Phase 2: List Resources

```go
func (u *userBuilder) List(ctx context.Context, parentID *v2.ResourceId,
    token *pagination.Token) ([]*v2.Resource, string, annotations.Annotations, error)
```

Called repeatedly with pagination tokens until empty token returned. Must handle:
- Pagination via token parameter
- Parent resources (for hierarchical data)
- Annotations (rate limits, metadata)

### Phase 3: Entitlements

```go
func (g *groupBuilder) Entitlements(ctx context.Context, resource *v2.Resource,
    token *pagination.Token) ([]*v2.Entitlement, string, annotations.Annotations, error)
```

Called once per resource discovered in Phase 2. Returns what permissions exist on this resource.

Example: A group has "member" entitlement that can be granted to users.

### Phase 4: Grants

```go
func (g *groupBuilder) Grants(ctx context.Context, resource *v2.Resource,
    token *pagination.Token) ([]*v2.Grant, string, annotations.Annotations, error)
```

Called once per resource. Returns who has which entitlements.

Example: User "alice" has "member" entitlement on group "admins".

---

## Checkpointing

SDK checkpoints every 10 seconds during sync. If interrupted:
- Sync resumes from last checkpoint
- Connector receives pagination token from checkpoint
- No need to restart from zero

This is why pagination must be stateless - all state is in the token.

---

## Stateless Requirement

Connectors must be stateless:
- No global variables
- No instance state between calls
- All context in method parameters
- Pagination tokens are opaque (SDK manages)

**Rationale:** Connectors may run in Lambda, may be interrupted, may resume on different instance.

---

## Data Flow Summary

```
External API -> Connector.List() -> Resources
                                      |
                                      v
              Connector.Entitlements() -> Entitlements
                                            |
                                            v
                  Connector.Grants() -> Grants
                                          |
                                          v
                            SDK builds access graph
                                          |
                                          v
                              c1z file (SQLite + gzip)
                                          |
                                          v
                              ConductorOne platform
```
