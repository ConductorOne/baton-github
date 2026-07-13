package connector

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/annotations"
	"github.com/conductorone/baton-sdk/pkg/pagination"
	"github.com/conductorone/baton-sdk/pkg/types/entitlement"
	"github.com/conductorone/baton-sdk/pkg/types/grant"
	resourceSdk "github.com/conductorone/baton-sdk/pkg/types/resource"
	"github.com/conductorone/baton-sdk/pkg/uhttp"
	"github.com/google/go-github/v69/github"
	"github.com/grpc-ecosystem/go-grpc-middleware/logging/zap/ctxzap"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"
)

const (
	orgRoleMember       = "member"
	orgRoleDirectMember = "direct_member" // invite
	orgRoleAdmin        = "admin"
)

var orgAccessLevels = []string{
	orgRoleAdmin,
	orgRoleMember,
}

type orgResourceType struct {
	resourceType *v2.ResourceType
	client       *github.Client
	appClient    *github.Client
	orgs         map[string]struct{}
	orgCache     *orgNameCache
	syncSecrets  bool
	// authScope isolates source-cache scopes per credential; GitHub ETag
	// visibility is credential-specific (see source_cache.go).
	authScope string
}

func organizationResource(
	ctx context.Context,
	org *github.Organization,
	parentResourceID *v2.ResourceId,
	syncSecrets bool,
) (*v2.Resource, error) {
	annotations := []proto.Message{
		&v2.ExternalLink{Url: org.GetHTMLURL()},
		&v2.V1Identifier{Id: fmt.Sprintf("org:%d", org.GetID())},
		&v2.ChildResourceType{ResourceTypeId: resourceTypeUser.Id},
		&v2.ChildResourceType{ResourceTypeId: resourceTypeTeam.Id},
		&v2.ChildResourceType{ResourceTypeId: resourceTypeRepository.Id},
		&v2.ChildResourceType{ResourceTypeId: resourceTypeOrgRole.Id},
		&v2.ChildResourceType{ResourceTypeId: resourceTypeInvitation.Id},
		&v2.ChildResourceType{ResourceTypeId: resourceTypeApp.Id},
	}
	if syncSecrets {
		annotations = append(annotations, &v2.ChildResourceType{ResourceTypeId: resourceTypeApiToken.Id})
	}

	return resourceSdk.NewResource(
		org.GetLogin(),
		resourceTypeOrg,
		org.GetID(),
		resourceSdk.WithParentResourceID(parentResourceID),
		resourceSdk.WithAnnotation(
			annotations...,
		),
	)
}

func (o *orgResourceType) ResourceType(_ context.Context) *v2.ResourceType {
	return o.resourceType
}

func (o *orgResourceType) List(
	ctx context.Context,
	parentResourceID *v2.ResourceId,
	opts resourceSdk.SyncOpAttrs,
) ([]*v2.Resource, *resourceSdk.SyncOpResults, error) {
	if o.appClient != nil {
		orgResource, pageToken, anno, err := o.listOrganizationsFromAppInstallations(ctx, parentResourceID)
		if err != nil {
			return nil, nil, err
		}
		return []*v2.Resource{orgResource}, &resourceSdk.SyncOpResults{
			NextPageToken: pageToken,
			Annotations:   anno,
		}, nil
	}

	l := ctxzap.Extract(ctx)

	bag, page, err := parsePageToken(opts.PageToken.Token, &v2.ResourceId{ResourceType: resourceTypeOrg.Id})
	if err != nil {
		return nil, nil, err
	}

	listOpts := &github.ListOptions{
		Page:    page,
		PerPage: maxPageSize,
	}

	orgs, resp, err := o.client.Organizations.List(ctx, "", listOpts)
	if err != nil {
		return nil, nil, wrapGitHubError(err, resp, "github-connector: failed to fetch organizations")
	}

	pageToken, reqAnnos, err := nextPageToken(bag, resp)
	if err != nil {
		return nil, nil, err
	}

	var ret []*v2.Resource
	for _, org := range orgs {
		if _, ok := o.orgs[org.GetLogin()]; !ok && len(o.orgs) > 0 {
			continue
		}
		membership, resp, err := o.client.Organizations.GetOrgMembership(ctx, "", org.GetLogin())
		if err != nil {
			if resp != nil && resp.StatusCode == http.StatusForbidden {
				l.Debug("insufficient access to list org membership, skipping org",
					zap.String("org", org.GetLogin()),
					zap.String("github_error", gitHubErrorMessage(err)),
				)
				continue
			}
			return nil, nil, wrapGitHubError(err, resp, "github-connector: failed to get org membership")
		}

		// Only sync orgs that we are an admin for
		if strings.ToLower(membership.GetRole()) != orgRoleAdmin {
			continue
		}

		orgResource, err := organizationResource(ctx, org, parentResourceID, o.syncSecrets)
		if err != nil {
			return nil, nil, err
		}

		ret = append(ret, orgResource)
	}

	return ret, &resourceSdk.SyncOpResults{
		NextPageToken: pageToken,
		Annotations:   reqAnnos,
	}, nil
}

