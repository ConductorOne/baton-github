package main

import (
	"context"
	"fmt"

	"github.com/conductorone/baton-sdk/pkg/cli"
	"github.com/spf13/cobra"
)

// config defines the external configuration required for the connector to run.
type config struct {
	cli.BaseConfig `mapstructure:",squash"` // Puts the base config options in the same place as the connector options

	Orgs        []string `mapstructure:"orgs"`
	AccessToken string   `mapstructure:"token"`
	InstanceURL string   `mapstructure:"instance-url"`
}

// validateConfig is run after the configuration is loaded, and should return an error if it isn't valid.
func validateConfig(ctx context.Context, cfg *config) error {
	if cfg.AccessToken == "" {
		return fmt.Errorf("access token is missing")
	}

	return nil
}

// cmdFlags sets the cmdFlags required for the connector.
func cmdFlags(cmd *cobra.Command) {
	cmd.PersistentFlags().String("token", "", "The GitHub access token used to connect to the Github API. ($BATON_TOKEN)")
	cmd.PersistentFlags().StringSlice("orgs", []string{}, "Limit syncing to specific organizations. ($BATON_ORGS)")
	cmd.PersistentFlags().String("instance-url", "", `The GitHub instance URL to connect to. ($BATON_INSTANCE_URL) (default "https://github.com")`)
}
