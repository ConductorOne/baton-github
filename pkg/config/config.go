package config

import (
	"github.com/conductorone/baton-sdk/pkg/field"
)

const (
	GithubAppGroup                 = "github-app-group"
	GithubPersonalAccessTokenGroup = "personal-access-token-group"
)

// TODO (mb): Make sure we don't need field.WithRequired(true) for required fields.
var (
	accessTokenField = field.StringField(
		"token",
		field.WithDisplayName("Personal access token"),
		field.WithDescription("The GitHub access token used to connect to the GitHub API."),
		field.WithIsSecret(true),
		field.WithRequired(true),
	)
	orgsField = field.StringSliceField(
		"orgs",
		field.WithDisplayName("Organizations"),
		field.WithDescription("Limit syncing to specific organizations."),
	)
	EnterprisesField = field.StringSliceField(
		"enterprises",
		field.WithDisplayName("Enterprises"),
		field.WithDescription("Sync enterprise roles, must be an admin of the enterprise."),
	)
	instanceUrlField = field.StringField(
		"instance-url",
		field.WithDisplayName("GitHub instance URL"),
		field.WithDescription(`The GitHub instance URL to connect to. (default "https://github.com")`),
	)
	appIDField = field.StringField(
		"app-id",
		field.WithDisplayName("GitHub App ID"),
		field.WithDescription("The GitHub App to connect to."),
		field.WithRequired(true),
	)

	appPrivateKeyPath = field.FileUploadField(
		"app-privatekey-path",
		[]string{".pem"},
		field.WithDisplayName("GitHub App private key (.pem)"),
		field.WithDescription("Path to private key that is used to connect to the GitHub App"),
		field.WithIsSecret(true),
		field.WithRequired(true),
	)

	syncSecrets = field.BoolField(
		"sync-secrets",
		field.WithDisplayName("Sync secrets"),
		field.WithDescription(`Whether to sync secrets or not`),
	)
	omitArchivedRepositories = field.BoolField(
		"omit-archived-repositories",
		field.WithDisplayName("Omit syncing archived repositories"),
		field.WithDescription("Whether to skip syncing archived repositories or not"),
	)
	// directCollaboratorsOnly enables performance optimizations for large orgs:
	// 1. Repo grants: ListCollaborators uses "direct" affiliation instead of "all",
	//    so team-based users are discovered via grant expansion instead of pagination.
	// 2. Org→repo expansion: adds expandable grants from org admin/member entitlements
	//    to repo permissions based on the org's default_repository_permission setting.
	// 3. Team sync: skips per-team GetTeamByID calls (members_count/repos_count become zero).
	directCollaboratorsOnly = field.BoolField(
		"direct-collaborators-only",
		field.WithDisplayName("Optimize sync for large organizations"),
		field.WithDescription(
			"Reduces API calls by using grant expansion for team-based repo access "+
				"and skipping per-team detail fetches. Recommended for large orgs.",
		),
	)
	orgField = field.StringField(
		"org",
		field.WithDisplayName("Github App Organization"),
		field.WithDescription("Organization of your github app"),
		field.WithRequired(true),
	)
	AccountCreationModeField = field.StringField(
		"account-creation-mode",
		field.WithDisplayName("Account creation mode"),
		field.WithDescription(
			`How the connector creates accounts. "invitation" (default) sends an org `+
				`invitation email. "site_admin_create" calls POST /admin/users using a `+
				`site-admin PAT, bypassing email invitations (GHES only).`,
		),
	)
	SiteAdminTokenField = field.StringField(
		"site-admin-token",
		field.WithDisplayName("Site-admin personal access token"),
		field.WithDescription(
			"A GHES site-admin PAT used for account creation when account-creation-mode "+
				"is site_admin_create. This token requires site-admin privilege on the GHES instance.",
		),
		field.WithIsSecret(true),
	)
)

const (
	AccountCreationModeInvitation     = "invitation"
	AccountCreationModeSiteAdminCreate = "site_admin_create"
)

//go:generate go run ./gen
var Config = field.NewConfiguration(
	[]field.SchemaField{
		accessTokenField,
		orgsField,
		EnterprisesField,
		instanceUrlField,
		appIDField,
		appPrivateKeyPath,
		orgField,
		syncSecrets,
		omitArchivedRepositories,
		directCollaboratorsOnly,
		AccountCreationModeField,
		SiteAdminTokenField,
	},
	field.WithConnectorDisplayName("GitHub v2"),
	field.WithHelpUrl("/docs/baton/github-v2"),
	field.WithIconUrl("/static/app-icons/github.svg"),
	field.WithFieldGroups([]field.SchemaFieldGroup{
		{
			Name:        GithubPersonalAccessTokenGroup,
			DisplayName: "Personal access token",
			HelpText:    "Use a personal access token for authentication.",
			Fields:      []field.SchemaField{accessTokenField, orgsField, omitArchivedRepositories, directCollaboratorsOnly},
			Default:     true,
		},
		{
			Name:        GithubAppGroup,
			DisplayName: "GitHub app",
			HelpText:    "Use a github app for authentication",
			Fields:      []field.SchemaField{appIDField, appPrivateKeyPath, orgField, syncSecrets, omitArchivedRepositories, directCollaboratorsOnly},
			Default:     false,
		},
	}),
)