func (o *orgResourceType) Entitlements(
	_ context.Context,
	resource *v2.Resource,
	_ resourceSdk.SyncOpAttrs,
) ([]*v2.Entitlement, *resourceSdk.SyncOpResults, error) {
	return nil, nil, nil
}

func (o *orgResourceType) StaticEntitlements(
	_ context.Context,
	_ resourceSdk.SyncOpAttrs,
) ([]*v2.Entitlement, *resourceSdk.SyncOpResults, error) {
	rv := make([]*v2.Entitlement, 0, len(orgAccessLevels))
	rv = append(rv, entitlement.NewAssignmentEntitlement(nil, orgRoleMember,
		entitlement.WithDisplayName("Org Member"),
		entitlement.WithDescription("Access to org in GitHub as member"),
		entitlement.WithGrantableTo(resourceTypeUser),
	))
	rv = append(rv, entitlement.NewPermissionEntitlement(nil, orgRoleAdmin,
		entitlement.WithDisplayName("Org Admin"),
		entitlement.WithDescription("Access to org in GitHub as admin"),
		entitlement.WithGrantableTo(resourceTypeUser),
	))

	return rv, &resourceSdk.SyncOpResults{}, nil
}

func (o *orgResourceType) orgRoleGrant(roleName string, org *v2.Resource, principalID *v2.ResourceId, userID int64) *v2.Grant {
	return grant.NewGrant(org, roleName, principalID, grant.WithAnnotation(&v2.V1Identifier{
		Id: fmt.Sprintf("org-grant:%s:%d:%s", org.Id.Resource, userID, roleName),
	}))
}

// orgMembersPageURL builds one page's URL with a fixed query-param order
// so the same logical page hashes to the same scope every sync.
func orgMembersPageURL(orgName, role string, page int) string {
	q := url.Values{}
	q.Set("page", strconv.Itoa(page))
	q.Set("per_page", strconv.Itoa(maxPageSize))
	q.Set("role", role)
	return fmt.Sprintf("orgs/%v/members?%s", orgName, q.Encode())
}

// listMembersConditional fetches one page of the org member listing for
// one role, with source-cache revalidation (see source_cache.go for the
// scope model, the horizon clamp, and token-carried validators).
func (o *orgResourceType) listMembersConditional(
	ctx context.Context,
	orgName string,
	role string,
	cursor pagedListCursor,
	opts resourceSdk.SyncOpAttrs,
) (*conditionalPage, error) {
	pageURL := orgMembersPageURL(orgName, role, cursor.Page)
	return listUsersPageConditional(ctx, o.client, opts.SourceCache, o.authScope, "org-members",
		pageURL, cursor, maxPageSize)
}

// cursorForResolvedPage builds a page cursor carrying the page's
// validator resolution from the origin's batched walk, so the page does
// no lookup of its own (zero ask/answer bounces on the Lambda
// continuation). Unqueried pages get an unresolved cursor (single-lookup
// fallback).
func cursorForResolvedPage(page int, horizon int, resolved map[int]resolvedPage) pagedListCursor {
	c := pagedListCursor{Page: page, Horizon: horizon}
	r, ok := resolved[page]
	if !ok {
		return c
	}
	if r.Found {
		c.Resolution = cursorValidatorHit
		c.Etag = r.Validator
	} else {
		c.Resolution = cursorValidatorMiss
	}
	return c
}

