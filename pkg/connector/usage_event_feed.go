package connector

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/annotations"
	"github.com/conductorone/baton-sdk/pkg/pagination"
	"github.com/google/go-github/v69/github"
	"go.uber.org/zap"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// defaultActivityLookback bounds the very first poll when no earliest-event
// boundary is given yet; later polls advance via the feed's own cursor.
const defaultActivityLookback = 1 * time.Hour

// maxAuditLogPagesPerCall caps total pages walked across all orgs per call
// so one very active org can't stall the feed; remaining pages resume via the cursor.
const maxAuditLogPagesPerCall = 20

// usageEventFeed streams member activity from each org's audit log as usage
// events, since GitHub has no per-user "last activity" field to sync directly.
type usageEventFeed struct {
	client *github.Client
	orgs   []string

	// used to handle log Warn prints for skipped orgs.
	skippedOrgs sampledWarn
}

func newUsageEventFeed(client *github.Client, orgs []string) *usageEventFeed {
	return &usageEventFeed{client: client, orgs: orgs}
}

func (f *usageEventFeed) EventFeedMetadata(_ context.Context) *v2.EventFeedMetadata {
	return &v2.EventFeedMetadata{
		Id:                  "github_usage_event_feed",
		SupportedEventTypes: []v2.EventType{v2.EventType_EVENT_TYPE_USAGE},
	}
}

// usageEventPageToken tracks progress through one pass over every configured
// org's audit log, walked newest-first until an already-seen entry (at or
// before Since) is reached.
type usageEventPageToken struct {
	Orgs     []string `json:"orgs,omitempty"`
	OrgIndex int      `json:"org_index"`
	// AuditLogCursor is the opaque value to resume from. Whether it goes into
	// the request's After or Page field depends on AuditLogCursorIsPage - see
	// nextAuditLogPage.
	AuditLogCursor       string `json:"audit_log_cursor,omitempty"`
	AuditLogCursorIsPage bool   `json:"audit_log_cursor_is_page,omitempty"`
	Since                string `json:"since,omitempty"`
}

func unmarshalUsageEventPageToken(pToken *pagination.StreamToken) (*usageEventPageToken, error) {
	pt := &usageEventPageToken{}
	if pToken == nil || pToken.Cursor == "" {
		return pt, nil
	}
	data, err := base64.StdEncoding.DecodeString(pToken.Cursor)
	if err != nil {
		return nil, fmt.Errorf("baton-github: failed to decode usage event feed cursor: %w", err)
	}
	if err := json.Unmarshal(data, pt); err != nil {
		return nil, fmt.Errorf("baton-github: failed to unmarshal usage event feed cursor: %w", err)
	}
	return pt, nil
}

func (pt *usageEventPageToken) marshal() (string, error) {
	data, err := json.Marshal(pt)
	if err != nil {
		return "", fmt.Errorf("baton-github: failed to marshal usage event feed cursor: %w", err)
	}
	return base64.StdEncoding.EncodeToString(data), nil
}

