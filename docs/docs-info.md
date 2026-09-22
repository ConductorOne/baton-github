# GitHub Connector Setup Guide

---

## Requirements

- A **GitHub** organization, and for enterprise features a **GitHub Enterprise Cloud** account
- Either a **personal access token (classic)** or a **GitHub App** owned by the organization or the enterprise
- For the built-in Enterprise Owner role: a GitHub App installed on **both** the enterprise account and the organization

---

## Connector capabilities

1. **What resources does the connector sync?**
   This connector syncs:
   - Organizations (the orgs the credential can administer, or the ones named in `--orgs` / `--org`)
   - Users (organization members, with SAML identity emails when SAML is configured)
   - Invitations (users invited to an organization who have not accepted, and invitations that expired)
   - Teams (including nested teams, with their parent team as the parent resource)
   - Repositories (optionally excluding archived ones)
   - Organization roles (GitHub's built-in and custom org roles, also called "enterprise licenses" in GitHub's docs)
   - Enterprise roles (only when `--enterprises` is set)
   - Licenses (enterprise seat consumption, only when `--enterprises` is set)
   - GitHub Apps installed on the organization
   - API keys (fine-grained personal access tokens with access to the org, only when `--sync-secrets` is set)

2. **Can the connector provision any resources? If so, which ones?**
   The connector can provision:
   - Organization membership and admin role via Grant and Revoke. Granting to a user who is not yet a member sends an org invitation
   - Team membership (`member`, `maintainer`) via Grant and Revoke
   - Repository access (`pull`, `triage`, `push`, `maintain`, `admin`) via Grant and Revoke, for users and for teams
   - Organization role assignment via Grant and Revoke
   - The built-in Enterprise **Owner** role via Grant and Revoke, GitHub App authentication only
   - Accounts, via the invitation resource type: `CreateAccount` sends an org invitation, and `Delete` cancels it
   - User removal from the organization via `Delete` on the user resource type

3. **Does the connector emit any event feeds?**
   Yes, when `--sync-last-activity` is set. `github_usage_event_feed` streams member activity from each organization's audit log as usage events, which is what drives last-login and activity reporting. The flag also registers a synthetic app resource that exists only to carry those events.

4. **Does the connector support grant expansion?**
   Yes, in three places:
   - Repository permissions implied by the organization's `default_repository_permission` are emitted against the organization and expanded through the org's `member` and `admin` entitlements, so every member's baseline repository access surfaces without enumerating collaborators
   - An organization role assigned to a team is expanded through that team's `member` and `maintainer` entitlements
   - An enterprise license held by a role is expanded through that role's `assigned` entitlement

   All three are `Shallow`, so the SDK does not recurse further. `--direct-collaborators-only` leans on this expansion instead of fetching per-team repository detail, which cuts API calls on large organizations.

---

## Connector credentials

1. **What credentials or information are needed to set up the connector?**
   This connector accepts one of two authentication methods.

   **Personal access token (classic)**

   **Args**:
   `--token` — the GitHub personal access token
   `--instance-url` — the GitHub instance URL, defaults to `https://github.com`
   `--orgs` — optional, limits syncing to specific organizations

   **GitHub App**

   **Args**:
   `--app-id` — the GitHub App ID
   `--app-privatekey-path` — path to the App's private key `.pem`
   `--org` — required, the single organization the App is installed on
   `--instance-url` — the GitHub instance URL, defaults to `https://github.com`

   Common to both:
   `--enterprises` — enterprises to sync enterprise roles and licenses for
   `--sync-secrets` — sync fine-grained personal access tokens as API keys
   `--sync-last-activity` — emit the audit-log usage event feed. Hidden from `--help` and from the GUI config here, because it only applies to GitHub Enterprise audit-log access; `baton-github-enterprise` sets it directly instead of going through this CLI layer
   `--omit-archived-repositories` — skip archived repositories
   `--direct-collaborators-only` — reduce API calls on large organizations

