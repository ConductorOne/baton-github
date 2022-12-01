package connector

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/annotations"
	"github.com/conductorone/baton-sdk/pkg/pagination"
	"github.com/google/go-github/v41/github"
	"golang.org/x/text/cases"
	"golang.org/x/text/language"
	"google.golang.org/protobuf/types/known/timestamppb"
)

var titleCaser = cases.Title(language.English)

func getOrgName(ctx context.Context, c *github.Client, orgID *v2.ResourceId) (string, error) {
	oID, err := strconv.ParseInt(orgID.Resource, 10, 64)
	if err != nil {
		return "", err
	}

	org, _, err := c.Organizations.GetByID(ctx, oID)
	if err != nil {
		return "", err
	}

	return org.GetLogin(), nil
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
