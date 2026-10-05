package connector

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/annotations"
	"github.com/conductorone/baton-sdk/pkg/uhttp"
	"github.com/grpc-ecosystem/go-grpc-middleware/logging/zap/ctxzap"
	"github.com/shurcooL/githubv4"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	// GraphQL variable names, shared by the query text and the variable map.
	enterpriseSlugVariable  = "slug"
	enterpriseLoginVariable = "login"
	enterpriseOrgVariable   = "org"
	enterpriseRoleVariable  = "role"
	enterpriseQueryVariable = "query"
	enterpriseFirstVariable = "first"
	enterpriseAfterVariable = "after"

	enterpriseGraphQLPath    = "/api/graphql"
	githubDotComGraphQL      = "https://api.github.com/graphql"
	enterpriseRateLimitField = "rateLimit"

	// GitHub caps every connection used here at 100 per page.
	enterpriseOwnerPageSize        = 100
	enterpriseOrganizationPageSize = 100
	enterpriseMemberPageSize       = 100
	// A login search also matches display names, so it can return many accounts.
	enterpriseMemberLookupPageSize = 100
	// One aliased invitation lookup per member of a page.
	enterpriseInvitationBatchSize = enterpriseMemberPageSize
	// Bounds in-call page walks so a mispaginating API cannot hang a provisioning task.
	enterpriseMaxPages = 1000
)

// enterpriseOwnerState is a user's Owner role and pending Owner invitation, if any.
type enterpriseOwnerState struct {
	enterpriseID        string
	isOwner             bool
	pendingInvitationID string
}

// holdsRole reports whether C1 should see a grant. C1 has no pending state, so an
// unaccepted invitation counts.
func (s enterpriseOwnerState) holdsRole() bool {
	return s.isOwner || s.pendingInvitationID != ""
}

// githubEnterpriseAdministratorClient reads and writes the Owner role of one
// enterprise. It needs both installation tokens:
//
//   - Enterprise.ownerInfo is null for installation tokens, so owners are read
//     from Organization.enterpriseOwners with the org token.
//   - The invitation lookup and mutations need the enterprise token.
//
// Enterprise.members(role: OWNER) is not used: it returns owners of organizations
// in the enterprise, not owners of the enterprise account.
type githubEnterpriseAdministratorClient struct {
	enterpriseClient *githubv4.Client
	orgClient        *githubv4.Client
	org              string
	enterpriseNodeID string
	endpoint         *url.URL
	// batchClient skips enterpriseGraphQLTransport because the aliased invitation
	// lookup always contains NOT_FOUND entries.
	batchClient *uhttp.BaseHttpClient
}

// newEnterpriseAdministratorClient returns the client for one enterprise, with a
// GraphQL client per installation token.
func newEnterpriseAdministratorClient(
	instanceURL string,
	enterpriseHTTPClient *http.Client,
	orgHTTPClient *http.Client,
	org string,
) (*githubEnterpriseAdministratorClient, error) {
	endpoint, err := enterpriseGraphQLEndpoint(instanceURL)
	if err != nil {
		return nil, err
	}

	// NewBaseHttpClient returns nil when its cache setup fails.
	batchClient := uhttp.NewBaseHttpClient(enterpriseHTTPClient)
	if batchClient == nil {
		return nil, fmt.Errorf("baton-github: error building the enterprise GraphQL batch client")
	}

	return &githubEnterpriseAdministratorClient{
		enterpriseClient: newEnterpriseGraphQLClient(endpoint.String(), enterpriseHTTPClient),
		orgClient:        newEnterpriseGraphQLClient(endpoint.String(), orgHTTPClient),
		org:              org,
		endpoint:         endpoint,
		batchClient:      batchClient,
	}, nil
}

// newEnterpriseGraphQLClient returns a GraphQL client that also classifies the
// errors GitHub returns inside an HTTP 200, such as rate limits.
func newEnterpriseGraphQLClient(endpoint string, httpClient *http.Client) *githubv4.Client {
	base := httpClient.Transport
	if base == nil {
		base = http.DefaultTransport
	}

	return githubv4.NewEnterpriseClient(endpoint, &http.Client{
		Timeout: httpClient.Timeout,
		Transport: &enterpriseGraphQLTransport{
			base: &statusClassifyingTransport{base: base},
		},
	})
}

