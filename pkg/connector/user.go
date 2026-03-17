package connector

import (
	"context"
	"fmt"
	"net/mail"
	"strconv"
	"strings"

	"github.com/conductorone/baton-github/pkg/customclient"
	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/annotations"
	"github.com/conductorone/baton-sdk/pkg/types/resource"
	"github.com/google/go-github/v69/github"
	"github.com/grpc-ecosystem/go-grpc-middleware/logging/zap/ctxzap"
	"github.com/shurcooL/githubv4"
	"go.uber.org/zap"
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

// enterpriseEmailInfo holds email data from the enterprise consumed licenses API.
type enterpriseEmailInfo struct {
	samlNameID           string
	verifiedDomainEmails []string
}

type userResourceType struct {
	resourceType   *v2.ResourceType
	client         *github.Client
	graphqlClient  *githubv4.Client
	hasSAMLEnabled *bool
	orgCache       *orgNameCache
	orgs           []string
	customClient   *customclient.Client
	enterprises    []string
	// enterpriseEmailCache maps GitHub login to enterprise email info.
	// Populated lazily when enterprise SAML is detected.
	enterpriseEmailCache map[string]*enterpriseEmailInfo
}

func (o *userResourceType) ResourceType(_ context.Context) *v2.ResourceType {
	return o.resourceType
}

func (o *userResourceType) List(ctx context.Context, parentID *v2.ResourceId, opts resource.SyncOpAttrs) ([]*v2.Resource, *resource.SyncOpResults, error) {
	l := ctxzap.Extract(ctx)
	var annotations annotations.Annotations
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

	hasSamlBool, err := o.hasSAML(ctx, orgName)
	if err != nil {
		return nil, nil, err
	}
	var restApiRateLimit *v2.RateLimitDescription

	listOpts := github.ListMembersOptions{
		ListOptions: github.ListOptions{
			Page:    page,
			PerPage: maxPageSize,
		},
	}

	users, resp, err := o.client.Organizations.ListMembers(ctx, orgName, &listOpts)
	if err != nil {
		return nil, nil, wrapGitHubError(err, resp, "github-connector: failed to list organization members")
	}

	restApiRateLimit, err = extractRateLimitData(resp)
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

	q := listUsersQuery{}
	rv := make([]*v2.Resource, 0, len(users))
	for _, user := range users {
		u, res, err := o.client.Users.GetByID(ctx, user.GetID())
		if err != nil {
			// This undocumented API can return 404 for some users. If this fails it means we won't get some of their details like email
			if isNotFoundError(res) {
				l.Warn("error fetching user by id", zap.Error(err), zap.Int64("user_id", user.GetID()))
				u = user
			} else {
				return nil, nil, wrapGitHubError(err, res, "github-connector: failed to get user by id")
			}
		}
		userEmail := u.GetEmail()
		var extraEmails []string
		if hasSamlBool {
			variables := map[string]interface{}{
				"orgLoginName": githubv4.String(orgName),
				"userName":     githubv4.String(u.GetLogin()),
			}
			err = o.graphqlClient.Query(ctx, &q, variables)
			if err != nil {
				// When SAML is configured at the Enterprise level (not org level),
				// GitHub returns this error. Fall back to using the regular user email
				// and disable further SAML queries for this connector instance.
				if strings.Contains(err.Error(), "SAML identity provider is disabled when an Enterprise SAML identity provider is available") {
					l.Info("org SAML disabled in favor of Enterprise SAML, falling back to enterprise consumed licenses API for email enrichment",
						zap.String("org", orgName),
						zap.String("user", u.GetLogin()))
					samlDisabled := false
					o.hasSAMLEnabled = &samlDisabled
					hasSamlBool = false
					// Load enterprise email data so we can enrich users
					if loadErr := o.loadEnterpriseEmailCache(ctx); loadErr != nil {
						l.Warn("failed to load enterprise email cache", zap.Error(loadErr))
					}
				} else {
					return nil, nil, err
				}
			}
			if err == nil && len(q.Organization.SamlIdentityProvider.ExternalIdentities.Edges) == 1 {
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
		}
		// If org-level SAML is not available, try enterprise email enrichment.
		// Enterprise SAML emails override REST API emails (consistent with
		// org-level SAML behavior where SAML NameID overrides REST email).
		if !hasSamlBool {
			if entEmail, entExtraEmails := o.getEnterpriseEmails(u.GetLogin()); entEmail != "" {
				if userEmail != "" && userEmail != entEmail {
					extraEmails = append(extraEmails, userEmail)
				}
				l.Debug("enriched user email from enterprise consumed licenses",
					zap.String("user", u.GetLogin()),
					zap.String("email", entEmail))
				userEmail = entEmail
				extraEmails = append(extraEmails, entExtraEmails...)
			}
		}
		ur, err := userResource(ctx, u, userEmail, extraEmails)
		if err != nil {
			return nil, nil, err
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

	return rv, &resource.SyncOpResults{
		NextPageToken: pageToken,
		Annotations:   annotations,
	}, nil
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

func userBuilder(
	client *github.Client,
	hasSAMLEnabled *bool,
	graphqlClient *githubv4.Client,
	orgCache *orgNameCache,
	orgs []string,
	customClient *customclient.Client,
	enterprises []string,
) *userResourceType {
	return &userResourceType{
		resourceType:   resourceTypeUser,
		client:         client,
		graphqlClient:  graphqlClient,
		hasSAMLEnabled: hasSAMLEnabled,
		orgCache:       orgCache,
		orgs:           orgs,
		customClient:   customClient,
		enterprises:    enterprises,
	}
}

// loadEnterpriseEmailCache fetches enterprise consumed licenses and builds a
// lookup map from GitHub login to SAML/verified-domain email data.
func (o *userResourceType) loadEnterpriseEmailCache(ctx context.Context) error {
	l := ctxzap.Extract(ctx)

	if o.enterpriseEmailCache != nil {
		return nil
	}

	if o.customClient == nil || len(o.enterprises) == 0 {
		o.enterpriseEmailCache = make(map[string]*enterpriseEmailInfo)
		return nil
	}

	cache := make(map[string]*enterpriseEmailInfo)
	for _, enterprise := range o.enterprises {
		page := 1
		for {
			consumedLicenses, _, err := o.customClient.ListEnterpriseConsumedLicenses(ctx, enterprise, page)
			if err != nil {
				o.enterpriseEmailCache = cache
				return fmt.Errorf("baton-github: failed to fetch enterprise consumed licenses for %s (page %d): %w", enterprise, page, err)
			}

			if len(consumedLicenses.Users) == 0 {
				break
			}

			for _, user := range consumedLicenses.Users {
				if user.GitHubComLogin == "" {
					continue
				}
				info := &enterpriseEmailInfo{
					verifiedDomainEmails: user.GitHubComVerifiedDomainEmails,
				}
				if user.GitHubComSAMLNameID != nil {
					info.samlNameID = *user.GitHubComSAMLNameID
				}
				cache[strings.ToLower(user.GitHubComLogin)] = info
			}
			page++
		}
	}

	l.Info("loaded enterprise email cache",
		zap.Int("user_count", len(cache)))
	o.enterpriseEmailCache = cache
	return nil
}

// getEnterpriseEmails returns the primary email and extra emails for a user
// from the enterprise consumed licenses data. Returns empty strings if no
// enterprise email data is available.
func (o *userResourceType) getEnterpriseEmails(login string) (string, []string) {
	if o.enterpriseEmailCache == nil {
		return "", nil
	}

	info, ok := o.enterpriseEmailCache[strings.ToLower(login)]
	if !ok {
		return "", nil
	}

	var primaryEmail string
	var extraEmails []string

	// Prefer SAML NameID as primary email if it's a valid email
	if info.samlNameID != "" && isEmail(info.samlNameID) {
		primaryEmail = info.samlNameID
	}

	// Add verified domain emails
	for _, email := range info.verifiedDomainEmails {
		if !isEmail(email) {
			continue
		}
		if primaryEmail == "" {
			primaryEmail = email
		} else if email != primaryEmail {
			extraEmails = append(extraEmails, email)
		}
	}

	return primaryEmail, extraEmails
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
			l.Info("org SAML disabled in favor of Enterprise SAML, will use enterprise consumed licenses API for email enrichment",
				zap.String("org", orgName))
			o.hasSAMLEnabled = &samlBool
			// Proactively load enterprise email data
			if loadErr := o.loadEnterpriseEmailCache(ctx); loadErr != nil {
				l.Warn("failed to load enterprise email cache", zap.Error(loadErr))
			}
			return false, nil
		}
		return false, err
	}
	if q.Organization.SamlIdentityProvider.Id != "" {
		samlBool = true
	}
	o.hasSAMLEnabled = &samlBool

	// If org has no SAML but we have enterprises configured, proactively
	// load the enterprise email cache for email enrichment.
	if !samlBool && len(o.enterprises) > 0 {
		l.Info("org has no SAML provider, will use enterprise consumed licenses API for email enrichment",
			zap.String("org", orgName))
		if loadErr := o.loadEnterpriseEmailCache(ctx); loadErr != nil {
			l.Warn("failed to load enterprise email cache", zap.Error(loadErr))
		}
	}

	return *o.hasSAMLEnabled, nil
}