// spawnMemberPageCursors resolves one role collection's stored page chain
// (a single batched lookup — one ask bounce on the Lambda continuation)
// and computes the spawn set: on a warm sync the connector emits one
// sibling cursor per stored page 2..horizon and the SDK worker pool
// revalidates them concurrently instead of one round trip per page
// serially. Each token is a self-contained single-state pagination bag
// (the resource identity rides the spawned action) carrying the page's
// validator, so siblings do no lookups of their own.
//
// Returns the page-1 cursor (with its own resolution embedded), the
// marshaled sibling tokens (nil when there is no stored chain — cold
// syncs paginate serially exactly as before), or an error, which may
// wrap sourcecache.ErrLookupDeferred and must propagate.
func (o *orgResourceType) spawnMemberPageCursors(
	ctx context.Context,
	orgName string,
	role string,
	opts resourceSdk.SyncOpAttrs,
) (pagedListCursor, []string, error) {
	resolved, horizon, err := resolveStoredPageChain(ctx, o.client, opts.SourceCache, o.authScope, "org-members",
		func(page int) string { return orgMembersPageURL(orgName, role, page) })
	if err != nil {
		return pagedListCursor{}, nil, err
	}
	if horizon < 2 {
		// One stored page or none: nothing to fan out. Page 1 still
		// carries its own resolution (and the real horizon, for the
		// past-horizon miss propagation on chains) so no further lookups
		// happen.
		return cursorForResolvedPage(1, horizon, resolved), nil, nil
	}
	tokens := make([]string, 0, horizon-1)
	for p := 2; p <= horizon; p++ {
		cursorTok, err := cursorForResolvedPage(p, horizon, resolved).marshal()
		if err != nil {
			return pagedListCursor{}, nil, err
		}
		sibling := &pagination.Bag{}
		sibling.Push(pagination.PageState{
			ResourceTypeID: role,
			Token:          cursorTok,
		})
		tok, err := sibling.Marshal()
		if err != nil {
			return pagedListCursor{}, nil, err
		}
		tokens = append(tokens, tok)
	}
	return cursorForResolvedPage(1, horizon, resolved), tokens, nil
}

func (o *orgResourceType) Grants(
	ctx context.Context,
	resource *v2.Resource,
	opts resourceSdk.SyncOpAttrs,
) ([]*v2.Grant, *resourceSdk.SyncOpResults, error) {
	// The role states carry a JSON pagedListCursor token, so the bag is
	// unmarshaled directly instead of through parsePageToken (which
	// assumes integer tokens).
	bag := &pagination.Bag{}
	if err := bag.Unmarshal(opts.PageToken.Token); err != nil {
		return nil, nil, err
	}
	if bag.Current() == nil {
		bag.Push(pagination.PageState{
			ResourceTypeID: resource.Id.ResourceType,
			ResourceID:     resource.Id.Resource,
		})
	}

	var (
		reqAnnos  annotations.Annotations
		pageToken string
		rv        = []*v2.Grant{}
	)

	switch rId := bag.ResourceTypeID(); rId {
	case resourceTypeOrg.Id:
		bag.Pop()
		bag.Push(pagination.PageState{
			ResourceTypeID: orgRoleAdmin,
		})
		bag.Push(pagination.PageState{
			ResourceTypeID: orgRoleMember,
		})
	case orgRoleAdmin, orgRoleMember:

		orgName, err := o.orgCache.GetOrgName(ctx, opts.Session, resource.Id)
		if err != nil {
			return nil, nil, err
		}
		cursor, err := parsePagedListCursor(bag.PageToken())
		if err != nil {
			return nil, nil, err
		}

		// The collection's first call resolves the stored chain in one
		// batched lookup and fans out: spawn one sibling cursor per stored
		// page (each carrying its validator, so siblings do no lookups),
		// and adopt the horizon so this page's own chain never continues
		// into sibling-owned pages. Spawned and chained cursors carry a
		// resolution/horizon already and never re-spawn.
		var spawnTokens []string
		if cursor.Page == 1 && cursor.Horizon == 0 && cursor.Resolution == "" {
			cursor, spawnTokens, err = o.spawnMemberPageCursors(ctx, orgName, rId, opts)
			if err != nil {
				return nil, nil, err
			}
		}

		pageRes, err := o.listMembersConditional(ctx, orgName, rId, cursor, opts)
		if err != nil {
			var resp *github.Response
			if pageRes != nil {
				resp = pageRes.Resp
			}
			if isNotFoundError(resp) {
				return nil, nil, uhttp.WrapErrors(codes.NotFound, fmt.Sprintf("org: %s not found", orgName))
			}
			return nil, nil, err
		}
		reqAnnos = pageRes.Annos
		if len(spawnTokens) > 0 {
			reqAnnos.Append(v2.SpawnCursors_builder{
				PageTokens: spawnTokens,
			}.Build())
		}

		nextToken := ""
		if pageRes.NextPage != 0 {
			nextCursor := pagedListCursor{Page: pageRes.NextPage, Horizon: cursor.Horizon}
			// Chained pages beyond the stored chain are known misses (the
			// chain is contiguous): a cold page's continuation, and the
			// probe past the horizon. Carrying the miss skips their
			// lookups; if ever wrong it merely forgoes a conditional
			// request and fetches cold — never stale.
			if cursor.Resolution == cursorValidatorMiss ||
				(cursor.Horizon > 0 && pageRes.NextPage > cursor.Horizon) {
				nextCursor.Resolution = cursorValidatorMiss
			}
			nextToken, err = nextCursor.marshal()
			if err != nil {
				return nil, nil, err
			}
		}
		err = bag.Next(nextToken)
		if err != nil {
			return nil, nil, err
		}

		for _, user := range pageRes.Users {
			ur, err := userResource(ctx, user, user.GetEmail(), nil)
			if err != nil {
				return nil, nil, err
			}

			if rId == orgRoleAdmin {
				rv = append(rv, o.orgRoleGrant(orgRoleMember, resource, ur.Id, user.GetID()))
			}
			rv = append(rv, o.orgRoleGrant(rId, resource, ur.Id, user.GetID()))
		}
	default:
		ctxzap.Extract(ctx).Warn("Unknown GitHub Role Name",
			zap.String("role_name", rId),
		)
	}

	pageToken, err := bag.Marshal()
	if err != nil {
		return nil, nil, err
	}
	return rv, &resourceSdk.SyncOpResults{
		NextPageToken: pageToken,
		Annotations:   reqAnnos,
	}, nil
}