// enterpriseGraphQLEndpoint returns the instance's GraphQL URL: api.github.com
// for GitHub.com, /api/graphql on a self-hosted host.
func enterpriseGraphQLEndpoint(instanceURL string) (*url.URL, error) {
	instanceURL = strings.TrimSuffix(instanceURL, "/")
	if instanceURL == "" || instanceURL == githubDotCom {
		return url.Parse(githubDotComGraphQL)
	}

	gqlURL, err := url.Parse(instanceURL)
	if err != nil {
		return nil, err
	}
	gqlURL.Path = enterpriseGraphQLPath

	return gqlURL, nil
}

type graphQLRateLimit struct {
	Limit     githubv4.Int
	Remaining githubv4.Int
	ResetAt   githubv4.DateTime
}

// annotations returns the remaining GraphQL budget, or nil when the response had
// no rateLimit block.
func (r graphQLRateLimit) annotations() annotations.Annotations {
	if r.Limit == 0 && r.ResetAt.IsZero() {
		return nil
	}

	rateLimit := &v2.RateLimitDescription{
		Status:    v2.RateLimitDescription_STATUS_OK,
		Limit:     int64(r.Limit),
		Remaining: int64(r.Remaining),
	}
	if r.Remaining <= 0 {
		rateLimit.Status = v2.RateLimitDescription_STATUS_OVERLIMIT
	}
	if !r.ResetAt.IsZero() {
		rateLimit.ResetAt = timestamppb.New(r.ResetAt.Time)
	}

	return annotations.New(rateLimit)
}

// enterpriseOwnersQuery reads one page of the enterprise account's owners
// through the organization the app is installed on.
type enterpriseOwnersQuery struct {
	Organization struct {
		EnterpriseOwners struct {
			Nodes []struct {
				DatabaseID githubv4.Int
				Login      githubv4.String
			}
			PageInfo struct {
				HasNextPage githubv4.Boolean
				EndCursor   githubv4.String
			}
		} `graphql:"enterpriseOwners(first: $first, after: $after)"`
	} `graphql:"organization(login: $org)"`
	RateLimit graphQLRateLimit
}

// enterpriseUser is a user account as the enterprise reports it.
type enterpriseUser struct {
	databaseID int64
	login      string
}

// owners returns one page of the enterprise account's owners.
func (c *githubEnterpriseAdministratorClient) owners(
	ctx context.Context,
	after *githubv4.String,
) ([]enterpriseUser, string, annotations.Annotations, error) {
	var query enterpriseOwnersQuery
	err := c.orgClient.Query(ctx, &query, map[string]any{
		enterpriseOrgVariable:   githubv4.String(c.org),
		enterpriseFirstVariable: githubv4.Int(enterpriseOwnerPageSize),
		enterpriseAfterVariable: after,
	})
	if err != nil {
		return nil, "", nil, fmt.Errorf("baton-github: error listing enterprise owners of org %s: %w", c.org, err)
	}

	owners := make([]enterpriseUser, 0, len(query.Organization.EnterpriseOwners.Nodes))
	for _, node := range query.Organization.EnterpriseOwners.Nodes {
		owners = append(owners, enterpriseUser{
			databaseID: int64(node.DatabaseID),
			login:      string(node.Login),
		})
	}

	nextCursor := ""
	if query.Organization.EnterpriseOwners.PageInfo.HasNextPage {
		nextCursor = string(query.Organization.EnterpriseOwners.PageInfo.EndCursor)
	}

	return owners, nextCursor, query.RateLimit.annotations(), nil
}

// resolveEnterpriseNodeID stores the node ID enterprise mutations take. It also
// checks that the enterprise is visible to this installation.
func (c *githubEnterpriseAdministratorClient) resolveEnterpriseNodeID(ctx context.Context, enterprise string) error {
	var query struct {
		Enterprise struct {
			ID githubv4.String
		} `graphql:"enterprise(slug: $slug)"`
	}
	err := c.enterpriseClient.Query(ctx, &query, map[string]any{
		enterpriseSlugVariable: githubv4.String(enterprise),
	})
	if err != nil {
		return fmt.Errorf("baton-github: error getting enterprise %s: %w", enterprise, err)
	}
	if query.Enterprise.ID == "" {
		return fmt.Errorf("baton-github: enterprise %s is not visible to the GitHub App", enterprise)
	}
	c.enterpriseNodeID = string(query.Enterprise.ID)

	return nil
}

