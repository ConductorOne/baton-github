package main

import (
	"context"
	"fmt"

	"github.com/conductorone/baton-sdk/pkg/field"
	"github.com/spf13/viper"
)

var (
	accessTokenField = field.StringField(
		"token",
		field.WithDescription("The GitHub access token used to connect to the GitHub API. ($BATON_TOKEN)"),
	)

	orgsField = field.StringArrayField(
		"orgs",
		field.WithDescription("Limit syncing to specific organizations. ($BATON_ORGS)"),
	)
	instanceUrlField = field.StringField(
		"instance-url",
		field.WithDescription(`The GitHub instance URL to connect to. ($BATON_INSTANCE_URL) (default "https://github.com")`),
	)
)

// configurationFields defines the external configuration required for the connector to run.
var configurationFields = []field.SchemaField{
	accessTokenField,
	orgsField,
	instanceUrlField,
}

// validateConfig is run after the configuration is loaded, and should return an error if it isn't valid.
func validateConfig(ctx context.Context, v *viper.Viper) error {
	if v.GetString(accessTokenField.FieldName) == "" {
		return fmt.Errorf("access token is missing")
	}
	return nil
}
