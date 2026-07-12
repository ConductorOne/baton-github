package main

import (
	"context"

	cfg "github.com/conductorone/baton-github/pkg/config"
	"github.com/conductorone/baton-sdk/pkg/config"
	"github.com/conductorone/baton-sdk/pkg/connectorrunner"

	"github.com/conductorone/baton-github/pkg/connector"
)

var version = "dev"

func main() {
	ctx := context.Background()
	config.RunConnector(ctx, "baton-github", version, cfg.Config, connector.NewLambdaConnector,
		connectorrunner.WithSessionStoreEnabled(),
		connectorrunner.WithKeepPreviousSyncC1Z(),
	)
}
