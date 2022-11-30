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
)

// Create a new connector resource for a github user.
func userResource(ctx context.Context, user *github.User) (*v2.Resource, error) {
	displayName := user.GetName()
	if displayName == "" {
		// users do not always specify a name and we only get public email from
		// this endpoint.
		displayName = user.GetLogin()
	}

	var annos annotations.Annotations
	annos.Append(&v2.ExternalLink{
		Url: user.GetHTMLURL(),
	})
	annos.Append(&v2.V1Identifier{
		Id: strconv.FormatInt(user.GetID(), 10),
	})

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

	ret, err := sdk.NewUserResource(displayName, resourceTypeUser, nil, user.GetID(), user.GetEmail(), profile)
	if err != nil {
		return nil, err
	}
	ret.Annotations = append(ret.Annotations, annos...)

	return ret, nil
}

type userResourceType struct {
	resourceType *v2.ResourceType
	client       *github.Client
}

func (o *userResourceType) ResourceType(_ context.Context) *v2.ResourceType {
	return o.resourceType
}

func (o *userResourceType) List(ctx context.Context, parentID *v2.ResourceId, pt *pagination.Token) ([]*v2.Resource, string, annotations.Annotations, error) {
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

	opts := github.ListMembersOptions{
		ListOptions: github.ListOptions{Page: page, PerPage: pt.Size},
	}

	users, resp, err := o.client.Organizations.ListMembers(ctx, orgName, &opts)
	if err != nil {
		return nil, "", nil, fmt.Errorf("github-connector: ListMembers failed: %w", err)
	}

	nextPage, reqAnnos, err := parseResp(resp)
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
		ur, err := userResource(ctx, u)
		if err != nil {
			return nil, "", nil, err
		}

		rv = append(rv, ur)
	}

	return rv, pageToken, reqAnnos, nil
}

func (o *userResourceType) Entitlements(_ context.Context, _ *v2.Resource, _ *pagination.Token) ([]*v2.Entitlement, string, annotations.Annotations, error) {
	return nil, "", nil, nil
}

func (o *userResourceType) Grants(_ context.Context, _ *v2.Resource, _ *pagination.Token) ([]*v2.Grant, string, annotations.Annotations, error) {
	return nil, "", nil, nil
}

func userBuilder(client *github.Client) *userResourceType {
	return &userResourceType{
		resourceType: resourceTypeUser,
		client:       client,
	}
}
