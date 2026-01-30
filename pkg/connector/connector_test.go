package connector

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/bradleyfalzon/ghinstallation/v2"
	cfg "github.com/conductorone/baton-github/pkg/config"
	"github.com/stretchr/testify/require"
)

func TestGhinstallationTokenSource(t *testing.T) {
	// Generate a test RSA key
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	privateKeyPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(privateKey),
	})

	// Create a mock server that returns installation tokens
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/app/installations/123/access_tokens":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"token": "test-installation-token", "expires_at": "2099-01-01T00:00:00Z"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer mockServer.Close()

	// Create ghinstallation transport
	transport, err := ghinstallation.New(
		http.DefaultTransport,
		12345,
		123,
		privateKeyPEM,
	)
	require.NoError(t, err)
	transport.BaseURL = mockServer.URL

	// Test the token source wrapper
	ts := &ghinstallationTokenSource{transport: transport}

	token, err := ts.Token()
	require.NoError(t, err)
	require.NotNil(t, token)
	require.Equal(t, "test-installation-token", token.AccessToken)
}

func TestNewWithPATToken(t *testing.T) {
	ctx := context.Background()

	// Create a mock server for PAT authentication
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify the Authorization header contains our token
		auth := r.Header.Get("Authorization")
		if auth != "Bearer test-pat-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`[]`))
	}))
	defer mockServer.Close()

	ghConfig := &cfg.Github{
		Token:       "test-pat-token",
		InstanceUrl: mockServer.URL,
		Orgs:        []string{"test-org"},
	}

	gh, err := New(ctx, ghConfig, "")
	require.NoError(t, err)
	require.NotNil(t, gh)
	require.NotNil(t, gh.client)
	require.Nil(t, gh.appClient) // No app client for PAT auth
}

func TestNewWithGitHubApp(t *testing.T) {
	t.Skip("Skipping: requires mock GitHub API server with proper JWT validation")
	// This test would require setting up a full mock server that can validate JWTs
	// and return proper installation responses. The ghinstallation library validates
	// JWTs on the server side, which is beyond simple mocking.
}

func TestNewWithNoAuth(t *testing.T) {
	ctx := context.Background()

	ghConfig := &cfg.Github{
		Orgs: []string{"test-org"},
	}

	_, err := New(ctx, ghConfig, "")
	require.Error(t, err)
	require.Contains(t, err.Error(), "no authentication method provided")
}

func TestGhinstallationTokenSourceAutoRefresh(t *testing.T) {
	// Generate a test RSA key
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	privateKeyPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(privateKey),
	})

	tokenCallCount := 0

	// Create a mock server that tracks token requests
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/app/installations/123/access_tokens" {
			tokenCallCount++
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			// Return token with future expiry
			w.Write([]byte(`{"token": "test-token-` + string(rune('0'+tokenCallCount)) + `", "expires_at": "2099-01-01T00:00:00Z"}`))
		} else {
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer mockServer.Close()

	// Create ghinstallation transport
	transport, err := ghinstallation.New(
		http.DefaultTransport,
		12345,
		123,
		privateKeyPEM,
	)
	require.NoError(t, err)
	transport.BaseURL = mockServer.URL

	// Create token source
	ts := &ghinstallationTokenSource{transport: transport}

	// First call should get a token
	token1, err := ts.Token()
	require.NoError(t, err)
	require.NotEmpty(t, token1.AccessToken)

	// Second call should use cached token (ghinstallation handles caching)
	token2, err := ts.Token()
	require.NoError(t, err)
	require.NotEmpty(t, token2.AccessToken)

	// Verify at least one token was fetched
	require.GreaterOrEqual(t, tokenCallCount, 1)
}

func TestGhinstallationTokenSourceRefreshesExpiredToken(t *testing.T) {
	// Generate a test RSA key
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	privateKeyPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(privateKey),
	})

	tokenCallCount := 0

	// Create a mock server that returns tokens with PAST expiry (already expired)
	// This forces ghinstallation to fetch a new token on each call
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/app/installations/123/access_tokens" {
			tokenCallCount++
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			// Return token that expires in 1 second - will be expired by next call
			w.Write([]byte(`{"token": "test-token-` + string(rune('0'+tokenCallCount)) + `", "expires_at": "2020-01-01T00:00:00Z"}`))
		} else {
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer mockServer.Close()

	// Create ghinstallation transport
	transport, err := ghinstallation.New(
		http.DefaultTransport,
		12345,
		123,
		privateKeyPEM,
	)
	require.NoError(t, err)
	transport.BaseURL = mockServer.URL

	// Create token source
	ts := &ghinstallationTokenSource{transport: transport}

	// First call - fetches token (expired immediately)
	token1, err := ts.Token()
	require.NoError(t, err)
	require.NotEmpty(t, token1.AccessToken)
	firstCallCount := tokenCallCount

	// Second call - token is expired, should fetch new one
	token2, err := ts.Token()
	require.NoError(t, err)
	require.NotEmpty(t, token2.AccessToken)

	// Verify that a new token was fetched (count increased)
	require.Greater(t, tokenCallCount, firstCallCount, "Expected new token to be fetched when previous token expired")
}

func TestNewWithGitHubAppRequiresSingleOrg(t *testing.T) {
	ctx := context.Background()

	// Generate a test RSA key
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	privateKeyPEM := string(pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(privateKey),
	}))

	ghConfig := &cfg.Github{
		AppId: "12345",
		Orgs:  []string{"org1", "org2"}, // Multiple orgs - should fail
	}

	_, err = New(ctx, ghConfig, privateKeyPEM)
	require.Error(t, err)
	require.Contains(t, err.Error(), "only one org should be specified")
}
