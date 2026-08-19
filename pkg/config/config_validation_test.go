package config

import (
	"testing"

	"github.com/conductorone/baton-sdk/pkg/field"
	"github.com/spf13/viper"
)

// validate runs the same field validation the SDK CLI performs at startup
// (pkg/cli.commands -> field.Validate with the selected auth-method group).
// An empty authMethod mirrors invoking the connector without an explicit
// --auth-method, which resolves to the default (personal-access-token) group.
func validate(t *testing.T, authMethod string, values map[string]string) error {
	t.Helper()
	v := viper.New()
	for k, val := range values {
		v.Set(k, val)
	}
	if authMethod == "" {
		return field.Validate(Config, v)
	}
	return field.Validate(Config, v, field.WithAuthMethod(authMethod))
}

// TestConfigValidationAuthMethods guards the real connector Config against
// regressions in how the two auth methods validate. In particular it locks in
// that the personal-access-token path validates with only a token set — i.e.
// the "app private key required" rule must NOT leak onto the PAT auth method.
func TestConfigValidationAuthMethods(t *testing.T) {
	cases := []struct {
		name       string
		authMethod string
		values     map[string]string
		wantErr    bool
	}{
		{
			name:       "default auth (PAT) with only token",
			authMethod: "",
			values:     map[string]string{"token": "ghp_example"},
			wantErr:    false,
		},
		{
			name:       "explicit PAT auth with only token",
			authMethod: GithubPersonalAccessTokenGroup,
			values:     map[string]string{"token": "ghp_example"},
			wantErr:    false,
		},
		{
			name:       "github app auth with app-privatekey-path",
			authMethod: GithubAppGroup,
			values: map[string]string{
				"app-id":              "123",
				"org":                 "my-org",
				"app-privatekey-path": "/secrets/key.pem",
			},
			wantErr: false,
		},
		{
			name:       "github app auth with in-memory app-privatekey",
			authMethod: GithubAppGroup,
			values: map[string]string{
				"app-id":         "123",
				"org":            "my-org",
				"app-privatekey": "-----BEGIN PRIVATE KEY-----\n...\n-----END PRIVATE KEY-----",
			},
			wantErr: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validate(t, tc.authMethod, tc.values)
			if tc.wantErr && err == nil {
				t.Fatalf("expected validation error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected validation error: %v", err)
			}
		})
	}
}

// TestFrameworkAtLeastOneConstraintBreaksPATAuth documents WHY the "at least
// one of --app-privatekey / --app-privatekey-path" requirement is enforced in
// the connector's GitHub App constructor (see pkg/connector.appPrivateKeyPEM)
// rather than as a framework-level field.FieldsAtLeastOneUsed constraint.
//
// field.Validate filters the field *presence* map by the selected auth-method
// group, but field.validateConstraints evaluates every constraint on the
// Configuration globally — it has no notion of field groups, and
// SchemaFieldGroup cannot carry group-scoped constraints. So a global
// AtLeastOne(app-privatekey-path, app-privatekey) constraint fires whenever
// neither key is present, INCLUDING under the personal-access-token auth
// method (and the default no-auth-method case), which would reject the common
// `baton-github --token=...` invocation.
//
// This test builds a config that mirrors the real Config but adds that
// constraint, and asserts it breaks PAT auth — if a future SDK version makes
// constraints group-aware this test will start failing, signalling that the
// requirement can safely move to the framework.
func TestFrameworkAtLeastOneConstraintBreaksPATAuth(t *testing.T) {
	cfg := field.NewConfiguration(
		Config.Fields,
		field.WithFieldGroups(Config.FieldGroups),
		field.WithConstraints(field.FieldsAtLeastOneUsed(appPrivateKeyPath, appPrivateKey)),
	)

	v := viper.New()
	v.Set("token", "ghp_example")

	// PAT auth with only a token — no app key is set on purpose.
	err := field.Validate(cfg, v, field.WithAuthMethod(GithubPersonalAccessTokenGroup))
	if err == nil {
		t.Fatalf("expected the global AtLeastOne constraint to (incorrectly) break PAT auth, " +
			"but validation passed — the SDK may now scope constraints to field groups; " +
			"if so, the app-privatekey requirement can move to a framework constraint")
	}
}
