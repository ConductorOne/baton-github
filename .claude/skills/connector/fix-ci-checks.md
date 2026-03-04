---
name: fix-ci-checks
description: Fix CI check failures in a connector repo. Covers doc freshness (Generate Baton Metadata), lint and test failures (Verify), and version mismatches (Check versions). Run this when any managed CI workflow fails on your PR.
---

# Fix CI Checks

Fix CI failures on connector PRs. This skill covers the three managed CI workflows: Generate Baton Metadata, Verify, and Check versions.

## Generate Baton Metadata failures

### Docs not matching metadata

**When**: The `Generate Baton Metadata` workflow step `Verify docs match current metadata` fails with "docs/connector.mdx was not updated."

**Cause**: Your code changes altered the connector's capabilities or config schema, but `docs/connector.mdx` wasn't updated to match.

**Fix**:

1. Build the connector and regenerate metadata into the root files:

```bash
CONNECTOR_NAME=$(ls cmd/ | head -1)
go build -o connector "./cmd/${CONNECTOR_NAME}"
./connector capabilities > baton_capabilities.json
./connector config > config_schema.json
```

2. Check what changed:

```bash
git diff baton_capabilities.json
git diff config_schema.json
```

3. Update `docs/connector.mdx` based on what changed. Each type of metadata change maps to a specific doc section:

| What changed | Doc section to update |
|-|-|
| Resource types added/removed | Capabilities table (AUTO-GENERATED) |
| Sync/provision support changed | Capabilities table (AUTO-GENERATED) |
| Config fields added/removed/renamed | Config params step (AUTO-GENERATED) |
| New required OAuth scopes | "Gather credentials" section |
| Permission level changed | "Gather credentials" section |
| New auth method (e.g., added OAuth) | "Gather credentials" section |
| New BatonActionSchema definitions | "Connector actions" section |
| Action arguments changed | "Connector actions" section |

4. For auto-generated sections, find the marker comments and update the content between them:

```
{/* AUTO-GENERATED:START - capabilities */}
...update capabilities table here...
{/* AUTO-GENERATED:END - capabilities */}
```

```
{/* AUTO-GENERATED:START - config-params */}
...update config params here...
{/* AUTO-GENERATED:END - config-params */}
```

#### Capabilities table format

Read `baton_capabilities.json`. The `resourceTypeCapabilities` array lists each resource type and its capabilities. Generate a table:

```mdx
| Resource | Sync | Provision |
| :--- | :--- | :--- |
| Accounts | <Icon icon="square-check" iconType="solid" color="#65DE23"/> | <Icon icon="square-check" iconType="solid" color="#65DE23"/> |
| Groups | <Icon icon="square-check" iconType="solid" color="#65DE23"/> | |
```

- Checkmark: `<Icon icon="square-check" iconType="solid" color="#65DE23"/>`
- No capability: leave cell empty
- Map type names: `user` -> `Accounts`, `group` -> `Groups`, `role` -> `Roles`

#### Config params format

Read `config_schema.json`. The `properties` object has field definitions, `required` array marks required fields:

```mdx
      <Step>
        Enter the required configuration:

        - **api-key** (required): API key for authentication
        - **domain** (required): Your instance domain
        - **include-inactive**: Include inactive users in sync
      </Step>
```

5. Stage the metadata files and docs, then push:

```bash
git add baton_capabilities.json config_schema.json docs/connector.mdx
```

### Build failures in metadata workflow

**When**: The `Generate Baton Metadata` workflow fails at the `Build` step.

**Fix**: The connector binary must compile. Fix build errors first — they'll show in the workflow logs. Common causes:
- Missing vendored dependencies: run `go mod vendor`
- Syntax errors in new code
- Incompatible SDK version

## Verify workflow failures

### Lint failures

**When**: The `Verify / lint` job fails.

**Fix**: Run the linter locally and fix reported issues:

```bash
golangci-lint run --timeout=6m
```

Common issues:
- `errcheck`: unchecked error return — add `if err != nil` handling
- `govet`: struct field alignment or printf format issues
- `goimports`: import ordering — run `goimports -w .`
- `revive`: naming conventions — check exported names follow Go conventions
- `gosec`: security issues — review and fix or add `//nolint:gosec` with justification

### Test failures

**When**: The `Verify / test` job fails.

**Fix**: Run tests locally:

```bash
go test -v -mod=vendor ./...
```

If tests need credentials or a running service, check if the connector supports `--base-url` for mock server testing.

## Check versions failures

`.versions.yaml` is managed by baton-admin. Never edit it directly in a connector repo. To change Go version, SDK version, or other dependency versions:

1. Open a PR on `baton-admin` updating the connector's config in `connectors.yaml` (the `versions` section)
2. Merge that PR — baton-admin pushes the updated `.versions.yaml` to the connector repo
3. Rebase your connector PR onto the updated main
4. Fix any breaking changes from the new SDK/Go version in your PR

### .versions.yaml edited directly

**When**: The `Check versions / block-versions-yaml-edits` job fails with ".versions.yaml is managed by baton-admin and must not be edited in PRs."

**Fix**: Revert the change. If you need different versions, update them via baton-admin (see above).

```bash
git checkout origin/main -- .versions.yaml
```

### Go or SDK version mismatch

**When**: The `Check versions / verify-versions-match` job fails with a version mismatch (e.g., "Go version mismatch: .versions.yaml expects 1.25.2, but go.mod has 1.24.0").

**Cause**: Your `go.mod` doesn't match `.versions.yaml` on main. This happens when baton-admin updated versions after you branched.

**Fix**: Rebase onto the latest main to pick up the version update, then fix any breaking changes:

```bash
git fetch origin
git rebase origin/main
```

If the rebase introduces conflicts in `go.mod` or `go.sum`, resolve them:

```bash
go mod tidy
go mod vendor
```

If the new SDK or Go version introduces breaking changes in your code, fix them as part of your PR.

## If docs/connector.mdx doesn't exist

If your connector doesn't have `docs/connector.mdx` yet, create it using the `build-connector-docs` skill which has the full template structure and writing standards.

## If AUTO-GENERATED markers don't exist

If `docs/connector.mdx` exists but doesn't have the `AUTO-GENERATED` marker comments, add them around the capabilities table and config params step:

```mdx
{/* AUTO-GENERATED:START - capabilities
     Generated from baton_capabilities.json. Do not edit manually. */}

...capabilities section...

{/* AUTO-GENERATED:END - capabilities */}
```