// enterpriseOrganizationsQuery reads one page of an enterprise's
// organizations, narrowed by a search term.
type enterpriseOrganizationsQuery struct {
	Enterprise struct {
		Organizations struct {
			Nodes []struct {
				Login githubv4.String
			}
			PageInfo struct {
				HasNextPage githubv4.Boolean
				EndCursor   githubv4.String
			}
		} `graphql:"organizations(first: $first, after: $after, query: $query)"`
	} `graphql:"enterprise(slug: $slug)"`
}

// verifyOrganization checks that the organization owners are read through
// belongs to this enterprise. organizations(query:) is a substring search, so
// every page is read before concluding it is not there.
func (c *githubEnterpriseAdministratorClient) verifyOrganization(ctx context.Context, enterprise string) error {
	var after *githubv4.String
	walked := false
	for page := 0; page < enterpriseMaxPages; page++ {
		var query enterpriseOrganizationsQuery
		err := c.enterpriseClient.Query(ctx, &query, map[string]any{
			enterpriseSlugVariable:  githubv4.String(enterprise),
			enterpriseFirstVariable: githubv4.Int(enterpriseOrganizationPageSize),
			enterpriseAfterVariable: after,
			enterpriseQueryVariable: githubv4.String(c.org),
		})
		if err != nil {
			return fmt.Errorf("baton-github: error listing organizations of enterprise %s: %w", enterprise, err)
		}

		for _, node := range query.Enterprise.Organizations.Nodes {
			if strings.EqualFold(string(node.Login), c.org) {
				return nil
			}
		}

		if !query.Enterprise.Organizations.PageInfo.HasNextPage {
			walked = true
			break
		}
		after = githubv4.NewString(query.Enterprise.Organizations.PageInfo.EndCursor)
	}
	// Running out of pages is not proof the organization is missing.
	if !walked {
		return status.Errorf(codes.Internal,
			"baton-github: gave up looking for organization %s in enterprise %s after %d pages",
			c.org, enterprise, enterpriseMaxPages)
	}

	return enterpriseSetupError{status.Errorf(codes.FailedPrecondition,
		"baton-github: organization %s does not belong to enterprise %s, so its owners cannot be synced",
		c.org, enterprise)}
}

// enterpriseMemberLookupQuery searches the enterprise's members by login. The
// search also matches display names, so the caller pages until an exact login.
type enterpriseMemberLookupQuery struct {
	Enterprise struct {
		Members struct {
			Nodes []struct {
				EnterpriseUserAccount struct {
					Login githubv4.String
				} `graphql:"... on EnterpriseUserAccount"`
				User struct {
					Login githubv4.String
				} `graphql:"... on User"`
			}
			PageInfo struct {
				HasNextPage githubv4.Boolean
				EndCursor   githubv4.String
			}
		} `graphql:"members(first: $first, after: $after, query: $query)"`
	} `graphql:"enterprise(slug: $slug)"`
	RateLimit graphQLRateLimit
}

// isMember reports whether login belongs to the enterprise. Grants() can only see
// invitations addressed to members.
func (c *githubEnterpriseAdministratorClient) isMember(
	ctx context.Context,
	enterprise string,
	login string,
) (bool, annotations.Annotations, error) {
	var (
		annos annotations.Annotations
		after *githubv4.String
	)
	for page := 0; page < enterpriseMaxPages; page++ {
		var query enterpriseMemberLookupQuery
		err := c.enterpriseClient.Query(ctx, &query, map[string]any{
			enterpriseSlugVariable:  githubv4.String(enterprise),
			enterpriseFirstVariable: githubv4.Int(enterpriseMemberLookupPageSize),
			enterpriseAfterVariable: after,
			enterpriseQueryVariable: githubv4.String(login),
		})
		if err != nil {
			return false, annos, fmt.Errorf("baton-github: error looking up member %s of enterprise %s: %w", login, enterprise, err)
		}
		annos = freshestRateLimit(annos, query.RateLimit.annotations())

		for _, node := range query.Enterprise.Members.Nodes {
			found := string(node.EnterpriseUserAccount.Login)
			if found == "" {
				found = string(node.User.Login)
			}
			if strings.EqualFold(found, login) {
				return true, annos, nil
			}
		}

		if !bool(query.Enterprise.Members.PageInfo.HasNextPage) {
			return false, annos, nil
		}
		after = githubv4.NewString(query.Enterprise.Members.PageInfo.EndCursor)
	}

	return false, annos, status.Errorf(codes.Unavailable,
		"baton-github: gave up looking for %s in enterprise %s after %d pages of search results",
		login, enterprise, enterpriseMaxPages)
}