func (f *usageEventFeed) ListEvents(
	ctx context.Context,
	earliestEvent *timestamppb.Timestamp,
	pToken *pagination.StreamToken,
) ([]*v2.Event, *pagination.StreamState, annotations.Annotations, error) {
	if f.client == nil {
		return nil, &pagination.StreamState{HasMore: false}, nil, nil
	}

	cursor, err := unmarshalUsageEventPageToken(pToken)
	if err != nil {
		return nil, nil, nil, err
	}

	if len(cursor.Orgs) == 0 {
		// Snapshot the org list and "since" boundary once per pass, so
		// mid-pass config changes don't shift what gets walked.
		orgs, err := getOrgs(ctx, f.client, f.orgs)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("baton-github: failed to list orgs for usage event feed: %w", err)
		}
		if len(orgs) == 0 {
			return nil, &pagination.StreamState{HasMore: false}, nil, nil
		}

		since := time.Now().Add(-defaultActivityLookback)
		// Guard against a zero/degenerate earliestEvent producing a
		// nonsensical "since year 1" query that GitHub's search parser rejects.
		if earliestEvent != nil {
			if t := earliestEvent.AsTime(); !t.IsZero() && t.After(time.Unix(0, 0)) {
				since = t
			}
		}

		cursor = &usageEventPageToken{
			Orgs:  orgs,
			Since: since.Format(time.RFC3339Nano),
		}
	}

	if cursor.OrgIndex < 0 || cursor.OrgIndex >= len(cursor.Orgs) {
		cursor.OrgIndex = 0
		cursor.AuditLogCursor = ""
		cursor.AuditLogCursorIsPage = false
	}

	since, err := time.Parse(time.RFC3339Nano, cursor.Since)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("baton-github: invalid usage event feed cursor timestamp: %w", err)
	}
	// created:>=<since> is sent server-side so GitHub excludes already-seen
	// entries; the check below stays as a safety net in case it's ignored.
	sincePhrase := "created:>=" + since.UTC().Format("2006-01-02T15:04:05-07:00")

	var events []*v2.Event
	// Tightest (lowest Remaining) rate limit seen across this call's requests.
	var tightestRateLimit *v2.RateLimitDescription

	// TODO(jdc): Probably change this for loop for a series of requests that uses a more complex pagination cursor.
	for page := 0; page < maxAuditLogPagesPerCall; page++ {
		orgName := cursor.Orgs[cursor.OrgIndex]

		opts := &github.GetAuditLogOptions{
			Order: github.Ptr("desc"),
			// "web" excludes raw git-protocol events (push/fetch/clone),
			// which dominate audit-log volume without losing members who are
			// otherwise covered by their web/API activity.
			Include: github.Ptr("web"),
			Phrase:  github.Ptr(sincePhrase),
			ListCursorOptions: github.ListCursorOptions{
				PerPage: maxPageSize,
			},
		}
		if cursor.AuditLogCursorIsPage {
			opts.Page = cursor.AuditLogCursor
		} else {
			opts.After = cursor.AuditLogCursor
		}

		entries, resp, err := f.client.Organizations.GetAuditLog(ctx, orgName, opts)
		// Read rate-limit headers before the error branch nils resp, since a
		// 429 still carries them.
		if resp != nil {
			if rl, rlErr := extractRateLimitData(resp); rlErr == nil {
				if tightestRateLimit == nil || rl.GetRemaining() < tightestRateLimit.GetRemaining() {
					tightestRateLimit = rl
				}
			}
		}
		if err != nil {
			// Skip-and-continue only for permanent per-org conditions (no
			// audit-log access); anything else aborts instead of wasting the
			// rest of the page budget. Rate-limit checks come first since
			// GitHub can signal rate limiting via a 403.
			var rateLimitErr *github.RateLimitError
			var abuseRateLimitErr *github.AbuseRateLimitError
			retryable := errors.As(err, &rateLimitErr) || errors.As(err, &abuseRateLimitErr) ||
				isRatelimited(resp) || isTemporarilyUnavailable(resp)

			switch {
			case retryable:
				return nil, nil, nil, wrapGitHubError(err, resp,
					fmt.Sprintf("baton-github: failed to fetch audit log for org %s", orgName))
			case isNotFoundError(resp) || isPermissionError(resp):
				f.skippedOrgs.log(ctx, "org lacks audit-log access, skipping it for this pass",
					zap.String("org", orgName), zap.Error(err),
				)

				entries, resp = nil, nil
			default:
				return nil, nil, nil, wrapGitHubError(err, resp,
					fmt.Sprintf("baton-github: failed to fetch audit log for org %s", orgName))
			}
		}

		reachedBoundary := false
		for _, entry := range entries {
			// Check every entry's timestamp, even filtered ones, so an all-bot page still stops pagination.
			if ts := entry.GetTimestamp().Time; !ts.IsZero() && !ts.After(since) {
				reachedBoundary = true
				break
			}

			evt, ok := usageEventFromAuditEntry(entry)
			if !ok {
				continue
			}
			events = append(events, evt)
		}

		if resp != nil && !reachedBoundary {
			if nextPage, isPage := nextAuditLogPage(resp); nextPage != "" {
				cursor.AuditLogCursor = nextPage
				cursor.AuditLogCursorIsPage = isPage
				continue
			}
		}

		// Done with this org for this pass - advance to the next one.
		cursor.OrgIndex++
		cursor.AuditLogCursor = ""
		cursor.AuditLogCursorIsPage = false
		if cursor.OrgIndex >= len(cursor.Orgs) {
			// Pass complete - the next call gets a fresh earliestEvent, so
			// nothing needs to survive in the cursor.
			tokenStr, err := (&usageEventPageToken{}).marshal()
			if err != nil {
				return nil, nil, nil, err
			}
			var annos annotations.Annotations
			if tightestRateLimit != nil {
				annos.WithRateLimiting(tightestRateLimit)
			}
			return events, &pagination.StreamState{Cursor: tokenStr, HasMore: false}, annos, nil
		}
	}

	tokenStr, err := cursor.marshal()
	if err != nil {
		return nil, nil, nil, err
	}
	var annos annotations.Annotations
	if tightestRateLimit != nil {
		annos.WithRateLimiting(tightestRateLimit)
	}
	return events, &pagination.StreamState{Cursor: tokenStr, HasMore: true}, annos, nil
}

