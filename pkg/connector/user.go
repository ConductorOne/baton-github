package connector

import (
	"context"
	"fmt"
	"net/mail"
	"strconv"
	"strings"

	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/annotations"
	"github.com/conductorone/baton-sdk/pkg/pagination"
	"github.com/conductorone/baton-sdk/pkg/types/resource"
	"github.com/google/go-github/v69/github"
	"github.com/grpc-ecosystem/go-grpc-middleware/logging/zap/ctxzap"
	"github.com/shurcooL/githubv4"
	"go.uber.org/zap"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const outsideCollaboratorPhase = "outside_collaborator"

// Create a new connector resource for a GitHub user.
func userResource(ctx context.Context, user *github.User, userEmail string, extraEmails []string, isOutsideCollaborator bool) (*v2.Resource, error) {
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
		"first_name":              firstName,
		"last_name":               lastName,
		"login":                   user.GetLogin(),
		"user_id":                 strconv.Itoa(int(user.GetID())),
		"is_outside_collaborator": isOutsideCollaborator,
	}

	userTrait := []resource.UserTraitOption{
		resource.WithEmail(userEmail, true),
		resource.WithUserProfile(profile),
		resource.WithStatus(v2.UserTrait_Status_STATUS_ENABLED),
	}

	for _, email := range extraEmails {
		userTrait = append(userTrait, resource.WithEmail(email, false))
	}

	if user.GetAvatarURL() != "" {
		userTrait = append(userTrait, resource.WithUserIcon(&v2.AssetRef{
			Id: user.GetAvatarURL(),
		}))
	}
	if user.GetLogin() != "" {
		userTrait = append(userTrait, resource.WithUserLogin(user.GetLogin()))
	}
	if user.TwoFactorAuthentication != nil {
		userTrait = append(userTrait, resource.WithMFAStatus(&v2.UserTrait_MFAStatus{
			MfaEnabled: user.GetTwoFactorAuthentication(),
		}))
	}

	ret, err := resource.NewUserResource(
		displayName,
		resourceTypeUser,
		user.GetID(),
		userTrait,
		resource.WithAnnotation(
			&v2.ExternalLink{Url: user.GetHTMLURL()},
			&v2.V1Identifier{Id: strconv.FormatInt(user.GetID(), 10)},
		),
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
	orgCache       *orgNameCache
	orgs           []string
}

func (o *userResourceType) ResourceType(_ context.Context) *v2.ResourceType {
	return o.resourceType
}

// fetchUserDetails fetches full user details by ID, falling back to the summary
// user on 404 (this undocumented API can return 404 for some users).
func (o *userResourceType) fetchUserDetails(ctx context.Context, user *github.User) (*github.User, error) {
	l := ctxzap.Extract(ctx)
	u, res, err := o.client.Users.GetByID(ctx, user.GetID())
	if err != nil {
		if isNotFoundError(res) {
			l.Warn("error fetching user by id", zap.Error(err), zap.Int64("user_id", user.GetID()))
			return user, nil
		}
		return nil, wrapGitHubError(err, res, "github-connector: failed to get user by id")
	}
	return u, nil
}

// enrichUserWithSAML queries the GraphQL API for the user's SAML identity.
// Returns the primary email, any extra emails, and the GraphQL rate limit description.
// If the org uses Enterprise-level SAML instead of org-level, it disables future SAML
// queries and returns empty strings with no error.
func (o *userResourceType) enrichUserWithSAML(ctx context.Context, orgName string, userLogin string) (string, []string, *v2.RateLimitDescription, error) {
	l := ctxzap.Extract(ctx)
	q := listUsersQuery{}
	variables := map[string]interface{}{
		"orgLoginName": githubv4.String(orgName),
		"userName":     githubv4.String(userLogin),
	}

	err := o.graphqlClient.Query(ctx, &q, variables)
	if err != nil {
		if strings.Contains(err.Error(), "SAML identity provider is disabled when an Enterprise SAML identity provider is available") {
			l.Debug("org SAML disabled in favor of Enterprise SAML, skipping SAML identity enrichment",
				zap.String("org", orgName),
				zap.String("user", userLogin))
			samlDisabled := false
			o.hasSAMLEnabled = &samlDisabled
			return "", nil, nil, nil
		}
		return "", nil, nil, err
	}

	rlDesc := &v2.RateLimitDescription{
		Limit:     int64(q.RateLimit.Limit),
		Remaining: int64(q.RateLimit.Remaining),
		ResetAt:   timestamppb.New(q.RateLimit.ResetAt.Time),
	}

	if len(q.Organization.SamlIdentityProvider.ExternalIdentities.Edges) != 1 {
		return "", nil, rlDesc, nil
	}

	samlIdent := q.Organization.SamlIdentityProvider.ExternalIdentities.Edges[0].Node.SamlIdentity
	primaryEmail := samlIdent.NameId
	setPrimary := primaryEmail != ""

	var extraEmails []string
	for _, email := range samlIdent.Emails {
		if !isEmail(email.Value) {
			continue
		}
		if !setPrimary {
			primaryEmail = email.Value
			setPrimary = true
		} else {
			extraEmails = append(extraEmails, email.Value)
		}
	}

	return primaryEmail, extraEmails, rlDesc, nil
}

// buildUserResource fetches full user details, optionally enriches with SAML identity,
// and returns a connector resource along with the GraphQL rate limit (if SAML was queried).
func (o *userResourceType) buildUserResource(ctx context.Context, orgName string, hasSaml bool, user *github.User) (*v2.Resource, *v2.RateLimitDescription, error) {
	u, err := o.fetchUserDetails(ctx, user)
	if err != nil {
		return nil, nil, err
	}

	userEmail := u.GetEmail()
	var extraEmails []string
	var graphqlRateLimit *v2.RateLimitDescription

	if hasSaml {
		var samlEmail string
		samlEmail, extraEmails, graphqlRateLimit, err = o.enrichUserWithSAML(ctx, orgName, u.GetLogin())
		if err != nil {
			return nil, nil, err
		}
		if samlEmail != "" {
			userEmail = samlEmail
		}
	}

	ur, err := userResource(ctx, u, userEmail, extraEmails, false)
	if err != nil {
		return nil, nil, err
	}
	return ur, graphqlRateLimit, nil
}

// listOutsideCollaboratorsPage fetches one page of outside collaborators and returns
// them as user resources with proper SDK-driven pagination.
func (o *userResourceType) listOutsideCollaboratorsPage(ctx context.Context, orgName string, page int, bag *pagination.Bag) ([]*v2.Resource, *resource.SyncOpResults, error) {
	opts := &github.ListOutsideCollaboratorsOptions{
		ListOptions: github.ListOptions{
			Page:    page,
			PerPage: maxPageSize,
		},
	}
	users, resp, err := o.client.Organizations.ListOutsideCollaborators(ctx, orgName, opts)
	if err != nil {
		return nil, nil, wrapGitHubError(err, resp, "github-connector: failed to list outside collaborators")
	}

	rlDesc, err := extractRateLimitData(resp)
	if err != nil {
		return nil, nil, err
	}

	nextPage, _, err := parseResp(resp)
	if err != nil {
		return nil, nil, err
	}

	pageToken, err := bag.NextToken(nextPage)
	if err != nil {
		return nil, nil, err
	}

	rv := make([]*v2.Resource, 0, len(users))
	for _, user := range users {
		u, err := o.fetchUserDetails(ctx, user)
		if err != nil {
			return nil, nil, err
		}
		// Outside collaborators are external to the org, so org-level SAML
		// identities do not apply to them. Use the REST API email as-is.
		isOutsideCollaborator := true
		ur, err := userResource(ctx, u, u.GetEmail(), nil, isOutsideCollaborator)
		if err != nil {
			return nil, nil, err
		}
		rv = append(rv, ur)
	}

	var annos annotations.Annotations
	annos.WithRateLimiting(rlDesc)

	return rv, &resource.SyncOpResults{
		NextPageToken: pageToken,
		Annotations:   annos,
	}, nil
}

func (o *userResourceType) List(ctx context.Context, parentID *v2.ResourceId, opts resource.SyncOpAttrs) ([]*v2.Resource, *resource.SyncOpResults, error) {
	if parentID == nil {
		return nil, &resource.SyncOpResults{}, nil
	}

	bag, page, err := parsePageToken(opts.PageToken.Token, &v2.ResourceId{ResourceType: resourceTypeUser.Id})
	if err != nil {
		return nil, nil, err
	}

	orgName, err := o.orgCache.GetOrgName(ctx, opts.Session, parentID)
	if err != nil {
		return nil, nil, err
	}

	// List runs in two phases:
	//   Phase 1 – org members           (ResourceTypeID == resourceTypeUser.Id)
	//   Phase 2 – outside collaborators (ResourceTypeID == outsideCollaboratorPhase)
	if isOutsideCollaboratorPhase(bag) {
		return o.listOutsideCollaboratorsPage(ctx, orgName, page, bag)
	}

	users, resp, err := o.client.Organizations.ListMembers(ctx, orgName, &github.ListMembersOptions{
		ListOptions: github.ListOptions{
			Page:    page,
			PerPage: maxPageSize,
		},
	})
	if err != nil {
		return nil, nil, wrapGitHubError(err, resp, "github-connector: failed to list organization members")
	}

	restApiRateLimit, err := extractRateLimitData(resp)
	if err != nil {
		return nil, nil, err
	}

	nextPage, _, err := parseResp(resp)
	if err != nil {
		return nil, nil, err
	}

	hasSamlBool, err := o.hasSAML(ctx, orgName)
	if err != nil {
		return nil, nil, err
	}

	rv, graphqlRateLimit, err := o.buildUserResources(ctx, orgName, hasSamlBool, users)
	if err != nil {
		return nil, nil, err
	}

	pageToken, err := membersNextPageToken(bag, nextPage, parentID)
	if err != nil {
		return nil, nil, err
	}

	var annos annotations.Annotations
	annos.WithRateLimiting(selectRateLimit(restApiRateLimit, graphqlRateLimit))

	return rv, &resource.SyncOpResults{
		NextPageToken: pageToken,
		Annotations:   annos,
	}, nil
}

func (o *userResourceType) buildUserResources(
	ctx context.Context, orgName string, hasSAML bool, users []*github.User,
) ([]*v2.Resource, *v2.RateLimitDescription, error) {
	rv := make([]*v2.Resource, 0, len(users))
	var graphqlRateLimit *v2.RateLimitDescription
	for _, user := range users {
		ur, rl, err := o.buildUserResource(ctx, orgName, hasSAML, user)
		if err != nil {
			return nil, nil, err
		}
		rv = append(rv, ur)
		if rl != nil {
			graphqlRateLimit = rl
		}
	}
	return rv, graphqlRateLimit, nil
}

func selectRateLimit(rest, graphql *v2.RateLimitDescription) *v2.RateLimitDescription {
	if rest == nil {
		return graphql
	}
	if graphql != nil && graphql.Remaining < rest.Remaining {
		return graphql
	}
	return rest
}

func isEmail(email string) bool {
	_, err := mail.ParseAddress(email)
	return err == nil
}

func (o *userResourceType) Entitlements(_ context.Context, _ *v2.Resource, _ resource.SyncOpAttrs) ([]*v2.Entitlement, *resource.SyncOpResults, error) {
	return nil, &resource.SyncOpResults{}, nil
}

func (o *userResourceType) Grants(_ context.Context, _ *v2.Resource, _ resource.SyncOpAttrs) ([]*v2.Grant, *resource.SyncOpResults, error) {
	return nil, &resource.SyncOpResults{}, nil
}

func (o *userResourceType) Delete(ctx context.Context, resourceId *v2.ResourceId) (annotations.Annotations, error) {
	if resourceId.ResourceType != resourceTypeUser.Id {
		return nil, fmt.Errorf("baton-github: non-user resource passed to user delete")
	}

	orgs, err := getOrgs(ctx, o.client, o.orgs)
	if err != nil {
		return nil, err
	}

	userID, err := strconv.ParseInt(resourceId.GetResource(), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("baton-github: invalid invitation id")
	}

	user, resp, err := o.client.Users.GetByID(ctx, userID)
	if err != nil {
		return nil, wrapGitHubError(err, resp, "baton-github: invalid userID")
	}

	var (
		isRemoved = false
	)
	for _, org := range orgs {
		resp, err = o.client.Organizations.RemoveOrgMembership(ctx, user.GetLogin(), org)
		if err == nil {
			isRemoved = true
		}
	}

	if !isRemoved {
		return nil, wrapGitHubError(err, resp, "baton-github: failed to remove user from organizations")
	}

	restApiRateLimit, err := extractRateLimitData(resp)
	if err != nil {
		return nil, err
	}

	var annotations annotations.Annotations
	annotations.WithRateLimiting(restApiRateLimit)
	return annotations, nil
}

func userBuilder(client *github.Client, hasSAMLEnabled *bool, graphqlClient *githubv4.Client, orgCache *orgNameCache, orgs []string) *userResourceType {
	return &userResourceType{
		resourceType:   resourceTypeUser,
		client:         client,
		graphqlClient:  graphqlClient,
		hasSAMLEnabled: hasSAMLEnabled,
		orgCache:       orgCache,
		orgs:           orgs,
	}
}

func (o *userResourceType) hasSAML(ctx context.Context, orgName string) (bool, error) {
	if o.hasSAMLEnabled != nil {
		return *o.hasSAMLEnabled, nil
	}

	l := ctxzap.Extract(ctx)
	samlBool := false
	q := hasSAMLQuery{}
	variables := map[string]interface{}{
		"orgLoginName": githubv4.String(orgName),
	}
	err := o.graphqlClient.Query(ctx, &q, variables)
	if err != nil {
		// When SAML is configured at the Enterprise level (not org level),
		// GitHub returns this error. Fall back to treating SAML as disabled.
		if strings.Contains(err.Error(), "SAML identity provider is disabled when an Enterprise SAML identity provider is available") {
			l.Debug("org SAML disabled in favor of Enterprise SAML, skipping SAML identity enrichment",
				zap.String("org", orgName))
			o.hasSAMLEnabled = &samlBool
			return false, nil
		}
		return false, err
	}
	if q.Organization.SamlIdentityProvider.Id != "" {
		samlBool = true
	}
	o.hasSAMLEnabled = &samlBool
	return *o.hasSAMLEnabled, nil
}
