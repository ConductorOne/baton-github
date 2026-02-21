---
name: review-connector
description: Review a baton connector PR. Use when asked to review a connector, review a PR, review connector code, or run a connector code review. Trigger phrases include "review connector", "review PR", "connector review", "code review connector".
---

# Review Baton Connector PR

Perform a structured code review of a baton connector PR. Uses at most 3 focused agents with embedded criteria to minimize token usage.

## Arguments

Usage: `/review-connector [connector-name] [--base branch] [--fresh] [--pr <number|url>]`

- `--fresh` — Force full review, ignore existing report.
- `--pr <number|url>` — Specify PR number or URL. Auto-detects from branch if omitted.

## Step 1: Determine Context

1. **Project root:** If connector name given, use `~/projects/<name>`. Otherwise use CWD (verify `pkg/connector/connector.go` exists).
2. **Base branch:** Use `--base` if given, else `git -C <root> symbolic-ref refs/remotes/origin/HEAD 2>/dev/null | sed 's@^refs/remotes/origin/@@'`, fallback `main`.
3. **Current branch:** `git -C <root> branch --show-current` → store as `BRANCH_NAME`.
4. **Changed files:** `git -C <root> diff --name-only <base>...HEAD`. Exclude `vendor/`, `conf.gen.go`, non-`.go` files (keep `go.mod`/`go.sum`). Stop if empty.

## Step 1.1: Fetch PR Context

1. Find PR: if `--pr` given, use `gh pr view <number|url> --repo <owner/repo> --json number,title,body,reviews,url,state`. Otherwise: `gh pr list --head <BRANCH_NAME> --repo <owner/repo> --json number,title,body,reviews,url,state --limit 1`. Derive repo from `git remote get-url origin`.
2. If found, fetch inline review comments: `gh api repos/<owner>/<repo>/pulls/<number>/comments --jq '.[] | {path, line, body, user: .user.login}'`
3. Store PR description and comments as `PR_CONTEXT`. Extract actionable change requests grouped by file as `PR_REQUESTED_CHANGES`.
4. If no PR found, continue without PR context.

## Step 1.5: Resume Detection

Skip if `--fresh`. Check for `<root>/REVIEW_<BRANCH_NAME with / replaced by _>.md`.

If exists: parse previous findings and files reviewed. Compare against current diff. Carry forward findings for unchanged files. Set `FILES_TO_REVIEW` to only new/modified files. If nothing changed, inform user and stop.

If not exists: full review of all changed files.

## Step 2: Gather Diffs

For each category of files in `FILES_TO_REVIEW`, gather the git diff:
```
git -C <root> diff <base>...HEAD -- <file-paths>
```

The orchestrator reads diffs and passes them to agents. Agents do NOT read reference docs — all criteria are embedded in their prompts.

## Step 3: Spawn Review Agents

Classify `FILES_TO_REVIEW` and spawn **at most 3 agents** in parallel using the Task tool.

### File Classification

| File Pattern | Category | Agent |
|---|---|---|
| `pkg/connector/client*.go`, `pkg/client/*.go` | Client | sync-reviewer |
| `pkg/connector/connector.go` | Connector Core | sync-reviewer |
| `pkg/connector/resource_types.go` | Resource Types | sync-reviewer |
| `pkg/connector/<resource>.go` (users.go, groups.go, etc.) | Resource Builder | sync-reviewer |
| `pkg/connector/*_actions.go`, `pkg/connector/actions.go` | Provisioning | provisioning-reviewer |
| `pkg/config/config.go` | Config | lightweight-reviewer |
| `go.mod`, `go.sum` | Dependencies | lightweight-reviewer |

### Agent 1: sync-reviewer (sonnet)

Spawn with `subagent_type: "general-purpose"`. Reviews ALL non-provisioning Go files including breaking change detection. This is the main review agent.

**Prompt template:**