func (o *orgResourceType) Grant(ctx context.Context, principal *v2.Resource, en *v2.Entitlement) (annotations.Annotations, error) {
	l := ctxzap.Extract(ctx)

	if principal.Id.ResourceType != resourceTypeUser.Id {
		l.Error(
			"github-connectorv2: only users can be granted org admin",
			zap.String("principal_type", principal.Id.ResourceType),
			zap.String("principal_id", principal.Id.Resource),
		)
		return nil, fmt.Errorf("github-connectorv2: only users can be granted org membership")
	}

	adminRoleID := entitlement.NewEntitlementID(en.Resource, orgRoleAdmin)
	memberRoleID := entitlement.NewEntitlementID(en.Resource, orgRoleMember)

	orgName, err := o.orgCache.GetOrgNameFromRemoteServer(ctx, en.Resource.Id.GetResource())
	if err != nil {
		return nil, err
	}

	principalID, err := strconv.ParseInt(principal.Id.Resource, 10, 64)
	if err != nil {
		return nil, err
	}

	user, resp, err := o.client.Users.GetByID(ctx, principalID)
	if err != nil {
		return nil, wrapGitHubError(err, resp, "github-connector: failed to get user")
	}

	requestedRole := ""
	switch en.Id {
	case adminRoleID:
		requestedRole = orgRoleAdmin
	case memberRoleID:
		requestedRole = orgRoleDirectMember
	default:
		return nil, fmt.Errorf("github-connectorv2: invalid entitlement id: %s", en.Id)
	}

	isMember, resp, err := o.client.Organizations.IsMember(ctx, orgName, user.GetLogin())
	if err != nil {
		return nil, wrapGitHubError(err, resp, "github-connector: failed to check org membership")
	}

	// TODO: check existing invitations. Duplicate invitations aren't allowed, so this will fail with 4xx from github.

	// If user isn't a member, invite them to the org with the requested role
	if !isMember {
		_, resp, err = o.client.Organizations.CreateOrgInvitation(ctx, orgName, &github.CreateOrgInvitationOptions{
			InviteeID: user.ID,
			Role:      &requestedRole,
		})
		if err != nil {
			return nil, wrapGitHubError(err, resp, "github-connector: failed to invite user to org")
		}
		return nil, nil
	}

	if requestedRole == orgRoleDirectMember {
		l.Debug("githubv2-connector: requested org membership but is already a member")
		return nil, nil
	}

	// If the user is a member, check to see what role they have
	membership, resp, err := o.client.Organizations.GetOrgMembership(ctx, user.GetLogin(), orgName)
	if err != nil {
		return nil, wrapGitHubError(err, resp, "github-connector: failed to get org membership")
	}

	// Skip if user already has requested role
	if membership.GetRole() == orgRoleAdmin {
		l.Debug("githubv2-connector: user is already an admin of the org")
		return nil, nil
	}

	// User is a member but grant is for admin, so make them an admin.
	_, resp, err = o.client.Organizations.EditOrgMembership(ctx, user.GetLogin(), orgName, &github.Membership{Role: github.Ptr(orgRoleAdmin)})
	if err != nil {
		return nil, wrapGitHubError(err, resp, "github-connector: failed to make user an admin")
	}

	return nil, nil
}

