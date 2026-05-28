package connector

import (
	"context"
	"fmt"

	"github.com/conductorone/baton-github/pkg/customclient"
	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/types/entitlement"
	"github.com/conductorone/baton-sdk/pkg/types/grant"
	resourceSdk "github.com/conductorone/baton-sdk/pkg/types/resource"
)

const (
	licenseEntitlementAssigned = "assigned"

	// enterpriseRoleMember is the GitHub enterprise role name that maps to
	// "this user counts as a member of the enterprise." It's the value GitHub
	// returns in github_com_enterprise_roles for license-consuming members.
	enterpriseRoleMember = "Member"
)

type licenseResourceType struct {
	resourceType *v2.ResourceType
	customClient *customclient.Client
	enterprises  []string
}

func (l *licenseResourceType) ResourceType(_ context.Context) *v2.ResourceType {
	return l.resourceType
}

func (l *licenseResourceType) List(
	ctx context.Context,
	parentID *v2.ResourceId,
	opts resourceSdk.SyncOpAttrs,
) ([]*v2.Resource, *resourceSdk.SyncOpResults, error) {
	var ret []*v2.Resource
	for _, enterprise := range l.enterprises {
		// total_seats_purchased and total_seats_consumed are enterprise-wide
		// aggregates that the API returns identically on every page, so the
		// first page is enough.
		consumedLicenses, _, err := l.customClient.ListEnterpriseConsumedLicenses(ctx, enterprise, 1)
		if err != nil {
			return nil, nil, fmt.Errorf("baton-github: error listing enterprise consumed licenses for %s: %w", enterprise, err)
		}

		memberRoleResource, err := resourceSdk.NewResource(
			enterpriseRoleMember,
			resourceTypeEnterpriseRole,
			fmt.Sprintf("%s:%s", enterprise, enterpriseRoleMember),
		)
		if err != nil {
			return nil, nil, err
		}

		resource, err := licenseResource(
			enterprise,
			int64(consumedLicenses.TotalSeatsPurchased),
			int64(consumedLicenses.TotalSeatsConsumed),
			entitlement.NewEntitlementID(memberRoleResource, licenseEntitlementAssigned),
		)
		if err != nil {
			return nil, nil, fmt.Errorf("baton-github: error creating license resource for enterprise %s: %w", enterprise, err)
		}
		ret = append(ret, resource)
	}

	return ret, &resourceSdk.SyncOpResults{}, nil
}

func (l *licenseResourceType) Entitlements(
	_ context.Context,
	_ *v2.Resource,
	_ resourceSdk.SyncOpAttrs,
) ([]*v2.Entitlement, *resourceSdk.SyncOpResults, error) {
	return nil, &resourceSdk.SyncOpResults{}, nil
}

func (l *licenseResourceType) StaticEntitlements(
	_ context.Context,
	_ resourceSdk.SyncOpAttrs,
) ([]*v2.Entitlement, *resourceSdk.SyncOpResults, error) {
	rv := []*v2.Entitlement{
		entitlement.NewAssignmentEntitlement(nil, licenseEntitlementAssigned,
			entitlement.WithDisplayName("License Assigned"),
			entitlement.WithDescription("Assignment of an enterprise license seat"),
			entitlement.WithGrantableTo(resourceTypeUser),
		),
	}
	return rv, &resourceSdk.SyncOpResults{}, nil
}

func (l *licenseResourceType) Grants(
	_ context.Context,
	resource *v2.Resource,
	_ resourceSdk.SyncOpAttrs,
) ([]*v2.Grant, *resourceSdk.SyncOpResults, error) {
	enterprise := resource.Id.Resource

	memberRoleResource, err := resourceSdk.NewResource(
		enterpriseRoleMember,
		resourceTypeEnterpriseRole,
		fmt.Sprintf("%s:%s", enterprise, enterpriseRoleMember),
	)
	if err != nil {
		return nil, nil, err
	}

	grants := []*v2.Grant{
		grant.NewGrant(resource, licenseEntitlementAssigned, memberRoleResource.Id,
			grant.WithAnnotation(
				&v2.GrantExpandable{
					EntitlementIds: []string{
						entitlement.NewEntitlementID(memberRoleResource, licenseEntitlementAssigned),
					},
					Shallow:         true,
					ResourceTypeIds: []string{resourceTypeUser.Id},
				},
				&v2.V1Identifier{
					Id: fmt.Sprintf("license-grant:%s", enterprise),
				},
			),
		),
	}

	return grants, &resourceSdk.SyncOpResults{}, nil
}

func licenseResource(enterprise string, purchasedSeats, consumedSeats int64, entitlementID string) (*v2.Resource, error) {
	traitOpts := []resourceSdk.LicenseProfileTraitOption{
		resourceSdk.WithLicenseName(enterprise),
		resourceSdk.WithLicenseSeats(purchasedSeats, consumedSeats),
		resourceSdk.WithLicenseEntitlementIDs(entitlementID),
	}

	licenseTrait, err := resourceSdk.NewLicenseProfileTrait(traitOpts...)
	if err != nil {
		return nil, err
	}

	return resourceSdk.NewResource(
		enterprise,
		resourceTypeLicense,
		enterprise,
		resourceSdk.WithAnnotation(
			licenseTrait,
			&v2.V1Identifier{Id: fmt.Sprintf("license:%s", enterprise)},
		),
	)
}

func LicenseBuilder(
	customClient *customclient.Client,
	enterprises []string,
) *licenseResourceType {
	return &licenseResourceType{
		resourceType: resourceTypeLicense,
		customClient: customClient,
		enterprises:  enterprises,
	}
}
