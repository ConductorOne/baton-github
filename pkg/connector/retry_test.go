package connector

import (
	"context"
	"errors"
	"testing"
	"time"
)

type fakeTimeoutErr struct{}

func (fakeTimeoutErr) Error() string   { return "i/o timeout" }
func (fakeTimeoutErr) Timeout() bool   { return true }
func (fakeTimeoutErr) Temporary() bool { return true }

func TestIsRetryableHTTP2Error(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"unrelated", errors.New("boom"), false},
		{"response-header timeout", errors.New("http2: timeout awaiting response headers"), true},
		{"goaway no GetBody", errors.New("http2: Transport: cannot retry err [http2: Transport received Server's graceful shutdown GOAWAY] after Request.Body was written; define Request.GetBody to avoid this error"), true},
		{"net.Error timeout", fakeTimeoutErr{}, true},
		{"context deadline exceeded (not wrapped as net.Error)", context.DeadlineExceeded, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := isRetryableHTTP2Error(c.err)
			if got != c.want {
				t.Fatalf("isRetryableHTTP2Error(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}

func TestListCollaboratorsBackoffsShape(t *testing.T) {
	if len(listCollaboratorsBackoffs) != 3 {
		t.Fatalf("expected 3 backoff slots, got %d", len(listCollaboratorsBackoffs))
	}
	prev := time.Duration(0)
	for i, d := range listCollaboratorsBackoffs {
		if d <= prev {
			t.Fatalf("backoff[%d]=%s is not strictly increasing from %s", i, d, prev)
		}
		prev = d
	}
}
