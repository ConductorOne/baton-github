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
	"github.com/conductorone/baton-sdk/pkg/session"
	"github.com/conductorone/baton-sdk/pkg/types/resource"
	"github.com/conductorone/baton-sdk/pkg/types/sessions"
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

// enterpriseEmailInfo holds SAML identity data from the enterprise consumed licenses API.
// Fields are exported for JSON serialization via session.Store.
type enterpriseEmailInfo struct {
	SAMLNameID string `json:"saml_name_id"`
}

const enterpriseEmailPrefix = "enterprise-email:"
const enterpriseEmailCacheLoadedKey = "enterprise-email-cache-loaded"

type userResourceType struct {
	resourceType  *v2.ResourceType
	client        *github.Client
	graphqlClient *githubv4.Client
	hasSAMLEnabled *bool
	orgCache      *orgNameCache
	orgs          []string
	customClient  *customclient.Client
	enterprises   []string
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

	hasSamlBool, err := o.hasSAML(ctx, orgName, opts.Session)
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
					if loadErr := o.loadEnterpriseEmailCache(ctx, opts.Session); loadErr != nil {
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
		} else if len(o.enterprises) > 0 {
			// Org-level SAML is not available but enterprise is configured.
			// Defer to the enterprise SAML cache as the source of truth — if
			// the user has a SAML identity, use it. If not, leave email blank
			// rather than using the REST API public email, which is not a
			// corporate identity.
			userEmail = o.getEnterpriseSAMLEmail(ctx, opts.Session, u.GetLogin())
			if userEmail != "" {
				l.Debug("enriched user email from enterprise consumed licenses",
					zap.String("user", u.GetLogin()),
					zap.String("email", userEmail))
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

// loadEnterpriseEmailCache fetches enterprise consumed licenses and stores
// SAML NameID data in the session store, keyed by lowercase GitHub login.
func (o *userResourceType) loadEnterpriseEmailCache(ctx context.Context, ss sessions.SessionStore) error {
	l := ctxzap.Extract(ctx)

	// Check if cache has already been loaded this sync.
	_, found, err := session.GetJSON[bool](ctx, ss, enterpriseEmailCacheLoadedKey)
	if err != nil {
		return err
	}
	if found {
		return nil
	}

	if o.customClient == nil || len(o.enterprises) == 0 {
		_ = session.SetJSON(ctx, ss, enterpriseEmailCacheLoadedKey, true)
		return nil
	}

	userCount := 0
	for _, enterprise := range o.enterprises {
		page := 1
		for {
			consumedLicenses, _, err := o.customClient.ListEnterpriseConsumedLicenses(ctx, enterprise, page)
			if err != nil {
				// Mark as loaded so we don't retry; partial data is still available.
				_ = session.SetJSON(ctx, ss, enterpriseEmailCacheLoadedKey, true)
				return fmt.Errorf("baton-github: failed to fetch enterprise consumed licenses for %s (page %d): %w", enterprise, page, err)
			}

			if len(consumedLicenses.Users) == 0 {
				break
			}

			batch := make(map[string]*enterpriseEmailInfo, len(consumedLicenses.Users))
			for _, user := range consumedLicenses.Users {
				if user.GitHubComLogin == "" {
					continue
				}
				info := &enterpriseEmailInfo{}
				if user.GitHubComSAMLNameID != nil {
					info.SAMLNameID = *user.GitHubComSAMLNameID
				}
				key := enterpriseEmailPrefix + strings.ToLower(user.GitHubComLogin)
				batch[key] = info
			}
			if err := session.SetManyJSON(ctx, ss, batch); err != nil {
				_ = session.SetJSON(ctx, ss, enterpriseEmailCacheLoadedKey, true)
				return fmt.Errorf("baton-github: failed to store enterprise email batch for %s (page %d): %w", enterprise, page, err)
			}
			userCount += len(batch)
			page++
		}
	}

	l.Info("loaded enterprise email cache",
		zap.Int("user_count", userCount))
	_ = session.SetJSON(ctx, ss, enterpriseEmailCacheLoadedKey, true)
	return nil
}

// getEnterpriseSAMLEmail looks up the SAML NameID for a user from the
// enterprise consumed-licenses data stored in the session store.
// Returns empty string if no SAML email is available.
func (o *userResourceType) getEnterpriseSAMLEmail(ctx context.Context, ss sessions.SessionStore, login string) string {
	key := enterpriseEmailPrefix + strings.ToLower(login)
	info, found, err := session.GetJSON[enterpriseEmailInfo](ctx, ss, key)
	if err != nil || !found {
		return ""
	}

	if info.SAMLNameID != "" && isEmail(info.SAMLNameID) {
		return info.SAMLNameID
	}

	return ""
}

func (o *userResourceType) hasSAML(ctx context.Context, orgName string, ss sessions.SessionStore) (bool, error) {
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
			if loadErr := o.loadEnterpriseEmailCache(ctx, ss); loadErr != nil {
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
		if loadErr := o.loadEnterpriseEmailCache(ctx, ss); loadErr != nil {
			l.Warn("failed to load enterprise email cache", zap.Error(loadErr))
		}
	}

	return *o.hasSAMLEnabled, nil
}
