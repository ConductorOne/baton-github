package connector

import (
	"net/http"
	"testing"

	"github.com/google/go-github/v93/github"
	"github.com/stretchr/testify/require"
)

func newTestGitHubClient(t *testing.T, httpClient *http.Client) *github.Client {
	t.Helper()
	var opts []github.ClientOptionsFunc
	if httpClient != nil {
		opts = append(opts, github.WithHTTPClient(httpClient))
	}
	client, err := github.NewClient(opts...)
	require.NoError(t, err)
	return client
}
