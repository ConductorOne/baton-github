package connector

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"

	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/annotations"
	"github.com/conductorone/baton-sdk/pkg/pagination"
	"github.com/google/go-github/v69/github"
	"github.com/shurcooL/githubv4"
	"golang.org/x/text/cases"
	"golang.org/x/text/language"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func titleCase(s string) string {
	titleCaser := cases.Title(language.English)

	return titleCaser.String(s)
}

type orgNameCache struct {
	sync.RWMutex
	c        *github.Client
	orgNames map[string]string
}

func (o *orgNameCache) GetOrgName(ctx context.Context, orgID *v2.ResourceId) (string, error) {
	o.RLock()
	if orgName, ok := o.orgNames[orgID.Resource]; ok {
		o.RUnlock()
		return orgName, nil
	}
	o.RUnlock()

	o.Lock()
	defer o.Unlock()

	if orgName, ok := o.orgNames[orgID.Resource]; ok {
		return orgName, nil
	}

	oID, err := strconv.ParseInt(orgID.Resource, 10, 64)
	if err != nil {
		return "", err
	}

	org, _, err := o.c.Organizations.GetByID(ctx, oID)
	if err != nil {
		return "", err
	}

	o.orgNames[orgID.Resource] = org.GetLogin()

	return org.GetLogin(), nil
}

func newOrgNameCache(c *github.Client) *orgNameCache {
	return &orgNameCache{
		c:        c,
		orgNames: make(map[string]string),
	}
}

type userDataCache struct {
	sync.RWMutex
	c              *github.Client
	usersById      map[int64]*github.User
	usersByLogin   map[string]*github.User
}

func (u *userDataCache) GetUser(ctx context.Context, userID int64) (*github.User, *github.Response, error) {
	u.RLock()
	if user, ok := u.usersById[userID]; ok {
		u.RUnlock()
		return user, nil, nil
	}
	u.RUnlock()

	u.Lock()
	defer u.Unlock()

	if user, ok := u.usersById[userID]; ok {
		return user, nil, nil
	}

	user, resp, err := u.c.Users.GetByID(ctx, userID)
	if err != nil {
		return nil, resp, err
	}

	u.usersById[userID] = user
	if user.Login != nil {
		u.usersByLogin[*user.Login] = user
	}

	return user, resp, nil
}

func (u *userDataCache) GetUserByLogin(ctx context.Context, login string) (*github.User, *github.Response, error) {
	u.RLock()
	if user, ok := u.usersByLogin[login]; ok {
		u.RUnlock()
		return user, nil, nil
	}
	u.RUnlock()

	u.Lock()
	defer u.Unlock()

	if user, ok := u.usersByLogin[login]; ok {
		return user, nil, nil
	}

	user, resp, err := u.c.Users.Get(ctx, login)
	if err != nil {
		return nil, resp, err
	}

	u.usersByLogin[login] = user
	if user.ID != nil {
		u.usersById[*user.ID] = user
	}

	return user, resp, nil
}

func newUserDataCache(c *github.Client) *userDataCache {
	return &userDataCache{
		c:            c,
		usersById:    make(map[int64]*github.User),
		usersByLogin: make(map[string]*github.User),
	}
}

type teamDataCache struct {
	sync.RWMutex
	c     *github.Client
	teams map[int64]*github.Team
}

func (t *teamDataCache) GetTeam(ctx context.Context, orgID int64, teamID int64) (*github.Team, *github.Response, error) {
	t.RLock()
	if team, ok := t.teams[teamID]; ok {
		t.RUnlock()
		return team, nil, nil
	}
	t.RUnlock()

	t.Lock()
	defer t.Unlock()

	if team, ok := t.teams[teamID]; ok {
		return team, nil, nil
	}

	team, resp, err := t.c.Teams.GetTeamByID(ctx, orgID, teamID)
	if err != nil {
		return nil, resp, err
	}

	t.teams[teamID] = team

	return team, resp, nil
}

func newTeamDataCache(c *github.Client) *teamDataCache {
	return &teamDataCache{
		c:     c,
		teams: make(map[int64]*github.Team),
	}
}

func v1AnnotationsForResourceType(resourceTypeID string) annotations.Annotations {
	annos := annotations.Annotations{}
	annos.Update(&v2.V1Identifier{
		Id: resourceTypeID,
	})

	return annos
}

