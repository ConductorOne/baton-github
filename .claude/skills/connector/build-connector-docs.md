<!-- This file is managed by baton-admin. DO NOT EDIT. -->
---
name: c1-connector-docs
description: Write connector documentation for C1 following the established template structure. Use when creating new connector docs in /baton/, updating existing connector pages, or reviewing connector documentation. Ensures consistency with the standardized connector doc format including capabilities tables, credential gathering, and cloud/self-hosted configuration tabs.
---

# C1 Connector Documentation

Write connector documentation for the `/baton/` directory following C1's standardized template.

## When to Use This Skill

Use this skill when:
- Creating new connector documentation
- Updating existing connector docs
- Reviewing connector documentation for consistency
- Converting connector information into proper doc format

## File Format

**Location:** Connector docs live in the connector's GitHub repository in the [C1 organization](https://github.com/ConductorOne), not in this docs repo. Search for the connector repo by name (e.g., `baton-okta`, `baton-salesforce`) to find the right repo.

**Naming:** The doc file should be named `docs.mdx` and placed in the root or a `docs/` directory of the connector repo, following the convention already established in that repo.

**Naming convention:** Use lowercase, hyphenated names matching the GitHub repo name.
- Example: `baton-boomi` repo → connector doc lives in `github.com/conductorone/baton-boomi`

## Information Checklist

Gather this information before writing:

### Basic Details
- App/service name (e.g., "Salesforce", "Boomi", "GitHub")
- Parent company name if applicable (e.g., "Atlassian Jira" vs just "Jira")
- GitHub baton repo name (e.g., `baton-salesforce`)
- Is this a v2/updated version?

### Capabilities
- What resources does it sync? (Accounts, Groups, Roles, Projects, etc.)
- Which resources support provisioning?
- Does it support account provisioning/deprovisioning?
- Any special features? (ticketing, last login, secrets syncing)
- Any limitations or platform-specific notes?

### Authentication
- What credentials are needed? (API token, OAuth app, service account)
- What permission level is required?
- What specific scopes/permissions are needed?
- Which scopes enable provisioning vs read-only?
- Where in the app do users create credentials?

### Configuration
- Is cloud-hosted available?
- Is self-hosted available?
- What fields need to be configured?
- What environment variables are needed for self-hosted?

### Connector Actions
- Does the connector support actions? (Check `baton_capabilities.json` for `CAPABILITY_ACTIONS`)
- What actions are available? (Check `pkg/connector/actions.go` and related files)
- What arguments does each action require?

---

## Document Structure

### Required Frontmatter

```yaml
---
title: "Set up a [Connector Name] connector"
og:title: "Set up a [Connector Name] connector"
description: "[See variants below]"
og:description: "[Same as description]"
sidebarTitle: "[Connector Name]"
---
```

**`description` and `og:description` must always match.** Choose the correct variant based on whether the connector supports provisioning — check the Provision column of the capabilities table.

**If the connector supports provisioning** (at least one resource has a checkmark in the Provision column):
```
"C1 provides identity governance and just-in-time provisioning for [App Name]. Integrate your [App Name] instance with C1 to run user access reviews (UARs), enable just-in-time access requests, and automatically provision and deprovision access."
```

**If the connector does not support provisioning** (no resources have a Provision checkmark):
```
"C1 provides identity governance for [App Name]. Integrate your [App Name] instance with C1 for unified visibility and governance over user access."
```

Use the connector's short product name (e.g., "Looker", not "Google Looker") in the description, even if the `sidebarTitle` uses the full parent-company-qualified name.

### Section Order

1. Version callout (if applicable)
2. Availability notes (if applicable)
3. Capabilities table
4. Connector actions (if applicable)
5. Special concepts section (if needed)
6. Gather credentials
7. Configure the connector (with Cloud/Self-hosted tabs)

---

## Section Templates

### Version Callout (if applicable)

```mdx
<Tip>
**This is version 2 of the [App Name] connector.** This version includes [improvements]. See [Migration guide] for details.
</Tip>
```

### Availability Notes (if applicable)

```mdx
<Warning>
This connector requires [specific requirements]. [Standard versions] are not supported.
</Warning>
```

### Beta Connectors

```mdx
<Warning>
**This connector is in beta.** This means it's undergoing ongoing testing and development while we gather feedback, validate functionality, and improve stability. Beta connectors are generally stable, but they may have limited feature support, incomplete error handling, or occasional issues.

We recommend closely monitoring workflows that use this connector and contacting our Support team with any issues or feedback.
</Warning>
```

