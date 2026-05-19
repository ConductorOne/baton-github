package connector

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestUnauthorized401Logger_PassesNon401ResponsesThrough(t *testing.T) {
	calls := 0
	base := rtFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("ok")), Header: http.Header{}}, nil
	})
	tr := &unauthorized401Logger{base: base}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://api.github.com/x", nil)
	resp, err := tr.RoundTrip(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, 1, calls)
}

func TestUnauthorized401Logger_PreservesResponseBodyOn401(t *testing.T) {
	base := rtFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusUnauthorized,
			Body:       io.NopCloser(bytes.NewReader([]byte(`{"message":"Requires authentication"}`))),
			Header: http.Header{
				"X-Github-Request-Id":   []string{"ABCD:1234:5678"},
				"X-Ratelimit-Remaining": []string{"4998"},
			},
		}, nil
	})
	tr := &unauthorized401Logger{base: base}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://api.github.com/x", nil)
	req.Header.Set("Authorization", "Bearer ghs_abcdefghijklmnopqrst")
	resp, err := tr.RoundTrip(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, `{"message":"Requires authentication"}`, string(body))
}

func TestTokenFingerprint(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"Bearer ghs_abcdefghijklmnopqrst", "ghs_..." + "pqrst"[1:]},
		{"Bearer 12345678", "1234...5678"},
		{"Bearer abc", ""},
		{"", ""},
		{"weirdvalueNoSpace", ""},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			require.Equal(t, tc.want, tokenFingerprint(tc.in))
		})
	}
}