2. **For each item in the list above:**
   - **How does a user create or look up that credential or info?**

     **Personal access token (classic):**
     1. In GitHub, click your profile photo, then **Settings**
     2. Go to **Developer settings** > **Personal access tokens** > **Tokens (classic)**
     3. Click **Generate new token** > **Generate new token (classic)**
     4. Name the token, optionally set an expiration, and select the scopes below
     5. Click **Generate token** and copy it — it is shown only once

     **GitHub App:**
     1. For enterprise features, create the App under the enterprise account: **Settings** > **GitHub Apps** > **New GitHub App**. Otherwise create it under the organization
     2. Give it a globally unique name, and use a placeholder URL for Homepage and Callback
     3. Uncheck **Active** under Webhook
     4. Select the permissions below
     5. Under **Where can this app be installed?** choose **Only on this account**
     6. Create the App, copy the **App ID**, then generate and save a **private key**
     7. Install the App. For enterprise features install it twice: once on the **enterprise account**, once on the **organization** the connector syncs

   - **Does the credential need any specific scopes or permissions?**

     **Personal access token (classic)** scopes:
     - `repo` — all
     - `admin:org` — all for organization-level provisioning, otherwise `read:org`
     - `user` — all
     - `admin:enterprise` — `read:enterprise`, for enterprise roles and licenses

     If the organization uses SAML single sign-on, the token must also be authorized for that organization.

     **GitHub App** permissions:
     - Repository: **Administration** read and write (implies **Metadata** read)
     - Organization: **Administration** read-only (detects SAML/SSO configuration), **Members** read and write, **Custom organization roles** read and write
     - Enterprise: **Enterprise people** read and write, required to sync and provision the built-in Owner role

   - **Is the list of scopes or permissions different to sync (read) versus provision (read-write)?**
     Yes. Read-only syncing needs `read:org` rather than `admin:org` on a PAT, and read-only equivalents of the App's organization permissions. Provisioning the built-in Enterprise Owner role requires **Enterprise people: read and write**; read-only is not enough because the connector issues invitations and role mutations.

   - **What level of access or permissions does the user need in order to create the credentials?**
     A personal access token must be created by a user with **Enterprise Owner** access when enterprise features are used, and organization admin access otherwise. Creating an enterprise-owned GitHub App requires someone who can manage GitHub Apps for the enterprise; installing it on an organization requires **Org Owner** on that organization.

---

## Resource Details

### Organizations

- **Resource type ID**: `org`
- **Description**: The GitHub organizations the credential can administer, or the ones named in `--orgs` / `--org`
- **Traits**: None
- **Entitlements**: `member` (assignment) and `admin` (permission)
- **Grants**: One grant per organization member for their role. Members are read from the members list; the connector distinguishes admins from plain members
- **Children**: Users, Invitations, Teams, Repositories, Organization roles, GitHub Apps, API keys
- **Provisioning**: Grant adds the member or promotes them to admin. A user who is not yet a member is sent an organization invitation instead, so the membership only exists once they accept. Revoke removes the organization membership

### Users

- **Resource type ID**: `user`
- **Description**: Members of the synced organizations
- **Traits**: User trait with login, email and profile
- **Parent**: Organization
- **Entitlements**: None
- **Grants**: None. Access is emitted by the organization, team, repository and role builders
- **Provisioning**: `Delete` removes the user from the organization
- **Note**: When the organization has SAML single sign-on, emails are read from the SAML identity rather than the public profile. Enterprise-level SAML is read from the enterprise consumed-licenses API, which is PAT-only; when that is unavailable the connector falls back to the REST email

### Invitations

- **Resource type ID**: `invitation`
- **Description**: Users invited to an organization who have not accepted, and invitations GitHub expired
- **Traits**: User trait with `RESOURCE_STATUS_PENDING`, plus `invitation_status` and `invitation_expires_at` profile fields
- **Parent**: Organization
- **Entitlements**: None
- **Grants**: None
- **Provisioning**: `CreateAccount` sends an organization invitation; `Delete` cancels it
- **Note**: Organization invitations expire seven days after creation. Because a pending invitation disappears once accepted, this resource type opts out of sync anomaly detection

### Teams

- **Resource type ID**: `team`
- **Description**: GitHub teams, including nested teams
- **Traits**: Group trait
- **Parent**: Organization, or the parent team for a nested team
- **Entitlements**: `member`, `maintainer` (permission)
- **Grants**: One grant per team member for their role
- **Provisioning**: Grant and Revoke add or remove team membership

