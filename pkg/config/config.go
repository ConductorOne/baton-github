package config

import (
	"github.com/conductorone/baton-sdk/pkg/field"
)

// TODO (mb): Make sure we don't need field.WithRequired(true) for required fields.
var (
	accessTokenField = field.StringField(
		"token",
		field.WithDisplayName("Personal access token"),
		field.WithDescription("The GitHub access token used to connect to the GitHub API."),
		field.WithIsSecret(true),
	)
	orgsField = field.StringSliceField(
		"orgs",
		field.WithDisplayName("Organizations"),
		field.WithDescription("Limit syncing to specific organizations."),
	)
	instanceUrlField = field.StringField(
		"instance-url",
		field.WithDisplayName("GitHub instance URL"),
		field.WithDescription(`The GitHub instance URL to connect to. (default "https://github.com")`),
	)
	appIDField = field.StringField(
		"app-id",
		field.WithDisplayName("GitHub App ID"),
		field.WithDescription("The GitHub App to connect to."),
	)

	appPrivateKeyPath = field.StringField(
		"app-privatekey-path",
		field.WithDisplayName("GitHub App private key (.pem)"),
		field.WithDescription("Path to private key that is used to connect to the GitHub App"),
	)
	syncSecrets = field.BoolField(
		"sync-secrets",
		field.WithDisplayName("Sync secrets"),
		field.WithDescription(`Whether to sync secrets or not`),
	)
	fieldRelationships = []field.SchemaFieldRelationship{
		field.FieldsMutuallyExclusive(
			accessTokenField,
			appPrivateKeyPath,
		),
		field.FieldsRequiredTogether(
			appPrivateKeyPath,
			appIDField,
		),
	}
)

//go:generate go run ./gen
var Config = field.NewConfiguration(
	[]field.SchemaField{
		accessTokenField,
		orgsField,
		instanceUrlField,
		syncSecrets,
		appIDField,
		appPrivateKeyPath,
	},
	field.WithConstraints(fieldRelationships...),
	field.WithConnectorDisplayName("GitHub v2"),
	field.WithHelpUrl("/docs/baton/github-v2"),
	field.WithIconUrl("/static/app-icons/github.svg"),
)
