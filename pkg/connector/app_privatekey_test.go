package connector

import (
	"testing"

	cfg "github.com/conductorone/baton-github/pkg/config"
	"github.com/stretchr/testify/require"
)

func TestAppPrivateKeyPEM(t *testing.T) {
	const (
		inlineKey = "-----BEGIN PRIVATE KEY-----\ninline\n-----END PRIVATE KEY-----"
		pathKey   = "-----BEGIN PRIVATE KEY-----\nfrom-path\n-----END PRIVATE KEY-----"
	)

	t.Run("prefers app-privatekey when both are set", func(t *testing.T) {
		got, err := appPrivateKeyPEM(&cfg.Github{
			AppPrivatekey:     inlineKey,
			AppPrivatekeyPath: []byte(pathKey),
		})
		require.NoError(t, err)
		require.Equal(t, inlineKey, got)
	})

	t.Run("uses app-privatekey when only it is set", func(t *testing.T) {
		got, err := appPrivateKeyPEM(&cfg.Github{AppPrivatekey: inlineKey})
		require.NoError(t, err)
		require.Equal(t, inlineKey, got)
	})

	t.Run("falls back to app-privatekey-path when app-privatekey is empty", func(t *testing.T) {
		got, err := appPrivateKeyPEM(&cfg.Github{AppPrivatekeyPath: []byte(pathKey)})
		require.NoError(t, err)
		require.Equal(t, pathKey, got)
	})

	t.Run("errors when neither is set", func(t *testing.T) {
		_, err := appPrivateKeyPEM(&cfg.Github{})
		require.Error(t, err)
	})
}