// nextAuditLogPage returns the cursor to request the next audit-log page
// (empty if there isn't one), and whether that cursor belongs in the
// request's Page field (true) or its After field (false).
//
// The org audit-log endpoint documents three pagination shapes depending on
// what the server returns in the Link header's rel="next" entry:
//   - after=<cursor> (GHEC and GHES): the documented, primary mechanism -
//     go-github parses this into Response.After. Checked first since it's
//     what both github.com and GHES actually return in practice.
//   - page=<opaque token> (legacy fallback some GHES versions may still
//     emit): go-github can't parse a non-numeric page value as an int, so it
//     lands in Response.NextPageToken instead.
//   - page=<N> (numeric, classic GHES offset pagination): parses into
//     Response.NextPage.
//
// Checking only NextPageToken/NextPage (as earlier code did) misses the
// after= case entirely, silently truncating every GHEC org - and most GHES
// orgs - to a single page.
func nextAuditLogPage(resp *github.Response) (string, bool) {
	if resp.After != "" {
		return resp.After, false
	}
	if resp.NextPageToken != "" {
		return resp.NextPageToken, true
	}
	if resp.NextPage != 0 {
		return strconv.Itoa(resp.NextPage), true
	}
	return "", false
}

// usageEventFromAuditEntry converts one audit-log entry into a usage event
// targeting the usage-app resource (see usage_app.go). Returns ok=false when
// the entry can't be attributed to a synced user.
func usageEventFromAuditEntry(entry *github.AuditEntry) (*v2.Event, bool) {
	actor := entry.GetActor()
	actorID := entry.GetActorID()
	ts := entry.GetTimestamp().Time
	if actorID == 0 || ts.IsZero() {
		return nil, false
	}

	// actor_is_bot is real but undocumented (only in AdditionalFields); trust
	// it when present, else fall back to the "[bot]" login suffix. Bots
	// aren't synced as users, so their events wouldn't correlate to anything.
	if isBot, ok := entry.AdditionalFields["actor_is_bot"].(bool); ok {
		if isBot {
			return nil, false
		}
	} else if strings.HasSuffix(actor, "[bot]") {
		return nil, false
	}

	orgID := entry.GetOrgID()

	id := entry.GetDocumentID()
	if id == "" {
		// No stable ID from GitHub - synthesize one so dedup doesn't collapse
		// every entry missing _document_id into one event.
		id = fmt.Sprintf("%d:%d:%d:%s", orgID, actorID, ts.UnixNano(), entry.GetAction())
	}

	return &v2.Event{
		Id:         id,
		OccurredAt: timestamppb.New(ts),
		Event: &v2.Event_UsageEvent{
			UsageEvent: &v2.UsageEvent{
				TargetResource: &v2.Resource{
					Id: &v2.ResourceId{
						ResourceType: resourceTypeUsageApp.Id,
						Resource:     usageAppResourceID,
					},
					DisplayName: usageAppDisplayName,
				},
				ActorResource: &v2.Resource{
					Id: &v2.ResourceId{
						ResourceType: resourceTypeUser.Id,
						Resource:     strconv.FormatInt(actorID, 10),
					},
					DisplayName: actor,
				},
			},
		},
	}, true
}
