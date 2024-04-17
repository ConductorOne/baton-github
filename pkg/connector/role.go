package connector

import (
	"context"

	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/annotations"
	"github.com/conductorone/baton-sdk/pkg/pagination"
	"github.com/conductorone/baton-sdk/pkg/types/resource"
	"github.com/google/go-github/v41/github"
)

const (
	OwnerRole          = "owner"
	BillingManagerRole = "billing_manager"
	MemberRole         = "member"
	GuestRole          = "guest"
)

var EnterpriseRoles = []string{OwnerRole, BillingManagerRole, MemberRole, GuestRole}

// Create a new connector resource for a github role.
func roleResource(role string) (*v2.Resource, error) {
	ret, err := resource.NewRoleResource(
		role,
		resourceTypeRole,
		role,
		nil,
		resource.WithAnnotation(
			&v2.V1Identifier{Id: role},
		),
	)
	if err != nil {
		return nil, err
	}

	return ret, nil
}

type roleResourceType struct {
	resourceType *v2.ResourceType
	client       *github.Client
}

func (r *roleResourceType) ResourceType(_ context.Context) *v2.ResourceType {
	return r.resourceType
}

func (r *roleResourceType) List(ctx context.Context, _ *v2.ResourceId, _ *pagination.Token) ([]*v2.Resource, string, annotations.Annotations, error) {
	var rv []*v2.Resource

	for _, role := range EnterpriseRoles {
		res, err := roleResource(role)
		if err != nil {
			return nil, "", nil, err
		}

		rv = append(rv, res)
	}

	return rv, "", nil, nil
}

func (r *roleResourceType) Entitlements(_ context.Context, _ *v2.Resource, _ *pagination.Token) ([]*v2.Entitlement, string, annotations.Annotations, error) {
	return nil, "", nil, nil
}

func (r *roleResourceType) Grants(_ context.Context, _ *v2.Resource, _ *pagination.Token) ([]*v2.Grant, string, annotations.Annotations, error) {
	var rv []*v2.Grant

	return rv, "", nil, nil
}

func roleBuilder(client *github.Client) *roleResourceType {
	return &roleResourceType{
		resourceType: resourceTypeRole,
		client:       client,
	}
}
