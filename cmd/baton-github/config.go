package main

import (
	"github.com/conductorone/baton-sdk/pkg/field"
)

var (
	accessTokenField = field.StringField(
		"token",
		field.WithDescription("The GitHub access token used to connect to the GitHub API."),
		field.WithRequired(true),
	)
	orgsField = field.StringSliceField(
		"orgs",
		field.WithDescription("Limit syncing to specific organizations."),
	)
	instanceUrlField = field.StringField(
		"instance-url",
		field.WithDescription(`The GitHub instance URL to connect to. (default "https://github.com")`),
	)
	// configuration defines the external configuration required for the connector to run.
	configuration = field.Configuration{
		Fields: []field.SchemaField{
			accessTokenField,
			orgsField,
			instanceUrlField,
		},
	}
)
