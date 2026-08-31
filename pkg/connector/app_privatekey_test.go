package connector

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"strings"
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

func TestLoadPrivateKeyFromString(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	der := x509.MarshalPKCS1PrivateKey(key)
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: der})
	realNewlines := string(pemBytes)

	t.Run("parses a PEM with real newlines", func(t *testing.T) {
		got, err := loadPrivateKeyFromString(realNewlines)
		require.NoError(t, err)
		require.Equal(t, key.D, got.D)
	})

	t.Run("parses a PEM with literal backslash-n escapes, as a single-line config form field would submit", func(t *testing.T) {
		escaped := strings.ReplaceAll(realNewlines, "\n", `\n`)
		got, err := loadPrivateKeyFromString(escaped)
		require.NoError(t, err)
		require.Equal(t, key.D, got.D)
	})

	t.Run("errors on garbage input", func(t *testing.T) {
		_, err := loadPrivateKeyFromString("not a pem")
		require.Error(t, err)
	})
}
