package connector

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	v2 "github.com/ductone/connector-sdk/pb/c1/connector/v2"
	"github.com/ductone/connector-sdk/pkg/annotations"
	"github.com/ductone/connector-sdk/pkg/pagination"
	"github.com/google/go-github/v41/github"
	"google.golang.org/protobuf/types/known/structpb"
)

// Create a new connector resource for a github user.
func userResource(ctx context.Context, user *github.User) (*v2.Resource, error) {
	displayName := user.GetName()
	if displayName == "" {
		// users do not always specify a name and we only get public email from
		// this endpoint.
		displayName = user.GetLogin()
	}

	ut, err := userTrait(ctx, user)
	if err != nil {
		return nil, err
	}
	var annos annotations.Annotations
	annos.Append(ut)
	annos.Append(&v2.ExternalLink{
		Url: user.GetHTMLURL(),
	})
	annos.Append(&v2.V1Identifier{
		Id: strconv.FormatInt(user.GetID(), 10),
	})

	return &v2.Resource{
		Id: &v2.ResourceId{
			ResourceType: resourceTypeUser.Id,
			Resource:     fmt.Sprintf("%d", user.GetID()),
		},
		DisplayName: displayName,
		Annotations: annos,
	}, nil
}

// Create and return a User trait for a github user.
func userTrait(ctx context.Context, user *github.User) (*v2.UserTrait, error) {
	ret := &v2.UserTrait{
		Status: &v2.UserTrait_Status{
			Status: v2.UserTrait_Status_STATUS_ENABLED,
		},
	}

	if user.GetAvatarURL() != "" {
		ret.Icon = &v2.AssetRef{Id: user.GetAvatarURL()}
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

	profile, err := structpb.NewStruct(map[string]interface{}{
		"first_name": firstName,
		"last_name":  lastName,
		"login":      user.GetLogin(),
		"user_id":    strconv.Itoa(int(user.GetID())),
	})
	if err != nil {
		return nil, fmt.Errorf("github-connectorv2: failed to construct user profile for user trait: %w", err)
	}
	ret.Profile = profile

	if user.GetEmail() != "" {
		ret.Emails = []*v2.UserTrait_Email{
			{
				Address:   user.GetEmail(),
				IsPrimary: true,
			},
		}
	}

	// TODO(jirwin): We can maybe fetch the gravatar ID here. What is the asset server story?
	if user.GetGravatarID() != "" {
		ret.Icon = &v2.AssetRef{Id: fmt.Sprintf("user:gravatar:%s", user.GetGravatarID())}
	}

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

	orgName := getOrgName(parentID)

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
