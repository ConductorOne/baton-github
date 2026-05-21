package connector

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-github/v69/github"
	"github.com/stretchr/testify/require"
)

type fakeTimeoutErr struct{}

func (fakeTimeoutErr) Error() string { return "i/o timeout" }
func (fakeTimeoutErr) Timeout() bool { return true }

// Temporary satisfies net.Error; deprecated and unused by isRetryableHTTP2Error.
func (fakeTimeoutErr) Temporary() bool { return false }

const (
	hdrTimeoutMsg = "http2: timeout awaiting response headers"
	goawayInner   = "http2: Transport received Server's graceful shutdown GOAWAY"
	goawayWrapped = "http2: Transport: cannot retry err [http2: Transport received Server's graceful shutdown GOAWAY] after Request.Body was written; define Request.GetBody to avoid this error"
)

func TestIsRetryableHTTP2Error(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"unrelated", errors.New("boom"), false},

		// Positive: bare error strings from x/net/http2.
		{"response-header timeout", errors.New(hdrTimeoutMsg), true},
		{"goaway raw inner string", errors.New(goawayInner), true},
		{"goaway wrapped no-GetBody", errors.New(goawayWrapped), true},

		// Positive: net.Error.Timeout().
		{"net.Error timeout", fakeTimeoutErr{}, true},

		// Positive: errors wrapped via %w — the substring check still matches
		// because err.Error() walks the wrap chain.
		{"wrapped response-header timeout", fmt.Errorf("ListCollaborators: %w", errors.New(hdrTimeoutMsg)), true},
		{"wrapped net.Error timeout", fmt.Errorf("dial: %w", fakeTimeoutErr{}), true},

		// Negative: context errors must short-circuit before the net.Error
		// check (context.DeadlineExceeded satisfies net.Error.Timeout()=true).
		{"context.DeadlineExceeded short-circuited", context.DeadlineExceeded, false},
		{"context.Canceled short-circuited", context.Canceled, false},
		{"wrapped context.Canceled", fmt.Errorf("op cancelled: %w", context.Canceled), false},

		// Negative: near-miss substrings that must not over-retry.
		{"truncated hdr-timeout substring", errors.New("http2: timeout"), false},
		{"generic http2 stream error", errors.New("http2: stream error"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			require.Equal(t, c.want, isRetryableHTTP2Error(c.err),
				"isRetryableHTTP2Error(%v) classification", c.err)
		})
	}
}

// TestListCollaboratorsBackoffsBudget enforces both the 3-attempt invariant
// and a total-budget ceiling so a future tuning regression that, e.g., bumps
// the last slot to 30s will fail loudly here.
func TestListCollaboratorsBackoffsBudget(t *testing.T) {
	require.Len(t, listCollaboratorsBackoffs, 3, "three-attempt invariant")
	require.Less(t, listCollaboratorsBackoffs[0], time.Second, "first delay must be sub-second")

	var total time.Duration
	prev := time.Duration(0)
	for i, d := range listCollaboratorsBackoffs {
		require.Greater(t, d, prev, "backoff[%d] must be strictly increasing", i)
		prev = d
		total += d
	}
	require.LessOrEqual(t, total, 10*time.Second, "total backoff budget exceeded")
}

// scriptedRT returns a scripted error or response per RoundTrip call. It panics
// the test on extra calls so over-retry is caught loudly.
type scriptedRT struct {
	mu    sync.Mutex
	t     *testing.T
	calls int
	steps []scriptedStep
}

type scriptedStep struct {
	err  error          // if non-nil, returned as the RoundTrip error
	resp *http.Response // otherwise this response is returned
}

func (r *scriptedRT) RoundTrip(req *http.Request) (*http.Response, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	require.Less(r.t, r.calls, len(r.steps), "unexpected extra RoundTrip call (call #%d)", r.calls+1)
	step := r.steps[r.calls]
	r.calls++
	if step.err != nil {
		return nil, step.err
	}
	return step.resp, nil
}

func (r *scriptedRT) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

func okEmptyResponse(req *http.Request) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(`[]`)),
		Header:     make(http.Header),
		Request:    req,
	}
}

// installImmediateSleep swaps sleepFn so retry backoffs don't burn wall time.
// Not safe under t.Parallel; callers must avoid it.
func installImmediateSleep(t *testing.T) {
	t.Helper()
	orig := sleepFn
	sleepFn = func(time.Duration) <-chan time.Time {
		ch := make(chan time.Time, 1)
		ch <- time.Now()
		return ch
	}
	t.Cleanup(func() { sleepFn = orig })
}

