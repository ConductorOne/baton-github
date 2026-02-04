package connector

import (
	"context"

	"github.com/conductorone/baton-sdk/pkg/types/sessions"
)

// noOpSessionStore always returns nil as response.
type noOpSessionStore struct{}

func (n *noOpSessionStore) Get(ctx context.Context, key string, opt ...sessions.SessionStoreOption) ([]byte, bool, error) {
	return nil, false, nil
}

func (n *noOpSessionStore) GetMany(ctx context.Context, keys []string, opt ...sessions.SessionStoreOption) (map[string][]byte, []string, error) {
	return nil, nil, nil
}

func (n *noOpSessionStore) Set(ctx context.Context, key string, value []byte, opt ...sessions.SessionStoreOption) error {
	return nil
}

func (n *noOpSessionStore) SetMany(ctx context.Context, values map[string][]byte, opt ...sessions.SessionStoreOption) error {
	return nil
}

func (n *noOpSessionStore) Delete(ctx context.Context, key string, opt ...sessions.SessionStoreOption) error {
	return nil
}

func (n *noOpSessionStore) Clear(ctx context.Context, opt ...sessions.SessionStoreOption) error {
	// NOTE: we call this unconditionally for cleanup, so don't throw.
	return nil
}

func (n *noOpSessionStore) GetAll(ctx context.Context, pageToken string, opt ...sessions.SessionStoreOption) (map[string][]byte, string, error) {
	return nil, "", nil
}
