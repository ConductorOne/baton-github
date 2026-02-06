# build-config

Configuration schema and CLI flags.

---

## Field Definitions

```go
// pkg/config/config.go
package config

import "github.com/conductorone/baton-sdk/pkg/field"

var (
    APIKeyField = field.StringField(
        "api-key",
        field.WithDescription("API key for authentication"),
        field.WithRequired(true),
    )

    DomainField = field.StringField(
        "domain",
        field.WithDescription("Service domain (e.g., example.okta.com)"),
        field.WithRequired(true),
    )

    IncludeDisabledField = field.BoolField(
        "include-disabled",
        field.WithDescription("Include disabled users in sync"),
        field.WithDefaultValue(false),
    )

    PageSizeField = field.IntField(
        "page-size",
        field.WithDescription("API page size"),
        field.WithDefaultValue(100),
    )
)

var Configuration = field.NewConfiguration([]field.SchemaField{
    APIKeyField,
    DomainField,
    IncludeDisabledField,
    PageSizeField,
})
```

---

## Environment Variables

Fields automatically become environment variables:

| Field Name | Environment Variable |
|------------|---------------------|
| `api-key` | `BATON_API_KEY` |
| `domain` | `BATON_DOMAIN` |
| `include-disabled` | `BATON_INCLUDE_DISABLED` |

Pattern: `BATON_` + uppercase + underscores

---

## Main Entry Point

```go
// cmd/baton-myservice/main.go
package main

import (
    "context"
    "fmt"
    "os"

    "github.com/myorg/baton-myservice/pkg/config"
    "github.com/myorg/baton-myservice/pkg/connector"
    "github.com/conductorone/baton-sdk/pkg/connectorbuilder"
    "github.com/conductorone/baton-sdk/pkg/connectorrunner"
    "github.com/conductorone/baton-sdk/pkg/types"
    "github.com/grpc-ecosystem/go-grpc-middleware/logging/zap/ctxzap"
    "go.uber.org/zap"
)

var version = "dev"

func main() {
    ctx := context.Background()

    _, cmd, err := configschema.DefineConfiguration(
        ctx,
        "baton-myservice",
        getConnector,
        config.Configuration,
        connectorrunner.WithDefaultCapabilitiesConnectorBuilder(&connector.MyService{}),
    )
    if err != nil {
        fmt.Fprintln(os.Stderr, err.Error())
        os.Exit(1)
    }

    cmd.Version = version

    if err := cmd.Execute(); err != nil {
        fmt.Fprintln(os.Stderr, err.Error())
        os.Exit(1)
    }
}

func getConnector(ctx context.Context, cfg *config.Config) (types.ConnectorServer, error) {
    l := ctxzap.Extract(ctx)

    cb, err := connector.New(ctx, cfg)
    if err != nil {
        l.Error("error creating connector", zap.Error(err))
        return nil, err
    }

    c, err := connectorbuilder.NewConnector(ctx, cb)
    if err != nil {
        l.Error("error creating connector server", zap.Error(err))
        return nil, err
    }

    return c, nil
}
```

---

## OAuth Configuration

For OAuth2 client credentials:

```go
var (
    ClientIDField = field.StringField(
        "client-id",
        field.WithDescription("OAuth2 client ID"),
        field.WithRequired(true),
    )

    ClientSecretField = field.StringField(
        "client-secret",
        field.WithDescription("OAuth2 client secret"),
        field.WithRequired(true),
        field.WithIsSecret(true),  // Marked as secret
    )

    TokenURLField = field.StringField(
        "token-url",
        field.WithDescription("OAuth2 token endpoint"),
    )
)
```

---

## Testability Configuration

For mock server testing:

```go
var (
    BaseURLField = field.StringField(
        "base-url",
        field.WithDescription("Override API base URL (for testing)"),
    )

    InsecureField = field.BoolField(
        "insecure",
        field.WithDescription("Skip TLS verification (for testing)"),
        field.WithDefaultValue(false),
    )
)
```

These enable pointing connector at mock servers in CI.

---

## Field Validation

```go
var (
    PageSizeField = field.IntField(
        "page-size",
        field.WithDescription("API page size (1-1000)"),
        field.WithDefaultValue(100),
        field.WithValidation(func(v int) error {
            if v < 1 || v > 1000 {
                return fmt.Errorf("page-size must be between 1 and 1000")
            }
            return nil
        }),
    )
)
```

---

## Field Relationships

For mutually exclusive or dependent fields:

```go
var relationships = []field.SchemaFieldRelationship{
    field.FieldsExclusivelyRequired(
        APIKeyField,
        ClientIDField,
    ),  // One or the other, not both
}

var Configuration = field.NewConfiguration(
    []field.SchemaField{...},
    field.WithConstraints(relationships...),
)
```

---

## CLI Usage

```bash
# With flags
baton-myservice --api-key="..." --domain="example.com"

# With environment variables
BATON_API_KEY="..." BATON_DOMAIN="example.com" baton-myservice

# Mixed (env vars override flags)
BATON_API_KEY="..." baton-myservice --domain="example.com"

# Output to specific file
baton-myservice --file=/path/to/sync.c1z

# With logging
baton-myservice --log-level=debug --log-format=json
```