func newScriptedClient(t *testing.T, steps ...scriptedStep) (*github.Client, *scriptedRT) {
	t.Helper()
	rt := &scriptedRT{t: t, steps: steps}
	return github.NewClient(&http.Client{Transport: rt}), rt
}

func TestListCollaboratorsWithRetry_SuccessFirstAttempt(t *testing.T) {
	installImmediateSleep(t)
	client, rt := newScriptedClient(t, scriptedStep{resp: okEmptyResponse(&http.Request{})})

	_, _, err := listCollaboratorsWithRetry(t.Context(), client, "org", "repo", &github.ListCollaboratorsOptions{})
	require.NoError(t, err)
	require.Equal(t, 1, rt.callCount(), "no retry on success")
}

func TestListCollaboratorsWithRetry_RetriesThenSucceeds(t *testing.T) {
	installImmediateSleep(t)
	client, rt := newScriptedClient(t,
		scriptedStep{err: errors.New(hdrTimeoutMsg)},
		scriptedStep{resp: okEmptyResponse(&http.Request{})},
	)

	_, _, err := listCollaboratorsWithRetry(t.Context(), client, "org", "repo", &github.ListCollaboratorsOptions{})
	require.NoError(t, err)
	require.Equal(t, 2, rt.callCount(), "exactly one retry before success")
}

func TestListCollaboratorsWithRetry_ExhaustsAllAttempts(t *testing.T) {
	installImmediateSleep(t)
	client, rt := newScriptedClient(t,
		scriptedStep{err: errors.New(hdrTimeoutMsg)},
		scriptedStep{err: errors.New(hdrTimeoutMsg)},
		scriptedStep{err: errors.New(goawayWrapped)},
	)

	_, _, err := listCollaboratorsWithRetry(t.Context(), client, "org", "repo", &github.ListCollaboratorsOptions{})
	require.Error(t, err)
	require.Equal(t, 3, rt.callCount(), "exactly three attempts on full failure")
	require.Contains(t, err.Error(), "GOAWAY", "final attempt's error is surfaced")
}

func TestListCollaboratorsWithRetry_NonRetryableShortCircuits(t *testing.T) {
	installImmediateSleep(t)
	// A non-classified error must abort after the first attempt.
	client, rt := newScriptedClient(t, scriptedStep{err: errors.New("some random non-retryable error")})

	_, _, err := listCollaboratorsWithRetry(t.Context(), client, "org", "repo", &github.ListCollaboratorsOptions{})
	require.Error(t, err)
	require.Equal(t, 1, rt.callCount(), "non-retryable error must not retry")
}

func TestListCollaboratorsWithRetry_ContextCancelDuringBackoff(t *testing.T) {
	// Replace sleepFn with one that blocks until we explicitly release it,
	// so we can cancel the ctx mid-backoff and assert the select branch.
	orig := sleepFn
	sleepStarted := make(chan struct{}, len(listCollaboratorsBackoffs))
	hold := make(chan time.Time) // never sent on
	sleepFn = func(time.Duration) <-chan time.Time {
		sleepStarted <- struct{}{}
		return hold
	}
	t.Cleanup(func() { sleepFn = orig })

	client, rt := newScriptedClient(t,
		scriptedStep{err: errors.New(hdrTimeoutMsg)},
		// Second step is unreachable because ctx will be cancelled in backoff.
		scriptedStep{err: errors.New("UNREACHABLE")},
	)
	ctx, cancel := context.WithCancel(context.Background())

	errCh := make(chan error, 1)
	go func() {
		_, _, err := listCollaboratorsWithRetry(ctx, client, "org", "repo", &github.ListCollaboratorsOptions{})
		errCh <- err
	}()

	select {
	case <-sleepStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("retry never entered backoff")
	}
	cancel()

	select {
	case err := <-errCh:
		require.Error(t, err)
		require.ErrorIs(t, err, context.Canceled, "ctx.Err() must be the primary signal")
		require.Contains(t, err.Error(), hdrTimeoutMsg, "underlying transient err preserved in joined error")
	case <-time.After(2 * time.Second):
		t.Fatal("listCollaboratorsWithRetry did not return after ctx cancel")
	}
	require.Equal(t, 1, rt.callCount(), "must not start the second attempt after ctx cancel")
}
