![Baton Logo](./docs/images/baton-logo.png)

# `baton-github` [![Go Reference](https://pkg.go.dev/badge/github.com/conductorone/baton-github.svg)](https://pkg.go.dev/github.com/conductorone/baton-github) ![ci](https://github.com/conductorone/baton-github/actions/workflows/ci.yaml/badge.svg) ![verify](https://github.com/conductorone/baton-github/actions/workflows/verify.yaml/badge.svg)

`baton-github` is a connector for GitHub built using the [Baton SDK](https://github.com/conductorone/baton-sdk). It communicates with the GitHub API to sync data about which teams and users have access to various repositories within an organization.

Check out [Baton](https://github.com/conductorone/baton) to learn more about the project in general.

# Getting Started

## brew

```
brew install conductorone/baton/baton conductorone/baton/baton-github

BATON_TOKEN=githubAccessToken baton-github
baton resources
```

## docker

```
docker run --rm -v $(pwd):/out -e BATON_TOKEN=githubAccessToken ghcr.io/conductorone/baton-github:latest -f "/out/sync.c1z"
docker run --rm -v $(pwd):/out ghcr.io/conductorone/baton:latest -f "/out/sync.c1z" resources
```

## source

```
go install github.com/conductorone/baton/cmd/baton@main
go install github.com/conductorone/baton-github/cmd/baton-github@main

BATON_TOKEN=githubAccessToken baton-github
baton resources
```

# Data Model

`baton-github` will pull down information about the following GitHub resources:

- Organizations
- Users
- Teams
- Repositories
- GitHub Apps (installed in organizations, synced as non-human identities)

By default, `baton-github` will sync information from any organizations that the provided credential has Administrator permissions on. You can specify exactly which organizations you would like to sync using the `--orgs` flag.

# Sync Secrets
in order to sync secrets, you must use a token created using a github app installed into your organization, more info here:
- [docs](https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/generating-a-user-access-token-for-a-github-app)
- [rest api](https://docs.github.com/rest/orgs/personal-access-tokens#list-fine-grained-personal-access-tokens-with-access-to-organization-resources)

# Contributing, Support and Issues

We started Baton because we were tired of taking screenshots and manually building spreadsheets. We welcome contributions, and ideas, no matter how small -- our goal is to make identity and permissions sprawl less painful for everyone. If you have questions, problems, or ideas: Please open a GitHub Issue!

See [CONTRIBUTING.md](https://github.com/ConductorOne/baton/blob/main/CONTRIBUTING.md) for more details.

# `baton-github` Command Line Usage

```
baton-github

Usage:
  baton-github [flags]
  baton-github [command]

Available Commands:
  capabilities       Get connector capabilities
  completion         Generate the autocompletion script for the specified shell
  config             Get the connector config schema
  help               Help about any command

Flags:
      --app-id string                                    The GitHub App to connect to. ($BATON_APP_ID)
      --app-privatekey string                            Raw PEM contents of the private key used to connect to the GitHub App. Takes precedence over app-privatekey-path when both are set. Since this field cannot hold newlines when entered through a form, literal \n escape sequences are also accepted and unescaped before use. ($BATON_APP_PRIVATEKEY)
      --app-privatekey-path string                       Path to private key that is used to connect to the GitHub App. Ignored when app-privatekey is set. ($BATON_APP_PRIVATEKEY_PATH)
      --client-id string                                 The client ID used to authenticate with ConductorOne ($BATON_CLIENT_ID)
      --client-secret string                             The client secret used to authenticate with ConductorOne ($BATON_CLIENT_SECRET)
      --enterprises strings                              Sync enterprise roles, must be an admin of the enterprise. ($BATON_ENTERPRISES)
      --external-resource-c1z string                     The path to the c1z file to sync external baton resources with ($BATON_EXTERNAL_RESOURCE_C1Z)
      --external-resource-entitlement-id-filter string   The entitlement that external users, groups must have access to sync external baton resources ($BATON_EXTERNAL_RESOURCE_ENTITLEMENT_ID_FILTER)
  -f, --file string                                      The path to the c1z file to sync with ($BATON_FILE) (default "sync.c1z")
  -h, --help                                             help for baton-github
      --instance-url string                              The GitHub instance URL to connect to. (default "https://github.com") ($BATON_INSTANCE_URL)
      --log-format string                                The output format for logs: json, console ($BATON_LOG_FORMAT) (default "console")
      --log-level string                                 The log level: debug, info, warn, error ($BATON_LOG_LEVEL) (default "info")
      --log-level-debug-expires-at string                The timestamp indicating when debug-level logging should expire ($BATON_LOG_LEVEL_DEBUG_EXPIRES_AT)
      --omit-archived-repositories                       Whether to skip syncing archived repositories or not ($BATON_OMIT_ARCHIVED_REPOSITORIES)
      --orgs strings                                     Limit syncing to specific organizations. ($BATON_ORGS)
      --otel-collector-endpoint string                   The endpoint of the OpenTelemetry collector to send observability data to (used for both tracing and logging if specific endpoints are not provided) ($BATON_OTEL_COLLECTOR_ENDPOINT)
  -p, --provisioning                                     This must be set in order for provisioning actions to be enabled ($BATON_PROVISIONING)
      --skip-entitlements-and-grants                     This must be set to skip syncing of entitlements and grants ($BATON_SKIP_ENTITLEMENTS_AND_GRANTS)
      --skip-full-sync                                   This must be set to skip a full sync ($BATON_SKIP_FULL_SYNC)
      --sync-resources strings                           The resource IDs to sync ($BATON_SYNC_RESOURCES)
      --sync-secrets                                     Whether to sync secrets or not ($BATON_SYNC_SECRETS)
      --ticketing                                        This must be set to enable ticketing support ($BATON_TICKETING)
      --token string                                     The GitHub access token used to connect to the GitHub API. ($BATON_TOKEN)
  -v, --version                                          version for baton-github

Use "baton-github [command] --help" for more information about a command.
```

# Authentication

To use this Baton connector, you need to create a GitHub organization access token with the following permissions:

Org:
- Administration: Read-only (required to detect SAML/SSO configuration and to sync installed GitHub Apps)
- Member Read and Write

Repo:
- Administrator: Read and Write
  - This permission implies Metadata: Read
