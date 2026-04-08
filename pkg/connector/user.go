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

type samlState int

const (
	samlStateUnknown    samlState = iota // not yet checked
	samlStateOrgEnabled                  // org-level SAML, use GraphQL
	samlStateEnterprise                  // enterprise SAML, use consumed licenses API
	samlStateDisabled                    // no SAML
)

const (
	// enterpriseSAMLKeyPrefix is prepended to each GitHub login to form
	// individual session keys, e.g. "enterprise_saml:octocat".
	enterpriseSAMLKeyPrefix = "enterprise_saml:"

	// enterpriseSAMLKeysIndex is the session key that stores the list of all
	// enterprise_saml:* keys. This allows bulk-reading SAML mappings with
	// GetManyJSON without scanning the entire session store.
	enterpriseSAMLKeysIndex = "enterprise_saml_keys"
)

type userResourceType struct {
	resourceType  *v2.ResourceType
	client        *github.Client
	graphqlClient *githubv4.Client
	samlStates    map[string]samlState // per-org SAML state, keyed by org name
	orgCache      *orgNameCache
	orgs          []string
	customClient  *customclient.Client
	enterprises   []string
}

func (u *userResourceType) ResourceType(_ context.Context) *v2.ResourceType {
	return u.resourceType
}

func (u *userResourceType) List(ctx context.Context, parentID *v2.ResourceId, opts resource.SyncOpAttrs) ([]*v2.Resource, *resource.SyncOpResults, error) {
	l := ctxzap.Extract(ctx)
	var annotations annotations.Annotations
	if parentID == nil {
		return nil, &resource.SyncOpResults{}, nil
	}

	bag, page, err := parsePageToken(opts.PageToken.Token, &v2.ResourceId{ResourceType: resourceTypeUser.Id})
	if err != nil {
		return nil, nil, err
	}

	orgName, err := u.orgCache.GetOrgName(ctx, opts.Session, parentID)
	if err != nil {
		return nil, nil, err
	}

	currentSAMLState, err := u.checkOrgSAML(ctx, orgName)
	if err != nil {
		return nil, nil, err
	}

	// For enterprise SAML: on the first page, fetch from the API and store in
	// session. On every page, bulk-read the mappings into a local map so the
	// user loop can do plain map lookups with no session calls.
	var enterpriseSAMLEmails map[string]string
	if currentSAMLState == samlStateEnterprise {
		_, alreadyFetched, err := session.GetJSON[[]string](ctx, opts.Session, enterpriseSAMLKeysIndex)
		if err != nil {
			return nil, nil, fmt.Errorf("baton-github: error checking enterprise SAML session: %w", err)
		}
		if !alreadyFetched {
			if err := u.fetchAndStoreEnterpriseSAML(ctx, opts.Session); err != nil {
				l.Debug("failed to fetch enterprise SAML emails, falling back to REST API emails",
					zap.Error(err))
				// Write empty sentinel so we don't retry for remaining orgs in this sync
				if setErr := session.SetJSON(ctx, opts.Session, enterpriseSAMLKeysIndex, []string{}); setErr != nil {
					l.Debug("failed to write empty SAML sentinel to session", zap.Error(setErr))
				}
				u.samlStates[orgName] = samlStateDisabled
				currentSAMLState = samlStateDisabled
			}
		}
		if currentSAMLState == samlStateEnterprise {
			enterpriseSAMLEmails, err = loadEnterpriseSAMLEmails(ctx, opts.Session)
			if err != nil {
				return nil, nil, err
			}
		}
	}

	var restApiRateLimit *v2.RateLimitDescription

	listOpts := github.ListMembersOptions{
		ListOptions: github.ListOptions{
			Page:    page,
			PerPage: maxPageSize,
		},
	}

	users, resp, err := u.client.Organizations.ListMembers(ctx, orgName, &listOpts)
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

	var lastGraphQLRateLimit *struct {
		Limit     int
		Remaining int
		ResetAt   githubv4.DateTime
	}
	rv := make([]*v2.Resource, 0, len(users))
	for _, user := range users {
		ghUser, res, err := u.client.Users.GetByID(ctx, user.GetID())
		if err != nil {
			// This undocumented API can return 404 for some users. If this fails it means we won't get some of their details like email
			if isNotFoundError(res) {
				l.Warn("error fetching user by id", zap.Error(err), zap.Int64("user_id", user.GetID()))
				ghUser = user
			} else {
				return nil, nil, wrapGitHubError(err, res, "github-connector: failed to get user by id")
			}
		}
		userEmail := ghUser.GetEmail()
		var extraEmails []string

		switch currentSAMLState {
		case samlStateUnknown:
			return nil, nil, fmt.Errorf("baton-github: unexpected unknown SAML state for org %s", orgName)
		case samlStateOrgEnabled:
			q := listUsersQuery{}
			variables := map[string]interface{}{
				"orgLoginName": githubv4.String(orgName),
				"userName":     githubv4.String(ghUser.GetLogin()),
			}
			err = u.graphqlClient.Query(ctx, &q, variables)

			if err != nil {
				return nil, nil, err
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
			lastGraphQLRateLimit = &struct {
				Limit     int
				Remaining int
				ResetAt   githubv4.DateTime
			}{
				Limit:     q.RateLimit.Limit,
				Remaining: q.RateLimit.Remaining,
				ResetAt:   q.RateLimit.ResetAt,
			}

		case samlStateEnterprise:
			key := enterpriseSAMLKeyPrefix + strings.ToLower(ghUser.GetLogin())
			if samlEmail, ok := enterpriseSAMLEmails[key]; ok && isEmail(samlEmail) {
				userEmail = samlEmail
			}

		case samlStateDisabled:
			// no SAML enrichment
		}

		ur, err := userResource(ctx, ghUser, userEmail, extraEmails)
		if err != nil {
			return nil, nil, err
		}

		rv = append(rv, ur)
	}
	annotations.WithRateLimiting(restApiRateLimit)
	if lastGraphQLRateLimit != nil && int64(lastGraphQLRateLimit.Remaining) < restApiRateLimit.Remaining {
		graphqlRateLimit := &v2.RateLimitDescription{
			Limit:     int64(lastGraphQLRateLimit.Limit),
			Remaining: int64(lastGraphQLRateLimit.Remaining),
			ResetAt:   timestamppb.New(lastGraphQLRateLimit.ResetAt.Time),
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

func (u *userResourceType) Entitlements(_ context.Context, _ *v2.Resource, _ resource.SyncOpAttrs) ([]*v2.Entitlement, *resource.SyncOpResults, error) {
	return nil, &resource.SyncOpResults{}, nil
}

func (u *userResourceType) Grants(_ context.Context, _ *v2.Resource, _ resource.SyncOpAttrs) ([]*v2.Grant, *resource.SyncOpResults, error) {
	return nil, &resource.SyncOpResults{}, nil
}

func (u *userResourceType) Delete(ctx context.Context, resourceId *v2.ResourceId) (annotations.Annotations, error) {
	if resourceId.ResourceType != resourceTypeUser.Id {
		return nil, fmt.Errorf("baton-github: non-user resource passed to user delete")
	}

	orgs, err := getOrgs(ctx, u.client, u.orgs)
	if err != nil {
		return nil, err
	}

	userID, err := strconv.ParseInt(resourceId.GetResource(), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("baton-github: invalid invitation id")
	}

	user, resp, err := u.client.Users.GetByID(ctx, userID)
	if err != nil {
		return nil, wrapGitHubError(err, resp, "baton-github: invalid userID")
	}

	var (
		isRemoved = false
	)
	for _, org := range orgs {
		resp, err = u.client.Organizations.RemoveOrgMembership(ctx, user.GetLogin(), org)
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

func userBuilder(client *github.Client, graphqlClient *githubv4.Client, orgCache *orgNameCache, orgs []string, customClient *customclient.Client, enterprises []string) *userResourceType {
	return &userResourceType{
		resourceType:  resourceTypeUser,
		client:        client,
		graphqlClient: graphqlClient,
		samlStates:    make(map[string]samlState),
		orgCache:      orgCache,
		orgs:          orgs,
		customClient:  customClient,
		enterprises:   enterprises,
	}
}

// checkOrgSAML queries GitHub to determine the SAML configuration.
// Returns one of: samlStateOrgEnabled, samlStateEnterprise, samlStateDisabled.
// The result is cached on the struct so subsequent calls skip the GraphQL query.
func (u *userResourceType) checkOrgSAML(ctx context.Context, orgName string) (samlState, error) {
	if state, ok := u.samlStates[orgName]; ok {
		return state, nil
	}

	l := ctxzap.Extract(ctx)
	q := hasSAMLQuery{}
	variables := map[string]interface{}{
		"orgLoginName": githubv4.String(orgName),
	}
	err := u.graphqlClient.Query(ctx, &q, variables)
	if err != nil {
		// When SAML is configured at the Enterprise level (not org level),
		// GitHub returns this error.
		if strings.Contains(err.Error(), "SAML identity provider is disabled when an Enterprise SAML identity provider is available") {
			if len(u.enterprises) == 0 {
				l.Debug("enterprise SAML detected but no enterprises configured, skipping SAML enrichment",
					zap.String("org", orgName))
				u.samlStates[orgName] = samlStateDisabled
				return u.samlStates[orgName], nil
			}
			l.Debug("org SAML disabled in favor of Enterprise SAML, will use consumed licenses API",
				zap.String("org", orgName))
			u.samlStates[orgName] = samlStateEnterprise
			return u.samlStates[orgName], nil
		}
		return samlStateUnknown, err
	}
	if q.Organization.SamlIdentityProvider.Id == "" {
		if len(u.enterprises) > 0 {
			l.Debug("no org-level SAML provider found but enterprises configured, will try consumed licenses API",
				zap.String("org", orgName))
			u.samlStates[orgName] = samlStateEnterprise
			return u.samlStates[orgName], nil
		}
		l.Debug("no SAML identity provider found for org, disabling SAML enrichment",
			zap.String("org", orgName))
		u.samlStates[orgName] = samlStateDisabled
		return u.samlStates[orgName], nil
	}

	ssoUrl := string(q.Organization.SamlIdentityProvider.SsoUrl)
	if strings.Contains(ssoUrl, "/enterprises/") && len(u.enterprises) > 0 {
		l.Debug("SAML provider SSO URL points to enterprise, will use consumed licenses API",
			zap.String("org", orgName),
			zap.String("sso_url", ssoUrl))
		u.samlStates[orgName] = samlStateEnterprise
		return u.samlStates[orgName], nil
	}

	if strings.Contains(ssoUrl, "/enterprises/") && len(u.enterprises) == 0 {
		l.Debug("SAML provider SSO URL points to enterprise but no enterprises configured, skipping SAML enrichment",
			zap.String("org", orgName),
			zap.String("sso_url", ssoUrl))
		u.samlStates[orgName] = samlStateDisabled
		return u.samlStates[orgName], nil
	}

	l.Debug("org-level SAML provider found, will use GraphQL for SAML identity lookups",
		zap.String("org", orgName),
		zap.String("sso_url", ssoUrl))
	u.samlStates[orgName] = samlStateOrgEnabled
	return u.samlStates[orgName], nil
}

// fetchAndStoreEnterpriseSAML pages through the consumed licenses API for all
// configured enterprises, aggregates the login-to-SAML-email mappings, and
// writes them to the session store in a single batch. It also stores the list
// of keys under enterpriseSAMLKeysIndex so that loadEnterpriseSAMLEmails can
// bulk-read them back on subsequent List pages.
func (u *userResourceType) fetchAndStoreEnterpriseSAML(ctx context.Context, ss sessions.SessionStore) error {
	l := ctxzap.Extract(ctx)
	samlByLogin := make(map[string]string)

	for _, enterprise := range u.enterprises {
		// GitHub's consumed-licenses API is 1-indexed; page 0 is undocumented
		// and may return the same results as page 1, causing duplicates.
		page := 1
		for {
			consumedLicenses, _, err := u.customClient.ListEnterpriseConsumedLicenses(ctx, enterprise, page)
			if err != nil {
				return fmt.Errorf("baton-github: error fetching enterprise consumed licenses for %s: %w", enterprise, err)
			}
			if len(consumedLicenses.Users) == 0 {
				break
			}

			for _, user := range consumedLicenses.Users {
				if user.GitHubComSAMLNameID != nil && *user.GitHubComSAMLNameID != "" && user.GitHubComLogin != "" {
					key := enterpriseSAMLKeyPrefix + strings.ToLower(user.GitHubComLogin)
					samlByLogin[key] = *user.GitHubComSAMLNameID
				}
			}
			page++
		}
	}

	keys := make([]string, 0, len(samlByLogin))
	for k := range samlByLogin {
		keys = append(keys, k)
	}
	if len(samlByLogin) > 0 {
		if err := session.SetManyJSON(ctx, ss, samlByLogin); err != nil {
			return fmt.Errorf("baton-github: error storing enterprise SAML mappings: %w", err)
		}
	}
	// Always write the key index - its presence in the session tells future
	// List() calls that we already fetched for this sync, even if zero
	// SAML mappings were found.
	if err := session.SetJSON(ctx, ss, enterpriseSAMLKeysIndex, keys); err != nil {
		return fmt.Errorf("baton-github: error storing enterprise SAML key index: %w", err)
	}

	l.Debug("stored enterprise SAML mappings in session", zap.Int("count", len(samlByLogin)))
	return nil
}

// loadEnterpriseSAMLEmails bulk-reads all enterprise SAML mappings from the
// session store in two calls: one to get the key index, one to get the values.
// Returns a map of "enterprise_saml:<login>" -> SAML email for use as a local
// lookup table in the List loop (no session calls needed per user).
func loadEnterpriseSAMLEmails(ctx context.Context, ss sessions.SessionStore) (map[string]string, error) {
	keys, found, err := session.GetJSON[[]string](ctx, ss, enterpriseSAMLKeysIndex)
	if err != nil {
		return nil, fmt.Errorf("baton-github: error reading enterprise SAML key index: %w", err)
	}
	if !found || len(keys) == 0 {
		return nil, nil
	}

	samlByLogin, err := session.GetManyJSON[string](ctx, ss, keys)
	if err != nil {
		return nil, fmt.Errorf("baton-github: error reading enterprise SAML mappings: %w", err)
	}
	return samlByLogin, nil
}