### Capabilities Table

```mdx
## Capabilities

| Resource | Sync | Provision |
| :--- | :--- | :--- |
| Accounts | <Icon icon="square-check" iconType="solid" color="#c937ae"/> | <Icon icon="square-check" iconType="solid" color="#c937ae"/> |
| Groups | <Icon icon="square-check" iconType="solid" color="#c937ae"/> | |
| Roles | <Icon icon="square-check" iconType="solid" color="#c937ae"/> | |

**Additional functionality:**
The [App Name] connector supports [automatic account provisioning](/product/admin/account-provisioning).

**Notes:**
- [Important capability details]
- [Limitations or special behaviors]
```

**Icon reference:**
- Checkmark: `<Icon icon="square-check" iconType="solid" color="#c937ae"/>`
- Empty cell: Leave blank (no icon)

### Connector Actions Section (if applicable)

Some connectors support custom actions that can be used in C1 automations. Add this section after the Capabilities table if the connector supports actions.

```mdx
### Connector actions

Connector actions are custom capabilities that extend C1 automations with app-specific operations. You can use connector actions in the [Perform connector action](/product/admin/automations-steps-reference#perform-connector-action) automation step.

| Action name | Additional fields | Description |
|-------------|-------------------|-------------|
| enable_user | `user_id` (string, required) | Unsuspends a suspended user account |
| disable_user | `user_id` (string, required) | Suspends an active user account |
```

**Finding action information in baton repos:**

1. Check `baton_capabilities.json` in the repo root for `CAPABILITY_ACTIONS` in the `connectorCapabilities` array
2. Look for action definitions in:
   - `pkg/connector/actions.go` - main connector-level actions
   - `pkg/connector/user_actions.go` - user resource actions
   - `pkg/connector/group_actions.go` - group resource actions
   - `pkg/connector/connector.go` - sometimes contains `BatonActionSchema` definitions
3. Each action schema includes:
   - `Name` - the action identifier (e.g., `enable_user`)
   - `Arguments` - required and optional fields
   - `ReturnTypes` - what the action returns
   - `Description` - what the action does

**Common action types:**
- `enable_user` / `disable_user` - Account enable/disable (most common)
- `sign_out_user` - Force sign out from all sessions
- `transfer_user_drive_files` / `transfer_user_calendar` - Data transfer operations
- `change_user_org_unit` - Move user to different organizational unit
- `create_group` - Create new groups
- `offboarding_profile_update` - Comprehensive offboarding operations

### Special Concepts Section (if needed)

```mdx
## Understanding [Concept] in [App Name]

[Clear explanation of important concepts users need to understand before setup]
```

### Gather Credentials

```mdx
## Gather [App Name] credentials

<Warning>
To configure the [App Name] connector, you need [specific permission level] permissions in [App Name]. [Additional requirements].
</Warning>

<Steps>
  <Step>
    [First action to take]

    <Tip>
    [Helpful information]
    </Tip>
  </Step>

  <Step>
    [Create credentials with numbered sub-steps if needed]

    1. Navigate to [location]
    2. Click **[Button]**
    3. Enter a name: `C1`
    4. Select the following scopes:
       - `scope:name` - [What this enables]
       - `scope:name` - [What this enables]

    <Warning>
    The **scope:name** scope is used by C1 when automatically provisioning access. **If you do not want C1 to perform these tasks, do not grant this scope.**
    </Warning>

    5. Click **[Generate/Create]**
    6. Copy and save the [token/credentials] securely
  </Step>
</Steps>

For more information, see [link to vendor docs].
```

**Multiple authentication options:** Use subheadings like `### Option 1: [Method]` and `### Option 2: [Method]`

### Configure the Connector

