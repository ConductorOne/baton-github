package config

import (
	"github.com/conductorone/baton-sdk/pkg/field"
)

var (
	accessTokenField = field.StringField(
		"token",
		field.WithDescription("The GitHub access token used to connect to the GitHub API."),
	)
	orgsField = field.StringSliceField(
		"orgs",
		field.WithDescription("Limit syncing to specific organizations."),
	)
	instanceUrlField = field.StringField(
		"instance-url",
		field.WithDescription(`The GitHub instance URL to connect to. (default "https://github.com")`),
	)
	appIDField = field.StringField(
		"app-id",
		field.WithDescription("The GitHub App to connect to."),
	)
	appPrivateKey = field.StringField(
		"app-privatekey",
		field.WithDescription("The private key used to connect to the GitHub App"),
	)
	fieldRelationships = []field.SchemaFieldRelationship{
		field.FieldsAtLeastOneUsed(
			accessTokenField,
			appPrivateKey,
		),
		field.FieldsRequiredTogether(
			appPrivateKey,
			appIDField,
		),
	}
)

//go:generate go run ./gen
var Config = field.NewConfiguration([]field.SchemaField{
	accessTokenField,
	orgsField,
	instanceUrlField,
	appIDField,
	appPrivateKey,
}, fieldRelationships...)
