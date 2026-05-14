package connector

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestBufferedInstallationTokenExpiry(t *testing.T) {
	t.Run("subtracts the refresh buffer from a normal stated expiry", func(t *testing.T) {
		stated := time.Now().Add(60 * time.Minute)
		got := bufferedInstallationTokenExpiry(stated)
		require.Equal(t, stated.Add(-installationTokenRefreshBuffer), got)
	})

	t.Run("subtracts the refresh buffer from a short stated expiry too", func(t *testing.T) {
		// 5m stated → buffer of 10m pushes result into the past, which is fine:
		// oauth2 will treat the token as expired and refresh on next use.
		stated := time.Now().Add(5 * time.Minute)
		got := bufferedInstallationTokenExpiry(stated)
		require.Equal(t, stated.Add(-installationTokenRefreshBuffer), got)
		require.True(t, got.Before(time.Now()))
	})

	t.Run("subtracts the refresh buffer from a past stated expiry (still in the past)", func(t *testing.T) {
		stated := time.Now().Add(-5 * time.Minute)
		got := bufferedInstallationTokenExpiry(stated)
		require.Equal(t, stated.Add(-installationTokenRefreshBuffer), got)
	})

	t.Run("zero stated expiry forces an immediate refresh", func(t *testing.T) {
		before := time.Now()
		got := bufferedInstallationTokenExpiry(time.Time{})
		after := time.Now()
		// Should land within the call window — no future expiry assumed.
		require.False(t, got.Before(before))
		require.False(t, got.After(after))
	})
}
