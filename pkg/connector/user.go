package connector

import (
	"context"
	"fmt"
	"net/http"
	"net/mail"
	"strconv"
	"strings"

	"github.com/conductorone/baton-github/pkg/customclient"
	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/annotations"
	"github.com/conductorone/baton-sdk/pkg/pagination"
	"github.com/conductorone/baton-sdk/pkg/types/resource"
	"github.com/conductorone/baton-sdk/pkg/uhttp"
	"github.com/google/go-github/v69/github"
	"github.com/grpc-ecosystem/go-grpc-middleware/logging/zap/ctxzap"
	"github.com/shurcooL/githubv4"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Create a new connector resource for a GitHub user.
func userResource(ctx context.Context, user *github.User, userEmail string, extraEmails []string) (*v2.Resource, error) {
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
	resourceType       *v2.ResourceType
	client             *github.Client
	graphqlClient      *githubv4.Client
	customClient       *customclient.Client
	hasSAMLEnabled     *bool
	orgCache           *orgNameCache
	orgs               []string
	enterprises        []string
	enterpriseSAMLData map[string]*enterpriseUserSAML // login -> SAML data
}

func (o *userResourceType) ResourceType(_ context.Context) *v2.ResourceType {
	return o.resourceType
}

func (o *userResourceType) List(ctx context.Context, parentID *v2.ResourceId, pt *pagination.Token) ([]*v2.Resource, string, annotations.Annotations, error) {
	l := ctxzap.Extract(ctx)
	var annotations annotations.Annotations
	if parentID == nil {
		return nil, "", nil, nil
	}

	bag, page, err := parsePageToken(pt.Token, &v2.ResourceId{ResourceType: resourceTypeUser.Id})
	if err != nil {
		return nil, "", nil, err
	}

	orgName, err := o.orgCache.GetOrgName(ctx, parentID)
	if err != nil {
		return nil, "", nil, err
	}

	hasSamlBool, err := o.hasSAML(ctx, orgName)
	if err != nil {
		return nil, "", nil, err
	}

	// If org-level SAML is disabled but we have enterprise config, try to load enterprise SAML data.
	// This handles the case where SAML is configured at the enterprise level instead of org level.
	useEnterpriseSAML := false
	if !hasSamlBool && len(o.enterprises) > 0 {
		if err := o.loadEnterpriseSAMLData(ctx); err != nil {
			l.Warn("failed to load enterprise SAML data", zap.Error(err))
		}
		if len(o.enterpriseSAMLData) > 0 {
			useEnterpriseSAML = true
			l.Debug("using enterprise SAML data for user email enrichment",
				zap.String("org", orgName),
				zap.Int("enterprise_saml_users", len(o.enterpriseSAMLData)))
		}
	}

	var restApiRateLimit *v2.RateLimitDescription

	opts := github.ListMembersOptions{
		ListOptions: github.ListOptions{
			Page:    page,
			PerPage: maxPageSize,
		},
	}

	users, resp, err := o.client.Organizations.ListMembers(ctx, orgName, &opts)
	if err != nil {
		return nil, "", nil, wrapGitHubError(err, resp, "github-connector: failed to list organization members")
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

	q := listUsersQuery{}
	rv := make([]*v2.Resource, 0, len(users))
	for _, user := range users {
		u, res, err := o.client.Users.GetByID(ctx, user.GetID())
		if err != nil {
			if isRatelimited(res) {
				return nil, "", nil, uhttp.WrapErrors(codes.Unavailable, "too many requests", err)
			}
			// This undocumented API can return 404 for some users. If this fails it means we won't get some of their details like email
			if res == nil || res.StatusCode != http.StatusNotFound {
				return nil, "", nil, err
			}
			l.Error("error fetching user by id", zap.Error(err), zap.Int64("user_id", user.GetID()))
			u = user
		}
		userEmail := u.GetEmail()
		var extraEmails []string
		if hasSamlBool {
			// Use org-level SAML
			variables := map[string]interface{}{
				"orgLoginName": githubv4.String(orgName),
				"userName":     githubv4.String(u.GetLogin()),
			}
			err = o.graphqlClient.Query(ctx, &q, variables)
			if err != nil {
				return nil, "", nil, err
			}
			if len(q.Organization.SamlIdentityProvider.ExternalIdentities.Edges) == 1 {
				samlIdent := q.Organization.SamlIdentityProvider.ExternalIdentities.Edges[0].Node.SamlIdentity
				userEmail = samlIdent.NameId
				setUserEmail := false

				if userEmail != "" {
					setUserEmail = true
				}
				for _, email := range samlIdent.Emails {
					ok := isEmail(email.Value)
					if !ok {
						continue
					}

					if !setUserEmail {
						userEmail = email.Value
						setUserEmail = true
					} else {
						extraEmails = append(extraEmails, email.Value)
					}
				}
			}
		} else if useEnterpriseSAML {
			// Use enterprise-level SAML data when org-level SAML is not available
			enterpriseEmail, enterpriseExtraEmails := o.getEnterpriseSAMLEmail(u.GetLogin())
			if enterpriseEmail != "" {
				userEmail = enterpriseEmail
				extraEmails = enterpriseExtraEmails
			}
		}
		ur, err := userResource(ctx, u, userEmail, extraEmails)
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

func isEmail(email string) bool {
	_, err := mail.ParseAddress(email)
	return err == nil
}

func (o *userResourceType) Entitlements(_ context.Context, _ *v2.Resource, _ *pagination.Token) ([]*v2.Entitlement, string, annotations.Annotations, error) {
	return nil, "", nil, nil
}

func (o *userResourceType) Grants(_ context.Context, _ *v2.Resource, _ *pagination.Token) ([]*v2.Grant, string, annotations.Annotations, error) {
	return nil, "", nil, nil
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

func userBuilder(
	client *github.Client,
	hasSAMLEnabled *bool,
	graphqlClient *githubv4.Client,
	customClient *customclient.Client,
	orgCache *orgNameCache,
	orgs []string,
	enterprises []string,
) *userResourceType {
	return &userResourceType{
		resourceType:       resourceTypeUser,
		client:             client,
		graphqlClient:      graphqlClient,
		customClient:       customClient,
		hasSAMLEnabled:     hasSAMLEnabled,
		orgCache:           orgCache,
		orgs:               orgs,
		enterprises:        enterprises,
		enterpriseSAMLData: make(map[string]*enterpriseUserSAML),
	}
}

// loadEnterpriseSAMLData fetches SAML identity data from the enterprise consumed-licenses endpoint
// and builds a lookup map by user login. This is used when org-level SAML is disabled because
// SAML is configured at the enterprise level.
func (o *userResourceType) loadEnterpriseSAMLData(ctx context.Context) error {
	l := ctxzap.Extract(ctx)

	if len(o.enterprises) == 0 || o.customClient == nil {
		return nil
	}

	// Only load once
	if len(o.enterpriseSAMLData) > 0 {
		return nil
	}

	l.Debug("loading enterprise SAML data", zap.Strings("enterprises", o.enterprises))

	for _, enterprise := range o.enterprises {
		page := 0
		for {
			consumedLicenses, _, err := o.customClient.ListEnterpriseConsumedLicenses(ctx, enterprise, page)
			if err != nil {
				l.Warn("failed to load enterprise consumed licenses for SAML data",
					zap.String("enterprise", enterprise),
					zap.Error(err))
				// Don't fail the sync, just continue without enterprise SAML data
				return nil
			}

			if len(consumedLicenses.Users) == 0 {
				break
			}

			for _, user := range consumedLicenses.Users {
				if user.GitHubComLogin == "" {
					continue
				}

				samlData := &enterpriseUserSAML{}

				// Get SAML name ID if available
				if user.GitHubComSAMLNameID != nil && *user.GitHubComSAMLNameID != "" {
					samlData.SAMLNameID = *user.GitHubComSAMLNameID
				}

				// Get verified domain emails
				if len(user.GitHubComVerifiedDomainEmails) > 0 {
					samlData.VerifiedEmails = user.GitHubComVerifiedDomainEmails
				}

				// Only store if we have useful data
				if samlData.SAMLNameID != "" || len(samlData.VerifiedEmails) > 0 {
					o.enterpriseSAMLData[user.GitHubComLogin] = samlData
				}
			}

			page++
		}
	}

	l.Debug("loaded enterprise SAML data", zap.Int("users_with_saml_data", len(o.enterpriseSAMLData)))
	return nil
}

// getEnterpriseSAMLEmail returns the best email for a user from enterprise SAML data.
// Returns the SAML name ID if it looks like an email, otherwise returns the first verified email.
func (o *userResourceType) getEnterpriseSAMLEmail(login string) (string, []string) {
	samlData, ok := o.enterpriseSAMLData[login]
	if !ok {
		return "", nil
	}

	// Check if SAML name ID is an email
	if samlData.SAMLNameID != "" && isEmail(samlData.SAMLNameID) {
		primary := samlData.SAMLNameID
		var extra []string
		// Add verified emails as extra emails
		for _, email := range samlData.VerifiedEmails {
			if email != primary {
				extra = append(extra, email)
			}
		}
		return primary, extra
	}

	// Use verified emails
	if len(samlData.VerifiedEmails) > 0 {
		primary := samlData.VerifiedEmails[0]
		var extra []string
		if len(samlData.VerifiedEmails) > 1 {
			extra = samlData.VerifiedEmails[1:]
		}
		return primary, extra
	}

	return "", nil
}

func (o *userResourceType) hasSAML(ctx context.Context, orgName string) (bool, error) {
	l := ctxzap.Extract(ctx)
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
		// Check if the error is due to Enterprise SAML being configured instead of org-level SAML.
		// In this case, we should not fail but instead treat org-level SAML as disabled.
		if strings.Contains(err.Error(), "SAML identity provider is disabled when an Enterprise SAML identity provider is available") ||
			strings.Contains(err.Error(), "Organization's SAML identity provider is disabled") {
			l.Info("org-level SAML is disabled because Enterprise SAML is configured",
				zap.String("org", orgName),
				zap.Error(err))
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
