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
	"github.com/shurcooL/githubv4"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	// GraphQL variable names, shared by the query text and the variable map so
	// the two cannot drift: a typo'd map key reaches the API as a missing
	// variable rather than failing to compile.
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
	// One aliased invitation lookup per member of a page.
	enterpriseInvitationBatchSize = enterpriseMemberPageSize
	// Bounds the walks that happen inside one call rather than across page
	// tokens, so a mispaginating API cannot pin a provisioning task.
	enterpriseMaxPages = 1000
)

// enterpriseOwnerState is what the connector can observe about a user's Owner
// access: whether they hold the role today, and the invitation that would give
// it to them once they accept it.
type enterpriseOwnerState struct {
	enterpriseID        string
	isOwner             bool
	pendingInvitationID string
}

// githubEnterpriseAdministratorClient reads and writes the built-in Owner role
// of one enterprise. It needs both of the app's installations, because GitHub
// splits the data across them:
//
//   - Enterprise.ownerInfo, which holds admins and pendingAdminInvitations,
//     resolves to null for an installation token, so the owners are read from
//     Organization.enterpriseOwners with the organization token. The
//     enterprise token gets FORBIDDEN on any organization field.
//   - The invitation lookup and every mutation are enterprise fields, so they
//     go through the enterprise token.
//
// Enterprise.members(role: OWNER) is deliberately not used: that role is
// EnterpriseUserAccountMembershipRole, whose OWNER means "owner of an
// organization in the enterprise", not owner of the enterprise account.
type githubEnterpriseAdministratorClient struct {
	enterpriseClient *githubv4.Client
	orgClient        *githubv4.Client
	org              string
	// Immutable for a given slug, so it is resolved once at construction
	// rather than on each Grant and Revoke.
	enterpriseNodeID string
	// batchClient serves the aliased invitation lookup, and omits
	// enterpriseGraphQLTransport because that batch always reports NOT_FOUND
	// entries the classifier would read as a failure.
	endpoint    *url.URL
	batchClient *uhttp.BaseHttpClient
}

// newEnterpriseAdministratorClient returns the client for one enterprise
// installation, with a GraphQL client per token because the two installations
// are separate credentials.
//
// The HTTP clients arrive unwrapped because that is what both consumers take:
// githubv4 builds its own client over one, uhttp wraps one. They already carry
// the installation token, its 401 refresh, and uhttp's transport underneath.
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

	// NewBaseHttpClient reports a failed cache setup by returning nil, which
	// only panics later inside Do.
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

// newEnterpriseGraphQLClient returns a GraphQL client that classifies both the
// HTTP status and the errors[] GitHub returns alongside an HTTP 200. Both
// layers are needed, because the SDK only backs off on a typed Unavailable and
// a rate limit arrives as a successful status.
//
// The connector's shared GraphQL client keeps only the status classifier:
// user.go detects enterprise SAML by matching the text of its error.
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

// enterpriseGraphQLEndpoint returns the GraphQL URL of the instance, which is
// api.github.com for GitHub.com and /api/graphql on a self-hosted host.
//
// The trailing slash is trimmed before the comparison, so "https://github.com/"
// resolves to GitHub.com rather than to a /api/graphql path on that host.
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

// annotations returns the remaining GraphQL budget, or nil when the response
// carried no rateLimit block: the zero value would otherwise be reported as an
// exhausted budget with no reset time.
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

// enterpriseUser is a user account as the enterprise reports it, whether as an
// owner or as a plain member.
type enterpriseUser struct {
	databaseID int64
	login      string
}

// owners returns one page of the users who currently own the enterprise
// account.
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

// resolveEnterpriseNodeID looks up the node ID every enterprise mutation takes
// and stores it on the client. It is called once at construction: the ID is
// immutable for a given slug, and resolving it per operation would add two
// requests to every Grant and Revoke, which each read OwnerState twice.
//
// It doubles as the check that the enterprise is visible to this installation.
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

