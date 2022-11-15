# baton-github

## usage
```
baton-github

Usage:
  baton-github [flags]
  baton-github [command]

Available Commands:
  completion         Generate the autocompletion script for the specified shell
  help               Help about any command

Flags:
  -f, --file string           The path to the c1z file to sync with ($C1_FILE) (default "sync.c1z")
  -h, --help                  help for baton-github
      --instance-url string   The GitHub instance URL to connect to. ($C1_INSTANCE_URL) (default "https://github.com")
      --log-format string     The output format for logs: json, console ($C1_LOG_FORMAT) (default "json")
      --log-level string      The log level: debug, info, warn, error ($C1_LOG_LEVEL) (default "info")
      --orgs strings          Limit syncing to specific organizations. ($C1_ORGS)
      --token string          The GitHub access token used to connect to the Github API. ($C1_TOKEN)
  -v, --version               version for baton-github

Use "baton-github [command] --help" for more information about a command.
```