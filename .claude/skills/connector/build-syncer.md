# build-syncer

Implementing the ResourceSyncer interface.

---

## Interface Definition

```go
type ResourceSyncer interface {
    ResourceType(ctx context.Context) *v2.ResourceType
    List(ctx context.Context, parentResourceID *v2.ResourceId, token *pagination.Token) (
        []*v2.Resource, string, annotations.Annotations, error)
    Entitlements(ctx context.Context, resource *v2.Resource, token *pagination.Token) (
        []*v2.Entitlement, string, annotations.Annotations, error)
    Grants(ctx context.Context, resource *v2.Resource, token *pagination.Token) (
        []*v2.Grant, string, annotations.Annotations, error)
}
```

---

## Resource Type Definition

Define at package level for stability:

```go
// pkg/connector/resource_types.go
var (
    userResourceType = &v2.ResourceType{
        Id:          "user",
        DisplayName: "User",
        Traits:      []v2.ResourceType_Trait{v2.ResourceType_TRAIT_USER},
    }

    groupResourceType = &v2.ResourceType{
        Id:          "group",
        DisplayName: "Group",
        Traits:      []v2.ResourceType_Trait{v2.ResourceType_TRAIT_GROUP},
    }
)
```

**Critical:** IDs must be stable across versions. Changing `Id` breaks grant history.

---

## User Builder Example

```go
type userBuilder struct {
    client *client.Client
}

func (u *userBuilder) ResourceType(ctx context.Context) *v2.ResourceType {
    return userResourceType
}

func (u *userBuilder) List(ctx context.Context, parentID *v2.ResourceId,
    token *pagination.Token) ([]*v2.Resource, string, annotations.Annotations, error) {

    bag := &pagination.Bag{}
    if err := bag.Unmarshal(token.Token); err != nil {
        return nil, "", nil, err
    }

    users, nextToken, err := u.client.ListUsers(ctx, bag.PageToken(), 100)
    if err != nil {
        return nil, "", nil, fmt.Errorf("baton-myservice: failed to list users: %w", err)
    }

    var resources []*v2.Resource
    for _, user := range users {
        resource, err := rs.NewUserResource(
            user.Name,
            userResourceType,
            user.ID,  // Stable, immutable ID
            []rs.UserTraitOption{
                rs.WithEmail(user.Email, true),
                rs.WithStatus(mapStatus(user.Active)),
                rs.WithUserLogin(user.Username),
            },
        )
        if err != nil {
            return nil, "", nil, err
        }
        resources = append(resources, resource)
    }

    nextPage, err := bag.NextToken(nextToken)
    if err != nil {
        return nil, "", nil, err
    }

    return resources, nextPage, nil, nil
}

func (u *userBuilder) Entitlements(ctx context.Context, resource *v2.Resource,
    token *pagination.Token) ([]*v2.Entitlement, string, annotations.Annotations, error) {
    // Users don't offer entitlements - they receive grants
    return nil, "", nil, nil
}

func (u *userBuilder) Grants(ctx context.Context, resource *v2.Resource,
    token *pagination.Token) ([]*v2.Grant, string, annotations.Annotations, error) {
    // Users don't have grants on them - they receive grants elsewhere
    return nil, "", nil, nil
}
```

---

## Group Builder Example

Groups offer entitlements and have grants:

```go
func (g *groupBuilder) Entitlements(ctx context.Context, resource *v2.Resource,
    token *pagination.Token) ([]*v2.Entitlement, string, annotations.Annotations, error) {

    entitlement := sdkEntitlement.NewAssignmentEntitlement(
        resource,
        "member",
        sdkEntitlement.WithDisplayName("Member"),
        sdkEntitlement.WithDescription("Member of "+resource.DisplayName),
        sdkEntitlement.WithGrantableTo(userResourceType),
    )

    return []*v2.Entitlement{entitlement}, "", nil, nil
}

func (g *groupBuilder) Grants(ctx context.Context, resource *v2.Resource,
    token *pagination.Token) ([]*v2.Grant, string, annotations.Annotations, error) {

    groupID := resource.Id.Resource

    bag := &pagination.Bag{}
    if err := bag.Unmarshal(token.Token); err != nil {
        return nil, "", nil, err
    }

    members, nextToken, err := g.client.GetGroupMembers(ctx, groupID, bag.PageToken())
    if err != nil {
        return nil, "", nil, fmt.Errorf("baton-myservice: failed to get group members: %w", err)
    }

    var grants []*v2.Grant
    for _, member := range members {
        grant := sdkGrant.NewGrant(
            resource,
            "member",
            &v2.ResourceId{
                ResourceType: "user",
                Resource:     member.UserID,
            },
        )
        grants = append(grants, grant)
    }

    nextPage, err := bag.NextToken(nextToken)
    if err != nil {
        return nil, "", nil, err
    }

    return grants, nextPage, nil, nil
}
```

---

## Registering Syncers

```go
// pkg/connector/connector.go
func (c *Connector) ResourceSyncers(ctx context.Context) []connectorbuilder.ResourceSyncer {
    return []connectorbuilder.ResourceSyncer{
        newUserBuilder(c.client),
        newGroupBuilder(c.client),
        newRoleBuilder(c.client),
    }
}
```

---

## Key Points

- `ResourceType()` called once per sync to learn metadata
- `List()` may be called multiple times for pagination
- `Entitlements()` called once per resource instance
- `Grants()` called once per resource instance, may paginate
- Return empty string for nextToken when done paginating
- Users typically have empty Entitlements() and Grants()
- Groups/Roles have entitlements and grants
