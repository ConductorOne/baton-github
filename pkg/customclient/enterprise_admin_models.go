package customclient

import (
	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/annotations"
	"github.com/shurcooL/githubv4"
	"google.golang.org/protobuf/types/known/timestamppb"
)

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

// EnterpriseUser is a user account as the enterprise reports it.
type EnterpriseUser struct {
	DatabaseID int64
	Login      string
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

type graphQLRateLimit struct {
	Limit     githubv4.Int
	Remaining githubv4.Int
	ResetAt   githubv4.DateTime
}

// EnterpriseOwnerState is a user's Owner role and pending Owner invitation, if any.
type EnterpriseOwnerState struct {
	EnterpriseID        string
	IsOwner             bool
	PendingInvitationID string
}

// HoldsRole reports whether C1 should see a grant. C1 has no pending state, so an
// unaccepted invitation counts.
func (s EnterpriseOwnerState) HoldsRole() bool {
	return s.IsOwner || s.PendingInvitationID != ""
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