// verifyOrganization checks that the organization the owners are read from
// belongs to this enterprise. Otherwise the connector would report another
// enterprise's owners as owners of this one.
//
// organizations(query:) is a substring search, so an enterprise with many
// similarly named organizations can push the exact match past the first page.
// Every page is read before concluding that the organization is not there.
func (c *githubEnterpriseAdministratorClient) verifyOrganization(ctx context.Context, enterprise string) error {
	var after *githubv4.String
	for page := 0; page < enterpriseMaxPages; page++ {
		var query struct {
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
			return fmt.Errorf(
				"baton-github: organization %s does not belong to enterprise %s, so its owners cannot be synced",
				c.org, enterprise)
		}
		after = githubv4.NewString(query.Enterprise.Organizations.PageInfo.EndCursor)
	}

	return fmt.Errorf(
		"baton-github: gave up looking for organization %s in enterprise %s after %d pages",
		c.org, enterprise, enterpriseMaxPages)
}

// enterpriseMembersQuery reads one page of the enterprise's member accounts.
// members is a union: an enterprise with Enterprise Managed Users returns
// EnterpriseUserAccount, a regular enterprise can also return User, so both
// shapes are selected and each field falls back to the other branch.
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

// members returns one page of the enterprise's member accounts and the cursor
// of the next page, skipping any node without a database ID because guessing
// one would re-key the identity on the next sync.
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

// pendingOwnerInvitations returns the pending Owner invitation of each given
// login that has one, keyed by login, resolved in a single request.
//
// Invitations are not enumerable for an installation token, because
// Enterprise.ownerInfo resolves to null, so they can only be asked for one
// login at a time. Aliasing collapses that into one request, which GitHub
// charges a single rate-limit point.
//
// Logins travel as variables and only generated alias names reach the query
// text, so a login cannot alter the query. A login with no invitation answers
// with a NOT_FOUND entry, so those are dropped before the remaining errors are
// classified: left in, they would claim the code for the whole response, and
// NOT_FOUND is the one code the SDK downgrades to a warning. The rate limit is
// returned on the failure paths too, since that is when C1 needs it.
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
		if err := json.Unmarshal(raw, &node); err != nil || node.ID == "" {
			continue
		}
		invitations[login] = node.ID
	}

	return invitations, annos, nil
}

// doGraphQL runs a query whose selection set is built at runtime, which the
// typed client cannot express. It returns the top-level fields undecoded plus
// the errors array, so the caller decides which fields to read and which
// errors are expected. A non-2xx arrives already mapped onto a gRPC code by
// uhttp, so its error is returned unwrapped.
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
		return nil, nil, fmt.Errorf("creating GraphQL request: %w", err)
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

// pendingOwnerInvitation returns the ID of the pending Owner invitation for a
// login, or an empty string when there is none. GitHub reports a login without
// an invitation as NOT_FOUND rather than as a null field.
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

// OwnerState reports whether a login owns the enterprise account and whether
// an invitation for it is still pending, together with the remaining budget.
//
// Both facts are resolved even for an active owner, because Revoke acts on
// each separately: stopping at the role would let a stale invitation survive
// the demotion and then fail the verification that follows it.
//
// The owners connection does take a query argument, but it is a search rather
// than an exact-login filter, so an empty result cannot be trusted to mean
// "not an owner" — on Revoke that reading would report GrantAlreadyRevoked
// while the user keeps the role. The pages are walked and matched on the login
// instead, which in practice is one request: owners are a small set.
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
		// The freshest budget, not one descriptor per page.
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
	// Falling through the bound would report "not an owner" for someone the
	// walk never finished reading, which Revoke would answer with
	// GrantAlreadyRevoked while they still hold the role.
	if !walked {
		return state, annos, fmt.Errorf(
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
	// GraphQL rejects a mutation without a selection set, and rateLimit exists
	// only on Query, so every mutation selects clientMutationId.
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
