package main

import (
	"context"
	"fmt"
	"os"

	"github.com/ductone/connector-sdk/pkg/cli"
	"github.com/ductone/connector-sdk/pkg/connectorbuilder"
	"github.com/ductone/connector-sdk/pkg/sdk"
	"github.com/ductone/connector-sdk/pkg/types"
	"github.com/grpc-ecosystem/go-grpc-middleware/logging/zap/ctxzap"
	"go.uber.org/zap"

	"github.com/ductone/connector-github/pkg/connector"
)

var version = "dev"

func main() {
	ctx := context.Background()

	cfg := &config{}
	cmd, err := cli.NewCmd(ctx, "baton-github", cfg, validateConfig, getConnector, run)
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}
	cmd.Version = version
	cmdFlags(cmd)

	err = cmd.Execute()
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}
}

func getConnector(ctx context.Context, cfg *config) (types.ConnectorServer, error) {
	l := ctxzap.Extract(ctx)
	cb, err := connector.New(ctx, cfg.Orgs, cfg.InstanceURL, cfg.AccessToken)
	if err != nil {
		l.Error("error creating connector", zap.Error(err))
		return nil, err
	}

	c, err := connectorbuilder.NewConnector(ctx, cb)
	if err != nil {
		l.Error("error creating connector", zap.Error(err))
		return nil, err
	}

	return c, nil
}

// run is where the process of syncing with the connector is implemented.
func run(ctx context.Context, cfg *config) error {
	l := ctxzap.Extract(ctx)

	c, err := getConnector(ctx, cfg)
	if err != nil {
		return err
	}

	r, err := sdk.NewConnectorRunner(ctx, c, cfg.C1zPath, sdk.WithSlidingMemoryLimiter(50))
	if err != nil {
		l.Error("error creating connector runner", zap.Error(err))
		return err
	}
	defer r.Close()

	err = r.Run(ctx)
	if err != nil {
		l.Error("error running connector", zap.Error(err))
		return err
	}

	return nil
}