func (o *orgResourceType) Revoke(ctx context.Context, grant *v2.Grant) (annotations.Annotations, error) {
	l := ctxzap.Extract(ctx)

	en := grant.Entitlement
	principal := grant.Principal

	if principal.Id.ResourceType != resourceTypeUser.Id {
		l.Error(
			"github-connectorv2: org admin can only be revoked from users",
			zap.String("principal_type", principal.Id.ResourceType),
			zap.String("principal_id", principal.Id.Resource),
		)
		return nil, fmt.Errorf("github-connectorv2: org admin can only be revoked from users")
	}

	adminRoleID := entitlement.NewEntitlementID(en.Resource, orgRoleAdmin)
	memberRoleID := entitlement.NewEntitlementID(en.Resource, orgRoleMember)

	if en.Id != adminRoleID && en.Id != memberRoleID {
		return nil, fmt.Errorf("github-connectorv2: invalid entitlement id: %s", en.Id)
	}

	orgName, err := o.orgCache.GetOrgNameFromRemoteServer(ctx, en.Resource.Id.GetResource())
	if err != nil {
		return nil, err
	}

	principalID, err := strconv.ParseInt(principal.Id.Resource, 10, 64)
	if err != nil {
		return nil, err
	}

	user, resp, err := o.client.Users.GetByID(ctx, principalID)
	if err != nil {
		return nil, wrapGitHubError(err, resp, "github-connector: failed to get user")
	}

	membership, resp, err := o.client.Organizations.GetOrgMembership(ctx, user.GetLogin(), orgName)
	if err != nil {
		return nil, wrapGitHubError(err, resp, "github-connector: failed to get org membership")
	}

	if membership.GetState() != "active" {
		return nil, fmt.Errorf("github-connectorv2: user is not an active member of the org")
	}

	if en.Id == memberRoleID {
		resp, err = o.client.Organizations.RemoveOrgMembership(ctx, user.GetLogin(), orgName)
		if err != nil {
			return nil, wrapGitHubError(err, resp, "github-connector: failed to revoke org membership from user")
		}
		return nil, nil
	}

	_, resp, err = o.client.Organizations.EditOrgMembership(ctx, user.GetLogin(), orgName, &github.Membership{Role: github.Ptr(orgRoleMember)})
	if err != nil {
		return nil, wrapGitHubError(err, resp, "github-connector: failed to revoke org admin from user")
	}

	return nil, nil
}

func OrgBuilder(client, appClient *github.Client, orgCache *orgNameCache, orgs []string, syncSecrets bool, authScope string) *orgResourceType {
	orgMap := make(map[string]struct{})

	for _, o := range orgs {
		orgMap[o] = struct{}{}
	}

	return &orgResourceType{
		resourceType: resourceTypeOrg,
		orgs:         orgMap,
		client:       client,
		appClient:    appClient,
		orgCache:     orgCache,
		syncSecrets:  syncSecrets,
		authScope:    authScope,
	}
}

func (o *orgResourceType) listOrganizationsFromAppInstallations(
	ctx context.Context,
	parentResourceID *v2.ResourceId,
) (*v2.Resource, string, annotations.Annotations, error) {
	if len(o.orgs) != 1 {
		return nil, "", nil, fmt.Errorf("github-connector: only one org should be specified")
	}

	var (
		org  *github.Organization
		resp *github.Response
		err  error
	)
	for orgName := range o.orgs {
		org, resp, err = o.client.Organizations.Get(ctx, orgName)
		if err != nil {
			return nil, "", nil, wrapGitHubError(err, resp, "github-connector: failed to fetch organization")
		}
	}

	_, reqAnnos, err := parseResp(resp)
	if err != nil {
		return nil, "", nil, err
	}

	orgResource, err := organizationResource(ctx, org, parentResourceID, o.syncSecrets)
	if err != nil {
		return nil, "", nil, err
	}

	return orgResource, "", reqAnnos, nil
}
