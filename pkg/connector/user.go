package connector

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/annotations"
	"github.com/conductorone/baton-sdk/pkg/pagination"
	"github.com/conductorone/baton-sdk/pkg/sdk"
	"github.com/google/go-github/v41/github"
	"github.com/shurcooL/githubv4"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Create a new connector resource for a github user.
func userResource(ctx context.Context, user *github.User, userEmail string) (*v2.Resource, error) {
	displayName := user.GetName()
	if displayName == "" {
		// users do not always specify a name and we only get public email from
		// this endpoint.
		displayName = user.GetLogin()
	}

	names := strings.SplitN(user.GetName(), " ", 2)
	var firstName, lastName string
	switch len(names) {
	case 1:
		firstName = names[0]
	case 2:
		firstName = names[0]
		lastName = names[1]
	}

	profile := map[string]interface{}{
		"first_name": firstName,
		"last_name":  lastName,
		"login":      user.GetLogin(),
		"user_id":    strconv.Itoa(int(user.GetID())),
	}

	ret, err := sdk.NewUserResource(
		displayName,
		resourceTypeUser,
		nil,
		user.GetID(),
		userEmail,
		profile,
		&v2.ExternalLink{Url: user.GetHTMLURL()},
		&v2.V1Identifier{Id: strconv.FormatInt(user.GetID(), 10)},
	)
	if err != nil {
		return nil, err
	}

	return ret, nil
}

type userResourceType struct {
	resourceType   *v2.ResourceType
	client         *github.Client
	graphqlClient  *githubv4.Client
	hasSAMLEnabled *bool
}

func (o *userResourceType) ResourceType(_ context.Context) *v2.ResourceType {
	return o.resourceType
}

func (o *userResourceType) List(ctx context.Context, parentID *v2.ResourceId, pt *pagination.Token) ([]*v2.Resource, string, annotations.Annotations, error) {
	var annotations annotations.Annotations
	if parentID == nil {
		return nil, "", nil, nil
	}

	bag, page, err := parsePageToken(pt.Token, &v2.ResourceId{ResourceType: resourceTypeUser.Id})
	if err != nil {
		return nil, "", nil, err
	}

	orgName, err := getOrgName(ctx, o.client, parentID)
	if err != nil {
		return nil, "", nil, err
	}

	hasSamlBool, err := o.hasSAML(ctx, orgName)
	if err != nil {
		return nil, "", nil, err
	}
	q := listUsersQuery{}
	var restApiRateLimit *v2.RateLimitDescription

	opts := github.ListMembersOptions{
		ListOptions: github.ListOptions{Page: page, PerPage: pt.Size},
	}

	users, resp, err := o.client.Organizations.ListMembers(ctx, orgName, &opts)
	if err != nil {
		return nil, "", nil, fmt.Errorf("github-connector: ListMembers failed: %w", err)
	}

	restApiRateLimit, err = extractRateLimitData(resp)
	if err != nil {
		return nil, "", nil, err
	}

	nextPage, _, err := parseResp(resp)
	if err != nil {
		return nil, "", nil, err
	}

	pageToken, err := bag.NextToken(nextPage)
	if err != nil {
		return nil, "", nil, err
	}

	rv := make([]*v2.Resource, 0, len(users))
	for _, user := range users {
		u, _, err := o.client.Users.GetByID(ctx, user.GetID())
		if err != nil {
			return nil, "", nil, err
		}
		userEmail := u.GetEmail()
		if hasSamlBool {
			q = listUsersQuery{}
			variables := map[string]interface{}{
				"orgLoginName": githubv4.String(orgName),
				"userName":     githubv4.String(u.GetLogin()),
			}
			err = o.graphqlClient.Query(ctx, &q, variables)
			if err != nil {
				return nil, "", nil, err
			}
			if len(q.Organization.SamlIdentityProvider.ExternalIdentities.Edges) == 1 {
				userEmail = q.Organization.SamlIdentityProvider.ExternalIdentities.Edges[0].Node.SamlIdentity.NameId
			}
		}
		ur, err := userResource(ctx, u, userEmail)
		if err != nil {
			return nil, "", nil, err
		}

		rv = append(rv, ur)
	}
	annotations.WithRateLimiting(restApiRateLimit)
	if *o.hasSAMLEnabled && int64(q.RateLimit.Remaining) < restApiRateLimit.Remaining {
		graphqlRateLimit := &v2.RateLimitDescription{
			Limit:     int64(q.RateLimit.Limit),
			Remaining: int64(q.RateLimit.Remaining),
			ResetAt:   timestamppb.New(q.RateLimit.ResetAt.Time),
		}
		annotations.WithRateLimiting(graphqlRateLimit)
	}

	return rv, pageToken, annotations, nil
}

func (o *userResourceType) Entitlements(_ context.Context, _ *v2.Resource, _ *pagination.Token) ([]*v2.Entitlement, string, annotations.Annotations, error) {
	return nil, "", nil, nil
}

func (o *userResourceType) Grants(_ context.Context, _ *v2.Resource, _ *pagination.Token) ([]*v2.Grant, string, annotations.Annotations, error) {
	return nil, "", nil, nil
}

func userBuilder(client *github.Client, hasSAMLEnabled *bool, graphqlClient *githubv4.Client) *userResourceType {
	return &userResourceType{
		resourceType:   resourceTypeUser,
		client:         client,
		graphqlClient:  graphqlClient,
		hasSAMLEnabled: hasSAMLEnabled,
	}
}

func (o *userResourceType) hasSAML(ctx context.Context, orgName string) (bool, error) {
	if o.hasSAMLEnabled != nil {
		return *o.hasSAMLEnabled, nil
	}

	samlBool := false
	q := hasSAMLQuery{}
	variables := map[string]interface{}{
		"orgLoginName": githubv4.String(orgName),
	}
	err := o.graphqlClient.Query(ctx, &q, variables)
	if err != nil {
		return false, err
	}
	if q.Organization.SamlIdentityProvider.Id != "" {
		samlBool = true
	}
	o.hasSAMLEnabled = &samlBool
	return *o.hasSAMLEnabled, nil
}