// enterpriseMembersQuery reads one page of the enterprise's members. members is
// a union of EnterpriseUserAccount (EMU) and User, so both shapes are selected.
type enterpriseMembersQuery struct {
	Enterprise struct {
		Members struct {
			Nodes []struct {
				EnterpriseUserAccount struct {
					Login githubv4.String
					User  struct {
						DatabaseID githubv4.Int
						Login      githubv4.String
					}
				} `graphql:"... on EnterpriseUserAccount"`
				User struct {
					DatabaseID githubv4.Int
					Login      githubv4.String
				} `graphql:"... on User"`
			}
			PageInfo struct {
				HasNextPage githubv4.Boolean
				EndCursor   githubv4.String
			}
		} `graphql:"members(first: $first, after: $after)"`
	} `graphql:"enterprise(slug: $slug)"`
	RateLimit graphQLRateLimit
}

// members returns one page of the enterprise's members and the next cursor,
// skipping nodes without a database ID.
func (c *githubEnterpriseAdministratorClient) members(
	ctx context.Context,
	enterprise string,
	after *githubv4.String,
) ([]enterpriseUser, string, annotations.Annotations, error) {
	var query enterpriseMembersQuery
	err := c.enterpriseClient.Query(ctx, &query, map[string]any{
		enterpriseSlugVariable:  githubv4.String(enterprise),
		enterpriseFirstVariable: githubv4.Int(enterpriseMemberPageSize),
		enterpriseAfterVariable: after,
	})
	if err != nil {
		return nil, "", nil, fmt.Errorf("baton-github: error listing members of enterprise %s: %w", enterprise, err)
	}

	members := make([]enterpriseUser, 0, len(query.Enterprise.Members.Nodes))
	for _, node := range query.Enterprise.Members.Nodes {
		member := enterpriseUser{
			databaseID: int64(node.EnterpriseUserAccount.User.DatabaseID),
			login:      string(node.EnterpriseUserAccount.Login),
		}
		if member.databaseID == 0 {
			member.databaseID = int64(node.User.DatabaseID)
		}
		if member.login == "" {
			member.login = string(node.User.Login)
		}
		if member.databaseID == 0 || member.login == "" {
			ctxzap.Extract(ctx).Debug("baton-github: skipping an enterprise member with no database ID or login",
				zap.String("enterprise", enterprise),
				zap.Int64("database_id", member.databaseID),
				zap.String("login", member.login),
			)
			continue
		}
		members = append(members, member)
	}

	nextCursor := ""
	if query.Enterprise.Members.PageInfo.HasNextPage {
		nextCursor = string(query.Enterprise.Members.PageInfo.EndCursor)
	}

	return members, nextCursor, query.RateLimit.annotations(), nil
}