### Repositories

- **Resource type ID**: `repository`
- **Description**: Repositories of the synced organizations
- **Traits**: None
- **Parent**: Organization
- **Entitlements**: `pull`, `triage`, `push`, `maintain`, `admin` (permission), grantable to users and teams, and declared as an exclusion group because a principal holds one level at a time
- **Grants**: One grant per collaborator for their permission level, and one per team with repository access. The organization's `default_repository_permission` is expanded into the cumulative levels it implies and emitted against the organization, annotated as expandable through the org's `member` and `admin` entitlements
- **Provisioning**: Grant and Revoke add or remove a collaborator, or a team's repository access
- **Note**: `--omit-archived-repositories` skips archived repositories. `--direct-collaborators-only` relies on grant expansion for team access instead of fetching per-team detail

### Organization roles

- **Resource type ID**: `org_role`
- **Description**: GitHub's built-in and custom organization roles. GitHub's documentation also calls these "enterprise licenses"
- **Traits**: Role trait
- **Parent**: Organization
- **Entitlements**: `assigned` (assignment)
- **Grants**: One grant per user and per team assigned to the role. A team's grant is expandable through that team's `member` and `maintainer` entitlements
- **Provisioning**: Grant and Revoke assign or unassign the role

### Enterprise roles

- **Resource type ID**: `enterprise_role`
- **Description**: Roles of an enterprise account. Only synced when `--enterprises` is set
- **Traits**: Role trait
- **Entitlements**: `assigned` (assignment)
- **Grants**: Under PAT authentication, one grant per user holding each role, read from the enterprise consumed-licenses API. Under GitHub App authentication, only the built-in **Owner** role is visible, and its grants are the users who hold it plus the users who have been invited and have not accepted. The two are emitted against the same entitlement and C1 cannot tell them apart
- **Provisioning**: Only the built-in **Owner** role, and only with GitHub App authentication. See [Enterprise Owner provisioning](#enterprise-owner-provisioning)
- **Limitation**: A GitHub App cannot read `Enterprise.ownerInfo`, so under App authentication the connector sees only the Owner role, not billing managers or custom enterprise roles

### Licenses

- **Resource type ID**: `license`
- **Description**: Enterprise seat consumption. Only synced when `--enterprises` is set
- **Traits**: License profile trait
- **Entitlements**: `assigned` (assignment)
- **Grants**: One grant for the enterprise member role holding the license, expandable through that role's `assigned` entitlement
- **Limitation**: Requires a personal access token. GitHub does not offer the enterprise administration permission to GitHub Apps, so this resource type cannot sync with an App installation token, and its failure fails the whole sync. Customers using a GitHub App with `--enterprises` set must disable this resource type in the connector's resource capabilities in C1

### GitHub Apps

- **Resource type ID**: `app`
- **Description**: GitHub Apps installed on the organization
- **Traits**: App trait, annotated as a non-human identity of type app registration
- **Parent**: Organization
- **Entitlements**: None
- **Grants**: None

### API keys

- **Resource type ID**: `api-key`
- **Description**: Fine-grained personal access tokens with access to the organization. Only synced when `--sync-secrets` is set
- **Traits**: Secret trait
- **Parent**: Organization
- **Entitlements**: None
- **Grants**: None

---

## Enterprise Owner provisioning

Only the built-in **Owner** role of an enterprise is provisionable, and only under GitHub App authentication. The design is shaped by three GitHub constraints:

**`Enterprise.ownerInfo` is invisible to an App.** It holds `admins` and `pendingAdminInvitations`, and it resolves to `null` for an installation token regardless of which permissions the App declares.

**`Enterprise.members(role: OWNER)` is the wrong list.** That argument is an `EnterpriseUserAccountMembershipRole`, whose `OWNER` means "owner of an *organization* in the enterprise" — a different enum from the `EnterpriseAdministratorRole` the mutations take. Owners are read from `Organization.enterpriseOwners` instead, which returns every owner of the organization's enterprise account annotated with their role in that organization. The query must not pass `organizationRole`, because that would drop owners who are not owners of the organization.

**There is no single operation that assigns Owner.** Someone who already administers the enterprise, such as a billing manager, is promoted in place. Anyone else can only be invited, and the role lands when they accept. Which case applies is only readable through `ownerInfo`, so Grant attempts the invitation first and promotes as the fallback, keyed on the `FailedPrecondition` that GitHub's `UNPROCESSABLE` error maps to.

Consequences worth knowing:

- Grant returns the grant in both cases: when it promoted an administrator, and when it only created an invitation. `Grants()` matches that and emits pending invitations alongside accepted Owners, so C1 keeps a record of the request from the moment it is made
- Revoke clears both states rather than treating them as alternatives: it demotes an active Owner to `UNAFFILIATED`, which keeps them as a member of the enterprise rather than evicting them, and cancels an unaccepted invitation. A `NOT_FOUND` on either is success, because it means the state being asked for is already in place
- Reading owners uses the **organization** installation token and every mutation uses the **enterprise** installation token; the enterprise token is rejected on organization fields. Startup verifies that the configured organization belongs to the configured enterprise, and a failure there fails only this resource type
- Only one enterprise can be served under App authentication, because the owners are read through the single configured organization and an organization belongs to exactly one enterprise. A configuration naming several is rejected while the clients are built, which fails this resource type with an explanatory error rather than failing later on a check the operator cannot satisfy. The PAT path does accept a list

### Pending invitations look the same as real access

C1 has no pending state for a grant, so an invitation nobody has accepted and an accepted Owner are emitted as the same grant on the same entitlement, and nothing distinguishes them. That is a deliberate trade: emitting nothing until the invitee accepts would leave the request invisible for up to seven days and leave reviewers no record that it was made.

- An access review or an offboarding sweep counts an invitee as holding Owner. They do not hold it — GitHub assigns the role only on acceptance
- Nothing tracks an expiry. GitHub stops resolving an invitation once it is accepted, cancelled, or expired, so it simply stops being emitted and C1 drops the grant on that sync. `EnterpriseAdministratorInvitation` exposes no `expiresAt`, so there is nothing to compute from either

### The list of pending invitations cannot be complete

`Enterprise.ownerInfo.pendingAdminInvitations` is the only connection of invitations GitHub offers, and it resolves to `null` for an installation token. The root `enterpriseAdministratorInvitation` field answers for one login at a time, so the sync resolves invitations by asking about the enterprise members, batching up to 100 logins into one aliased request — GitHub charges that whole request a single rate-limit point.

An invitation sent to someone who is not a member of the enterprise is therefore invisible to the sync, and that is not hypothetical: an owner invitation can be addressed to any GitHub user. Invitations that C1 itself creates are always visible, because C1 grants to a user it has already synced.

---

## Authentication

The connector supports two methods, selected by which credentials are supplied.

1. **Personal access token (classic)**: a single bearer token used for REST and GraphQL.

2. **GitHub App**: the App's private key signs a JWT, which is exchanged for installation access tokens. Tokens are refreshed automatically when they expire. A connector using enterprise features holds two installation tokens at once, one for the organization and one for the enterprise account, because GitHub splits the data between them.

GraphQL is used for SAML identity lookups, the audit log, and all enterprise owner reads and mutations. Everything else is REST.

---

## API Endpoints Used

**REST** (via `go-github`):

- `GET /user`, `GET /users/{username}`, `GET /user/{id}` — resolve users
- `GET /organizations`, `GET /orgs/{org}`, `GET /organizations/{id}` — list and resolve organizations
- `GET /orgs/{org}/members` — organization members
- `GET /orgs/{org}/memberships/{username}` — a member's role
- `PUT /orgs/{org}/memberships/{username}` — promote to admin (Grant)
- `DELETE /orgs/{org}/memberships/{username}` — remove membership (Revoke, user Delete)
- `POST /orgs/{org}/invitations` — invite a user (Grant, CreateAccount)
- `GET /orgs/{org}/invitations`, `GET /orgs/{org}/failed_invitations` — pending and expired invitations
- `DELETE /orgs/{org}/invitations/{invitation_id}` — cancel an invitation (invitation Delete)
- `GET /orgs/{org}/teams`, `GET /teams/{team_id}` — teams
- `GET /teams/{team_id}/members` — team members
- `PUT /teams/{team_id}/memberships/{username}`, `DELETE /teams/{team_id}/memberships/{username}` — team membership (Grant, Revoke)
- `GET /orgs/{org}/repos`, `GET /repositories/{id}` — repositories
- `GET /repos/{owner}/{repo}/collaborators`, `GET /repos/{owner}/{repo}/collaborators/{username}/permission` — repository access
- `PUT /repos/{owner}/{repo}/collaborators/{username}`, `DELETE /repos/{owner}/{repo}/collaborators/{username}` — repository access (Grant, Revoke)
- `GET /repos/{owner}/{repo}/teams` — teams with repository access
- `PUT /orgs/{org}/teams/{team_slug}/repos/{owner}/{repo}`, `DELETE /orgs/{org}/teams/{team_slug}/repos/{owner}/{repo}` — team repository access (Grant, Revoke)
- `GET /orgs/{org}/organization-roles` — organization roles
- `GET /orgs/{org}/organization-roles/{role_id}/users`, `.../teams` — role assignments
- `GET /orgs/{org}/installations` — installed GitHub Apps, requires `organization_administration=read`
- `GET /orgs/{org}/personal-access-tokens` — fine-grained PATs, only with `--sync-secrets`
- `GET /orgs/{org}/audit-log` — organization audit log
- `GET /app/installations` — the App's installations, authenticated with the App JWT, used to find the enterprise installation
- `GET /enterprises/{enterprise}/consumed-licenses` — enterprise license consumption and enterprise SAML identities. **PAT only**

**GraphQL**:

- `organization(login:) { samlIdentityProvider { externalIdentities } }` — SAML identity emails
- `organization(login:) { enterpriseOwners }` — the enterprise account's owners, read with the organization token
- `enterprise(slug:) { id }`, `enterprise(slug:) { organizations }` — enterprise node ID, and the organization-belongs-to-enterprise check
- `enterpriseAdministratorInvitation(enterpriseSlug:, userLogin:, role:)` — a pending Owner invitation, for one login; the sync aliases up to 100 of these into a single request
- `enterprise(slug:).members` — the candidate logins the pending-invitation lookup asks about
- `inviteEnterpriseAdmin`, `updateEnterpriseAdministratorRole`, `cancelEnterpriseAdminInvitation` — Owner Grant and Revoke

---

## Pagination

- REST endpoints use GitHub's `page` and `per_page` parameters, 100 per page, driven one page per SDK call
- `GET /enterprises/{enterprise}/consumed-licenses` is 1-indexed; page 0 is undocumented and can repeat page 1, producing duplicates
- GraphQL connections use cursor pagination, 100 per page, with the `endCursor` passed through as the SDK page token
- The audit log event feed keeps its own cursor, which carries both the current organization index and GitHub's `after` token, so the feed resumes mid-organization

---

## Rate Limits

- REST: 5,000 requests per hour for a PAT. A GitHub App installation gets a larger budget that scales with the account; installations on this connector's test enterprise reported 15,000
- GraphQL: a separate points-based budget, reported per query in the `rateLimit` field; App installations reported 10,000
- The connector returns GitHub's rate limit headers and the GraphQL `rateLimit` values to the SDK as rate limit annotations, so it backs off rather than failing the sync. GraphQL errors arriving inside an HTTP 200 body are classified, so a rate limit surfaces as retryable rather than as an opaque failure

---

## API Documentation

**Official GitHub API references:**

- **REST**: https://docs.github.com/en/rest
- **GraphQL**: https://docs.github.com/en/graphql
- **Enterprise administration (GraphQL)**: https://docs.github.com/en/graphql/reference/enterprise-admin
- **Permissions required for GitHub Apps**: https://docs.github.com/en/rest/authentication/permissions-required-for-github-apps
- **Inviting people to manage your enterprise**: https://docs.github.com/en/enterprise-cloud@latest/admin/managing-accounts-and-repositories/managing-users-in-your-enterprise/inviting-people-to-manage-your-enterprise
- **Enterprise licensing (REST)**: https://docs.github.com/en/enterprise-cloud@latest/rest/enterprise-admin/license