```
You are a code reviewer for a Baton connector (Go project syncing identity data from SaaS APIs into ConductorOne).

Review the diffs below against these criteria. For each finding provide JSON:
{"id": "<code>", "severity": "critical|warning|suggestion", "file": "<path>", "lines": "<range>", "description": "<issue>", "recommendation": "<fix>", "confidence": 0-100}

Return a JSON array. Empty array if no issues. Only findings with confidence >= 80.

## CLIENT CRITERIA (C1-C7)
- C1: API endpoints documented at top of client.go (endpoints, docs links, required scopes)
- C2: Must use uhttp.BaseHttpClient, not raw http.Client
- C3: Rate limits: return annotations with v2.RateLimitDescription from response headers
- C4: All list functions must paginate
- C5: DRY: central doRequest function; WithQueryParam patterns
- C6: URL construction via url.JoinPath or url.Parse, never string concat
- C7: Endpoint paths as constants, not inline strings

## RESOURCE CRITERIA (R1-R11)
- R1: List methods return []*Type (pointer slices)
- R2: No unused function parameters
- R3: Clear variable names (groupMember not gm)
- R4: Errors use %w (not %v), include baton-{service}: prefix, use uhttp.WrapErrors
- R5: Use StaticEntitlements for uniform entitlements
- R6: Use Skip annotations (SkipEntitlementsAndGrants, etc.) appropriately
- R7: Missing API permissions = degrade gracefully, don't fail sync
- R8: Pagination via SDK pagination.Bag (Push/Next/Marshal). Return "" when done. NEVER hardcode tokens. NEVER buffer all pages.
- R9: User resources include: status, email, profile, login
- R10: Resource IDs = stable immutable API IDs, never emails or mutable fields
- R11: All API calls receive ctx; long loops check ctx.Done()

## CONNECTOR CRITERIA (N1-N4)
- N1: ResourceSyncers() returns all implemented builders
- N2: Metadata() has accurate DisplayName/Description
- N3: Validate() exercises API credentials (not just return nil)
- N4: New() accepts config, creates client properly

## HTTP SAFETY (H1-H5)
- H1: defer resp.Body.Close() AFTER err check (panic if resp nil)
- H2: No resp.StatusCode/resp.Body access when resp might be nil
- H3: Type assertions use two-value form: x, ok := val.(Type)
- H4: No error swallowing (log.Println + continue = silent data loss)
- H5: No secrets in logs (apiKey, password, token values)

## BREAKING CHANGES (B1-B8) — Check in diffs:
- B1: Resource type Id: field changes = CRITICAL (grants orphaned)
- B2: Entitlement slug changes in NewAssignmentEntitlement/NewPermissionEntitlement = CRITICAL
- B3: Resource ID derivation changes (user.ID→user.Email) = CRITICAL
- B4: Parent hierarchy changes (org→workspace) = HIGH
- B5: Removed resource types/entitlements = HIGH
- B6: Trait type changes (NewUserResource→NewAppResource) = MEDIUM
- B7: New required OAuth scopes = breaking
- B8: SAFE: display name changes, adding new types, adding trait options, adding pagination

## TOP BUG DETECTION PATTERNS
1. Pagination: `return resources, "", nil, nil` without conditional = stops after page 1
2. Pagination: `return resources, "next", nil, nil` hardcoded = infinite loop
3. HTTP: defer resp.Body.Close() BEFORE if err != nil = panic
4. HTTP: resp.StatusCode in error path without resp != nil check = panic
5. Type assertion: .(Type) without , ok := = panic
6. Error: log.Print(err) without return = silent data loss
7. Error: fmt.Errorf("...%v", err) should be %w
8. IDs: .Email as 3rd arg to NewUserResource = unstable ID
9. ParentResourceId.Resource without nil check = panic

Read the FULL file content (using Read tool) ONLY when the diff suggests a potential issue that requires full-file context (e.g., pagination flow, resource builder structure). For simple pattern issues visible in the diff, the diff alone is sufficient.

<IF PR_CONTEXT: include PR description and filtered PR_REQUESTED_CHANGES here>

FILES AND DIFFS:
<paste diffs here, grouped by file>
```

### Agent 2: provisioning-reviewer (sonnet)

Only spawn if `FILES_TO_REVIEW` contains `*_actions.go` or `actions.go` files. This agent MUST read the full provisioning files (not just diffs) because entity source correctness requires understanding the complete Grant/Revoke flow.

**Prompt template:**

