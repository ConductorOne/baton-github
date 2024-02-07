package connector

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"

	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/annotations"
	"github.com/conductorone/baton-sdk/pkg/pagination"
	"github.com/google/go-github/v41/github"
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

func v1AnnotationsForResourceType(resourceTypeID string) annotations.Annotations {
	annos := annotations.Annotations{}
	annos.Update(&v2.V1Identifier{
		Id: resourceTypeID,
	})

	return annos
}

// parseResourceToGithub returns the upstream API ID by looking at the last 'part' of the resource ID.
func parseResourceToGithub(id *v2.ResourceId) (int64, error) {
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

// fmtGithubPageToken return a formatted string for a github page token.
func fmtGithubPageToken(pageToken int) string {
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
		nextPage = fmtGithubPageToken(resp.NextPage)
	}

	return nextPage, annos, nil
}

// extractRateLimitData returns a set of annotations for rate limiting given the rate limit headers provided by Github.
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

	return &v2.RateLimitDescription{
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