// pendingOwnerInvitations returns the pending Owner invitation of each login that
// has one, in one aliased request. Invitations can't be listed with an
// installation token, so they're looked up per login.
//
// Logins are passed as variables; only generated aliases reach the query text.
// NOT_FOUND entries (no invitation) are dropped before classifying the rest.
func (c *githubEnterpriseAdministratorClient) pendingOwnerInvitations(
	ctx context.Context,
	enterprise string,
	logins []string,
) (map[string]string, annotations.Annotations, error) {
	if len(logins) == 0 {
		return map[string]string{}, nil, nil
	}
	if len(logins) > enterpriseInvitationBatchSize {
		return nil, nil, fmt.Errorf(
			"baton-github: pending owner invitation lookup takes at most %d logins, got %d",
			enterpriseInvitationBatchSize, len(logins))
	}

	declarations := []string{"$" + enterpriseSlugVariable + ":String!", "$" + enterpriseRoleVariable + ":EnterpriseAdministratorRole!"}
	selections := make([]string, 0, len(logins))
	variables := map[string]any{
		enterpriseSlugVariable: enterprise,
		enterpriseRoleVariable: string(githubv4.EnterpriseAdministratorRoleOwner),
	}
	aliasLogin := make(map[string]string, len(logins))
	for i, login := range logins {
		alias := fmt.Sprintf("i%d", i)
		variable := fmt.Sprintf("l%d", i)
		aliasLogin[alias] = login
		variables[variable] = login
		declarations = append(declarations, "$"+variable+":String!")
		selections = append(selections, fmt.Sprintf(
			"%s: enterpriseAdministratorInvitation(enterpriseSlug: $%s, userLogin: $%s, role: $%s){id}",
			alias, enterpriseSlugVariable, variable, enterpriseRoleVariable))
	}

	query := fmt.Sprintf("query(%s){%s %s{limit remaining resetAt}}",
		strings.Join(declarations, ","), strings.Join(selections, " "), enterpriseRateLimitField)

	aliases, graphQLErrors, err := c.doGraphQL(ctx, query, variables)

	var rateLimit graphQLRateLimit
	if raw, ok := aliases[enterpriseRateLimitField]; ok {
		if unmarshalErr := json.Unmarshal(raw, &rateLimit); unmarshalErr != nil {
			return nil, nil, fmt.Errorf("baton-github: error decoding the rate limit of enterprise %s: %w", enterprise, unmarshalErr)
		}
	}
	annos := rateLimit.annotations()
	if err != nil {
		return nil, annos, fmt.Errorf("baton-github: error listing pending owner invitations of enterprise %s: %w", enterprise, err)
	}
	unexpected := make([]graphQLError, 0, len(graphQLErrors))
	for _, graphQLErr := range graphQLErrors {
		if graphQLErrorType(graphQLErr) != graphQLErrorNotFound {
			unexpected = append(unexpected, graphQLErr)
		}
	}
	if len(unexpected) > 0 {
		return nil, annos, status.Errorf(graphQLErrorsCode(unexpected),
			"baton-github: error listing pending owner invitations of enterprise %s: %s",
			enterprise, unexpected[0].Message)
	}

	invitations := make(map[string]string, len(aliases))
	for alias, raw := range aliases {
		login, ok := aliasLogin[alias]
		if !ok {
			continue
		}
		var node struct {
			ID string `json:"id"`
		}
		// Only skip an absent invitation, not one that failed to decode.
		if err := json.Unmarshal(raw, &node); err != nil {
			return nil, annos, fmt.Errorf(
				"baton-github: error decoding the invitation reported for %s: %w", login, err)
		}
		if node.ID == "" {
			continue
		}
		invitations[login] = node.ID
	}

	return invitations, annos, nil
}

// doGraphQL runs a query built at runtime, which the typed client can't express,
// and returns the raw data fields and the errors array.
func (c *githubEnterpriseAdministratorClient) doGraphQL(
	ctx context.Context,
	query string,
	variables map[string]any,
) (map[string]json.RawMessage, []graphQLError, error) {
	req, err := c.batchClient.NewRequest(ctx, http.MethodPost, c.endpoint,
		uhttp.WithContentTypeJSONHeader(),
		uhttp.WithAcceptJSONHeader(),
		uhttp.WithJSONBody(map[string]any{"query": query, "variables": variables}),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("baton-github: error creating the GraphQL request: %w", err)
	}

	var envelope graphQLEnvelope
	resp, err := c.batchClient.Do(req, uhttp.WithJSONResponse(&envelope))
	if err != nil {
		if resp != nil {
			_ = resp.Body.Close()
		}
		return nil, nil, err
	}
	defer resp.Body.Close()

	return envelope.Data, envelope.Errors, nil
}

// pendingOwnerInvitation returns the pending Owner invitation ID for a login, or
// "" if none. GitHub reports no invitation as NOT_FOUND.
func (c *githubEnterpriseAdministratorClient) pendingOwnerInvitation(
	ctx context.Context,
	enterprise string,
	login string,
) (string, error) {
	var query struct {
		EnterpriseAdministratorInvitation struct {
			ID githubv4.String
		} `graphql:"enterpriseAdministratorInvitation(enterpriseSlug: $slug, userLogin: $login, role: $role)"`
	}
	err := c.enterpriseClient.Query(ctx, &query, map[string]any{
		enterpriseSlugVariable:  githubv4.String(enterprise),
		enterpriseLoginVariable: githubv4.String(login),
		enterpriseRoleVariable:  githubv4.EnterpriseAdministratorRoleOwner,
	})
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return "", nil
		}
		return "", fmt.Errorf("baton-github: error getting owner invitation for %s: %w", login, err)
	}

	return string(query.EnterpriseAdministratorInvitation.ID), nil
}

