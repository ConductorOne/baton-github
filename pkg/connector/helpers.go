package connector

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/annotations"
	"github.com/conductorone/baton-sdk/pkg/pagination"
	"github.com/conductorone/baton-sdk/pkg/session"
	"github.com/conductorone/baton-sdk/pkg/types/sessions"
	"github.com/conductorone/baton-sdk/pkg/uhttp"
	"github.com/google/go-github/v69/github"
	"github.com/shurcooL/githubv4"
	"golang.org/x/text/cases"
	"golang.org/x/text/language"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func titleCase(s string) string {
	titleCaser := cases.Title(language.English)

	return titleCaser.String(s)
}

type orgNameCache struct {
	c *github.Client
}

func (o *orgNameCache) GetOrgName(ctx context.Context, ss sessions.SessionStore, orgID *v2.ResourceId) (string, error) {
	orgName, found, err := session.GetJSON[string](ctx, ss, orgID.Resource)
	if err != nil {
		return "", err
	}

	if found {
		return orgName, nil
	}

	login, err := o.GetOrgNameFromRemoteServer(ctx, orgID.Resource)
	if err != nil {
		return "", err
	}

	err = session.SetJSON(ctx, ss, orgID.Resource, login)
	if err != nil {
		return "", err
	}

	return login, nil
}

func (o *orgNameCache) GetOrgNameFromRemoteServer(ctx context.Context, rID string) (string, error) {
	oID, err := strconv.ParseInt(rID, 10, 64)
	if err != nil {
		return "", err
	}

	org, _, err := o.c.Organizations.GetByID(ctx, oID)
	if err != nil {
		return "", err
	}
	return org.GetLogin(), nil
}

func newOrgNameCache(c *github.Client) *orgNameCache {
	return &orgNameCache{
		c: c,
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

// rateLimitDescriptionFromRate creates a RateLimitDescription from a github.Rate struct.
// This is used when go-github returns a RateLimitError with rate info but a synthetic response.
func rateLimitDescriptionFromRate(rate github.Rate) *v2.RateLimitDescription {
	desc := &v2.RateLimitDescription{
		Status:    v2.RateLimitDescription_STATUS_OVERLIMIT,
		Limit:     int64(rate.Limit),
		Remaining: int64(rate.Remaining),
	}
	if !rate.Reset.IsZero() {
		desc.ResetAt = timestamppb.New(rate.Reset.Time)
	}
	return desc
}

// rateLimitDescriptionFromRetryAfter creates a RateLimitDescription from a retry-after duration.
// This is used for AbuseRateLimitError which provides a RetryAfter duration.
func rateLimitDescriptionFromRetryAfter(retryAfter *time.Duration) *v2.RateLimitDescription {
	desc := &v2.RateLimitDescription{
		Status: v2.RateLimitDescription_STATUS_OVERLIMIT,
	}
	if retryAfter != nil {
		desc.ResetAt = timestamppb.New(time.Now().Add(*retryAfter))
	}
	return desc
}

// wrapErrorWithRateLimitDetails creates a gRPC error with rate limit details attached.
func wrapErrorWithRateLimitDetails(code codes.Code, msg string, rlDesc *v2.RateLimitDescription, err error) error {
	st := status.New(code, msg)
	if rlDesc != nil {
		st, _ = st.WithDetails(rlDesc)
	}
	return errors.Join(st.Err(), err)
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

func isTemporarilyUnavailable(resp *github.Response) bool {
	if resp == nil {
		return false
	}
	return resp.StatusCode == http.StatusServiceUnavailable ||
		resp.StatusCode == http.StatusBadGateway ||
		resp.StatusCode == http.StatusGatewayTimeout
}

// wrapGitHubError wraps GitHub API errors with appropriate gRPC status codes based on the HTTP response.
// It handles rate limiting, authentication errors, permission errors, and generic errors.
// The contextMsg parameter should describe the operation that failed (e.g., "failed to list teams").
func wrapGitHubError(err error, resp *github.Response, contextMsg string) error {
	if err == nil {
		return nil
	}

	// Check for go-github rate limit error types FIRST.
	// These may have synthetic responses with empty headers when the client
	// blocks requests without making an actual HTTP call.
	var rateLimitErr *github.RateLimitError
	if errors.As(err, &rateLimitErr) {
		rlDesc := rateLimitDescriptionFromRate(rateLimitErr.Rate)
		return wrapErrorWithRateLimitDetails(codes.Unavailable, "rate limit exceeded", rlDesc, err)
	}

	var abuseRateLimitErr *github.AbuseRateLimitError
	if errors.As(err, &abuseRateLimitErr) {
		rlDesc := rateLimitDescriptionFromRetryAfter(abuseRateLimitErr.RetryAfter)
		return wrapErrorWithRateLimitDetails(codes.Unavailable, "secondary rate limit exceeded", rlDesc, err)
	}

	// Check response-based rate limiting (real 429 or 403 with header)
	if isRatelimited(resp) {
		return uhttp.WrapErrors(codes.Unavailable, "too many requests", err)
	}

	// Check for temporary server errors (503, 502, 504)
	if isTemporarilyUnavailable(resp) {
		return uhttp.WrapErrors(codes.Unavailable, "service temporarily unavailable", err)
	}

	if isAuthError(resp) {
		return uhttp.WrapErrors(codes.Unauthenticated, contextMsg, err)
	}
	if isPermissionError(resp) {
		return uhttp.WrapErrors(codes.PermissionDenied, contextMsg, err)
	}
	return fmt.Errorf("%s: %w", contextMsg, err)
}
