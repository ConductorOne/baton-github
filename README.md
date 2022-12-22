![Baton Logo](./docs/images/baton-logo.png)

# `baton-github` [![Go Reference](https://pkg.go.dev/badge/github.com/conductorone/baton-github.svg)](https://pkg.go.dev/github.com/conductorone/baton-github) ![main ci](https://github.com/conductorone/baton-github/actions/workflows/main.yaml/badge.svg)

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

By default, `baton-github` will sync information from any organizations that the provided credential has Administrator permissions on. You can specify exactly which organizations you would like to sync using the `--orgs` flag.

# Contributing, Support and Issues

We started Baton because we were tired of taking screenshots and manually building spreadsheets. We welcome contributions, and ideas, no matter how small -- our goal is to make identity and permissions sprawl less painful for everyone. If you have questions, problems, or ideas: Please open a Github Issue!

See [CONTRIBUTING.md](https://github.com/ConductorOne/baton/blob/main/CONTRIBUTING.md) for more details.

# `baton-github` Command Line Usage

```
baton-github

Usage:
  baton-github [flags]
  baton-github [command]

Available Commands:
  completion         Generate the autocompletion script for the specified shell
  help               Help about any command

Flags:
  -f, --file string           The path to the c1z file to sync with ($BATON_FILE) (default "sync.c1z")
  -h, --help                  help for baton-github
      --instance-url string   The GitHub instance URL to connect to. ($BATON_INSTANCE_URL) (default "https://github.com")
      --log-format string     The output format for logs: json, console ($BATON_LOG_FORMAT) (default "json")
      --log-level string      The log level: debug, info, warn, error ($BATON_LOG_LEVEL) (default "info")
      --orgs strings          Limit syncing to specific organizations. ($BATON_ORGS)
      --token string          The GitHub access token used to connect to the Github API. ($BATON_TOKEN)
  -v, --version               version for baton-github

Use "baton-github [command] --help" for more information about a command.
```