// parseResourceToGitHub returns the upstream API ID by looking at the last 'part' of the resource ID.
func parseResourceToGitHub(id *v2.ResourceId) (int64, error) {
	idParts := strings.Split(id.Resource, ":")

	return strconv.ParseInt(idParts[len(idParts)-1], 10, 64)
}

func parsePageToken(i string, resourceID *v2.ResourceId) (*pagination.Bag, int, error) {
	b := &pagination.Bag{}
	err := b.Unmarshal(i)
	if err != nil {
		return nil, 0, err
	}

	if b.Current() == nil {
		b.Push(pagination.PageState{
			ResourceTypeID: resourceID.ResourceType,
			ResourceID:     resourceID.Resource,
		})
	}

	page, err := convertPageToken(b.PageToken())
	if err != nil {
		return nil, 0, err
	}

	return b, page, nil
}

// convertPageToken converts a string token into an int.
func convertPageToken(token string) (int, error) {
	if token == "" {
		return 0, nil
	}
	return strconv.Atoi(token)
}

// fmtGitHubPageToken return a formatted string for a github page token.
func fmtGitHubPageToken(pageToken int) string {
	if pageToken == 0 {
		return ""
	}
	return strconv.FormatInt(int64(pageToken), 10)
}

func parseResp(resp *github.Response) (string, annotations.Annotations, error) {
	var annos annotations.Annotations
	var nextPage string

	if resp != nil {
		if desc, err := extractRateLimitData(resp); err == nil {
			annos.WithRateLimiting(desc)
		}
		nextPage = fmtGitHubPageToken(resp.NextPage)
	}

	return nextPage, annos, nil
}

// extractRateLimitData returns a set of annotations for rate limiting given the rate limit headers provided by GitHub.
func extractRateLimitData(response *github.Response) (*v2.RateLimitDescription, error) {
	if response == nil {
		return nil, fmt.Errorf("github-connector: passed nil response")
	}
	var err error

	var r int64
	remaining := response.Header.Get("X-Ratelimit-Remaining")
	if remaining != "" {
		r, err = strconv.ParseInt(remaining, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("failed to parse ratelimit-remaining: %w", err)
		}
	}

	var l int64
	limit := response.Header.Get("X-Ratelimit-Limit")
	if limit != "" {
		l, err = strconv.ParseInt(limit, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("failed to parse ratelimit-limit: %w", err)
		}
	}

	var ra *timestamppb.Timestamp
	resetAt := response.Header.Get("X-Ratelimit-Reset")
	if resetAt != "" {
		ts, err := strconv.ParseInt(resetAt, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("failed to parse ratelimit-reset: %w", err)
		}
		ra = &timestamppb.Timestamp{Seconds: ts}
	}

	status := v2.RateLimitDescription_STATUS_OK
	if r <= 0 {
		status = v2.RateLimitDescription_STATUS_OVERLIMIT
	}
	return &v2.RateLimitDescription{
		Status:    status,
		Limit:     l,
		Remaining: r,
		ResetAt:   ra,
	}, nil
}

type listUsersQuery struct {
	Organization struct {
		SamlIdentityProvider struct {
			SsoUrl             githubv4.String
			ExternalIdentities struct {
				Edges []struct {
					Node struct {
						SamlIdentity struct {
							NameId string
							Emails []struct {
								Value string
							}
						}
						User struct {
							Login string
						}
					}
				}
			} `graphql:"externalIdentities(first: 1, login: $userName)"`
		}
	} `graphql:"organization(login: $orgLoginName)"`
	RateLimit struct {
		Limit     int
		Cost      int
		Remaining int
		ResetAt   githubv4.DateTime
	}
}

type hasSAMLQuery struct {
	Organization struct {
		SamlIdentityProvider struct {
			Id string
		}
	} `graphql:"organization(login: $orgLoginName)"`
}

func isNotFoundError(resp *github.Response) bool {
	if resp == nil {
		return false
	}
	return resp.StatusCode == http.StatusNotFound
}

func isRatelimited(resp *github.Response) bool {
	if resp == nil {
		return false
	}
	if resp.StatusCode == http.StatusForbidden && resp.Header.Get("X-Ratelimit-Remaining") == "0" {
		return true
	}
	return resp.StatusCode == http.StatusTooManyRequests
}

func isAuthError(resp *github.Response) bool {
	if resp == nil {
		return false
	}
	return resp.StatusCode == http.StatusUnauthorized
}

func isPermissionError(resp *github.Response) bool {
	if resp == nil {
		return false
	}
	return resp.StatusCode == http.StatusForbidden
}