// OwnerState reports whether a login owns the enterprise and whether an Owner
// invitation is pending. Both are resolved because Revoke clears each.
//
// Owners are paged and matched by login rather than using the owners query
// argument, which is a search that can lag right after a mutation.
func (c *githubEnterpriseAdministratorClient) OwnerState(
	ctx context.Context,
	enterprise string,
	login string,
) (enterpriseOwnerState, annotations.Annotations, error) {
	state := enterpriseOwnerState{enterpriseID: c.enterpriseNodeID}

	var annos annotations.Annotations
	var after *githubv4.String
	walked := false
	for page := 0; page < enterpriseMaxPages; page++ {
		owners, nextCursor, pageAnnos, err := c.owners(ctx, after)
		if err != nil {
			return state, annos, err
		}
		if len(pageAnnos) > 0 {
			annos = pageAnnos
		}
		for _, owner := range owners {
			if strings.EqualFold(owner.login, login) {
				state.isOwner = true
				break
			}
		}
		if state.isOwner || nextCursor == "" {
			walked = true
			break
		}
		after = githubv4.NewString(githubv4.String(nextCursor))
	}
	// An unfinished walk must not read as "not an owner".
	if !walked {
		return state, annos, status.Errorf(codes.Internal,
			"baton-github: gave up reading the owners of enterprise %s after %d pages",
			enterprise, enterpriseMaxPages)
	}

	invitationID, err := c.pendingOwnerInvitation(ctx, enterprise, login)
	if err != nil {
		return state, annos, err
	}
	state.pendingInvitationID = invitationID

	return state, annos, nil
}

// UpdateRole changes the role of someone who already administers the
// enterprise. It cannot promote a plain member.
func (c *githubEnterpriseAdministratorClient) UpdateRole(
	ctx context.Context,
	enterpriseID string,
	login string,
	role githubv4.EnterpriseAdministratorRole,
) error {
	// GraphQL requires a selection set and rateLimit exists only on Query.
	var mutation struct {
		UpdateEnterpriseAdministratorRole struct {
			ClientMutationID githubv4.String
		} `graphql:"updateEnterpriseAdministratorRole(input: $input)"`
	}
	input := githubv4.UpdateEnterpriseAdministratorRoleInput{
		EnterpriseID: githubv4.ID(enterpriseID),
		Login:        githubv4.String(login),
		Role:         role,
	}
	if err := c.enterpriseClient.Mutate(ctx, &mutation, input, nil); err != nil {
		return fmt.Errorf("baton-github: error setting enterprise role of %s to %s: %w", login, role, err)
	}

	return nil
}

// InviteOwner sends the Owner invitation a member has to accept.
func (c *githubEnterpriseAdministratorClient) InviteOwner(ctx context.Context, enterpriseID string, login string) error {
	var mutation struct {
		InviteEnterpriseAdmin struct {
			ClientMutationID githubv4.String
		} `graphql:"inviteEnterpriseAdmin(input: $input)"`
	}
	role := githubv4.EnterpriseAdministratorRoleOwner
	input := githubv4.InviteEnterpriseAdminInput{
		EnterpriseID: githubv4.ID(enterpriseID),
		Invitee:      githubv4.NewString(githubv4.String(login)),
		Role:         &role,
	}
	if err := c.enterpriseClient.Mutate(ctx, &mutation, input, nil); err != nil {
		return fmt.Errorf("baton-github: error inviting %s as enterprise owner: %w", login, err)
	}

	return nil
}

func (c *githubEnterpriseAdministratorClient) CancelInvitation(ctx context.Context, invitationID string) error {
	var mutation struct {
		CancelEnterpriseAdminInvitation struct {
			ClientMutationID githubv4.String
		} `graphql:"cancelEnterpriseAdminInvitation(input: $input)"`
	}
	input := githubv4.CancelEnterpriseAdminInvitationInput{InvitationID: githubv4.ID(invitationID)}
	if err := c.enterpriseClient.Mutate(ctx, &mutation, input, nil); err != nil {
		return fmt.Errorf("baton-github: error cancelling enterprise owner invitation: %w", err)
	}

	return nil
}