```mdx
## Configure the [App Name] connector

<Tabs>
  <Tab title="Cloud-hosted">
    Follow these instructions to use a built-in, no-code connector hosted by C1.

    <Steps>
      <Step>
        In C1, navigate to **Integrations** > **Connectors** and click **Add connector**.
      </Step>

      <Step>
        Search for **[Connector Name]** and click **Add**.
      </Step>

      <Step>
        Choose how to set up the new [App Name] connector:

        - Add the connector to a currently unmanaged app (select from the list of apps that were discovered in your identity, SSO, or federation provider that aren't yet managed with C1)
        - Add the connector to a managed app (select from the list of existing managed apps)
        - Create a new managed app
      </Step>

      <Step>
        Set the owner for this connector. You can manage the connector yourself, or choose someone else from the list of C1 users. Setting multiple owners is allowed.

        If you choose someone else, C1 will notify the new connector owner by email that their help is needed to complete the setup process.
      </Step>

      <Step>
        Click **Next**.
      </Step>

      <Step>
        Find the **Settings** area of the page and click **Edit**.
      </Step>

      <Step>
        Paste the [credentials] into the relevant fields:

        - **[Field Name]**: [What to enter]
        - **[Field Name]**: [What to enter]
      </Step>

      <Step>
        Click **Save**.
      </Step>

      <Step>
        The connector's label changes to **Syncing**, followed by **Connected**. You can view the logs to ensure that information is syncing.
      </Step>
    </Steps>

    **Done.** Your [App Name] connector is now pulling access data into C1.
  </Tab>

  <Tab title="Self-hosted">
    Follow these instructions to use the [App Name](https://github.com/conductorone/baton-[connector-name]) connector, hosted and run in your own environment.

    When running in service mode on Kubernetes, a self-hosted connector maintains an ongoing connection with C1, automatically syncing and uploading data at regular intervals. This data is immediately available in the C1 UI for access reviews and access requests.

    ### Step 1: Set up a new [App Name] connector

    <Steps>
      <Step>
        In C1, navigate to **Integrations** > **Connectors** > **Add connector**.
      </Step>

      <Step>
        Search for **Baton** and click **Add**.
      </Step>

      <Step>
        Choose how to set up the new [App Name] connector:

        - Add the connector to a currently unmanaged app (select from the list of apps that were discovered in your identity, SSO, or federation provider that aren't yet managed with C1)
        - Add the connector to a managed app (select from the list of existing managed apps)
        - Create a new managed app
      </Step>

      <Step>
        Set the owner for this connector. You can manage the connector yourself, or choose someone else from the list of C1 users. Setting multiple owners is allowed.

        If you choose someone else, C1 will notify the new connector owner by email that their help is needed to complete the setup process.
      </Step>

      <Step>
        Click **Next**.
      </Step>

      <Step>
        In the **Settings** area of the page, click **Edit**.
      </Step>

      <Step>
        Click **Rotate** to generate a new Client ID and Secret.

        Carefully copy and save these credentials. We'll use them in Step 2.
      </Step>
    </Steps>

    ### Step 2: Create Kubernetes configuration files

    Create two Kubernetes manifest files for your [App Name] connector deployment:

    #### Secrets configuration

    ```yaml expandable
    # baton-[connector-name]-secrets.yaml
    apiVersion: v1
    kind: Secret
    metadata:
      name: baton-[connector-name]-secrets
    type: Opaque
    stringData:
      # C1 credentials
      BATON_CLIENT_ID: <C1 client ID>
      BATON_CLIENT_SECRET: <C1 client secret>

      # [App Name] credentials
      BATON_[APP]_[CREDENTIAL_1]: <Your [credential 1]>
      BATON_[APP]_[CREDENTIAL_2]: <Your [credential 2]>

      # Optional: include if you want C1 to provision access using this connector
      BATON_PROVISIONING: true
    ```

    See the connector's README or run `--help` to see all available configuration flags and environment variables.

    #### Deployment configuration

    ```yaml expandable
    # baton-[connector-name].yaml
    apiVersion: apps/v1
    kind: Deployment
    metadata:
      name: baton-[connector-name]
      labels:
        app: baton-[connector-name]
    spec:
      selector:
        matchLabels:
          app: baton-[connector-name]
      template:
        metadata:
          labels:
            app: baton-[connector-name]
            baton: true
            baton-app: [connector-name]
        spec:
          containers:
          - name: baton-[connector-name]
            image: public.ecr.aws/conductorone/baton-[connector-name]:latest
            imagePullPolicy: IfNotPresent
            env:
            - name: BATON_HOST_ID
              value: baton-[connector-name]
            envFrom:
            - secretRef:
                name: baton-[connector-name]-secrets
    ```

    ### Step 3: Deploy the connector

    <Steps>
      <Step>
        Create a namespace in which to run C1 connectors (if desired), then apply the secret config and deployment config files.
      </Step>

      <Step>
        Check that the connector data uploaded correctly. In C1, click **Applications**. On the **Managed apps** tab, locate and click the name of the application you added the [App Name] connector to. [App Name] data should be found on the **Entitlements** and **Accounts** tabs.
      </Step>
    </Steps>

    **Done.** Your [App Name] connector is now pulling access data into C1.
  </Tab>
</Tabs>
```