```
You are reviewing provisioning (Grant/Revoke) code for a Baton connector.

CRITICAL CONTEXT — Entity Source Rules (caused 3 production reverts):
- WHO (user/account ID): principal.Id.Resource
- WHAT (group/role): entitlement.Resource.Id.Resource
- WHERE (workspace/org): principal.ParentResourceId.Resource
- NEVER get context from entitlement.Resource.ParentResourceId

In Revoke:
- Principal: grant.Principal.Id.Resource
- Entitlement: grant.Entitlement.Resource.Id.Resource
- Context: grant.Principal.ParentResourceId.Resource

Review criteria (P1-P6, H1-H5):
- P1: CRITICAL — entity source correctness per rules above
- P2: Revoke uses grant.Principal and grant.Entitlement correctly
- P3: Grant handles "already exists" as success; Revoke handles "not found" as success
- P4: Validate params before API calls; wrap errors with gRPC status codes
- P5: API argument order — multiple string params are easy to swap (verify against function signature)
- P6: ParentResourceId nil check before access
- H1-H5: (same HTTP safety rules as sync-reviewer)

Read the full provisioning files using the Read tool, then check the diff for what changed.

Return JSON array of findings (same format as above). Confidence >= 80 only.

<IF PR_CONTEXT: include filtered PR_REQUESTED_CHANGES>

FILES TO READ: <list full paths>
DIFFS: <paste diffs>
```

### Agent 3: lightweight-reviewer (haiku)

Only spawn if `FILES_TO_REVIEW` contains config or dependency files. Use `model: "haiku"` for efficiency.

**Prompt template:**

```
Review these connector config/dependency changes:

Config criteria (G1-G4):
- G1: conf.gen.go must NEVER be manually edited
- G2: Fields use field.StringField/BoolField from SDK
- G3: Required fields: WithRequired(true); secrets: WithIsSecret(true)
- G4: No hardcoded credentials/URLs; base URL configurable

Dependency checks:
- Is baton-sdk at a recent version?
- Any unexpected new dependencies?
- Any removed deps still needed?

Return JSON array of findings. Confidence >= 80 only.

DIFFS:
<paste diffs>
```

### Agent Spawning Rules

- If no provisioning files changed → skip Agent 2
- If no config/dep files changed → skip Agent 3
- If only config/dep files changed → skip Agent 1, only spawn Agent 3
- Always spawn at least one agent

## Step 4: Validate and Aggregate

1. Merge `PREVIOUS_FINDINGS` with new agent results.
2. Parse JSON arrays from each agent. Filter confidence < 80.
3. Deduplicate: same file + line range → keep highest confidence.
4. **Cross-validate entity sources** (if provisioning changed): Read the Grant/Revoke code yourself to verify P1/P2 findings. This is the #1 bug.
5. **Cross-validate PR feedback**: Check `PR_REQUESTED_CHANGES` against findings. Add missing unaddressed items as PR-N warnings.
6. Downgrade breaking changes gated behind config flags from critical → suggestion.

## Step 5: Build and Test

1. `cd <root> && make` — capture pass/fail.
2. `cd <root> && go test ./...` — capture pass/fail.

## Step 6: Write Report

Write to `<root>/REVIEW_<sanitized_branch>.md`:

```markdown
# Connector Code Review: <name>

**Branch:** `<branch>` | **Base:** `<base>` | **Date:** <date>
**PR:** [#<n> — <title>](<url>) / No PR found
**Review type:** Full / Resumed (from <prev date>) | **Build:** PASS/FAIL | **Tests:** PASS/FAIL

## Summary

| Severity | Count |
|----------|-------|
| Critical | X |
| Warning  | Y |
| Suggestion | Z |

## Breaking Changes

<findings or "None detected.">

## Unaddressed PR Feedback

<findings or "None.">

## Critical Issues

<grouped by file>

## Warnings

<grouped by file>

## Suggestions

<grouped by file>

## Files Reviewed

| File | Category | Findings | Status |
|------|----------|----------|--------|
| `<path>` | <cat> | <n> | Reviewed / Carried forward |
```

## Step 7: Present Results

Print concise summary: severity counts, breaking changes detected (y/n), build/test status, critical findings with file:line, path to report.