---

## Writing Standards

### Template Variables

Replace these consistently throughout:

| Variable | Example |
| :--- | :--- |
| `[Connector Name]` | Full official name: "Atlassian Jira Cloud" |
| `[App Name]` | Shortened name: "Jira Cloud" |
| `[connector-name]` | Lowercase with hyphens: "jira-cloud" |
| `[APP]` | Uppercase for env vars: "JIRA" |

### Naming Conventions
- Use official product names with proper capitalization
- Include version numbers when multiple versions exist (e.g., "v2")
- Include parent company when relevant (e.g., "Atlassian Jira")

### Step Granularity
- Each UI action gets its own `<Step>`
- Break down multi-part actions into numbered sub-steps within a `<Step>`
- Don't combine multiple actions in one step

### Language and Tone
- Use active voice: "Click **Save**" not "The Save button should be clicked"
- Be direct and concise
- Use "you" to address the reader
- Use present tense: "The connector syncs..." not "The connector will sync..."
- Don't use contractions in instructions

### UI Elements
- Bold all UI elements: **Connectors**, **Add connector**, **Settings**
- Use exact text from the UI
- Navigation paths: Use ">" for menu paths: **Integrations** > **Connectors**

### Code and Credentials
- Use backticks for code elements: `ConductorOne`, `BATON_CLIENT_ID`
- Use angle brackets for placeholders: `<Your API token>`
- File names in comments: `# baton-connector-name-secrets.yaml`

### Links
- Product docs: `/product/admin/provisioning` (never include `/docs/` — that produces a `/docs/docs/` double-prefix that 404s)
- GitHub repos: `https://github.com/conductorone/baton-[connector-name]`
- Always use full URLs for external links

---

## Common Patterns

### Permission Warnings

```mdx
<Warning>
The **write::org** scope is used by C1 when automatically provisioning and deprovisioning access. **If you do not want C1 to perform these tasks, do not grant this scope.**
</Warning>
```

### Completion Messages

Always end both tabs with:

```mdx
**Done.** Your [App Name] connector is now pulling access data into C1.
```

### Multiple Authentication Options

```mdx
### Option 1: Use a personal access token

<Steps>
  [Instructions]
</Steps>

### Option 2: Use an OAuth app

<Steps>
  [Instructions]
</Steps>
```

---

## Things to Avoid

### Don't Include
- Namespace specifications in YAML (`namespace: baton`)
- Resource limits/requests in deployment YAML
- `kubectl` commands for verification
- Manual sync verification steps (describe UI behavior instead)
- Overly detailed explanations of app setup choices
- "Optional: Enable Sync secrets" step
- Individual `env` variables with `valueFrom` in YAML (use `envFrom` instead)

### Don't Use
- "Setup" as a verb (use "set up")
- Passive voice
- Future tense
- First person ("we", "our")
- Contractions in instructions

---

## Quality Checklist

Before finalizing, verify:

- [ ] Frontmatter is complete and properly formatted
- [ ] `description` and `og:description` match each other
- [ ] Description uses the provisioning variant only if at least one capability table row has a Provision checkmark; otherwise use the no-provisioning variant
- [ ] All sections are in the correct order
- [ ] Connector actions section included if `CAPABILITY_ACTIONS` is present in baton repo
- [ ] Each `<Step>` contains only one primary action
- [ ] All UI elements are bolded
- [ ] Credentials section has clear warnings about required permissions
- [ ] Cloud-hosted tab uses **Integrations** > **Connectors** navigation
- [ ] Self-hosted tab searches for **Baton**
- [ ] Self-hosted tab has "Step 1, Step 2, Step 3" structure
- [ ] YAML files use `-secrets` suffix and `envFrom` pattern
- [ ] YAML has proper labels: `baton: true` and `baton-app: [name]`
- [ ] Both tabs end with completion message
- [ ] All links work
- [ ] Code blocks use proper language tags (`yaml expandable`, `bash`)
- [ ] Placeholder format is consistent: `<Description>`
- [ ] No kubectl verification commands
- [ ] Verification describes UI behavior, not manual checks
