<!-- This file is managed by baton-admin. DO NOT EDIT. -->
---
name: review-connector
description: Review a baton connector PR. Use when asked to review a connector, review a PR, review connector code, or run a connector code review. Trigger phrases include "review connector", "review PR", "connector review", "code review connector".
---

# Review Baton Connector PR

Perform a structured code review of a baton connector PR using an agent team. Spawns up to 3 reviewer teammates, each with a distinct review lens. Teammates independently explore the code, read full files for context, and report findings. The lead synthesizes all findings into a unified report.

## Prerequisites

Agent teams must be enabled (`CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS=1` in settings.json or environment). If agent teams are not available, fall back to spawning reviewers as parallel Task tool subagents (`subagent_type: "general-purpose"`) instead — the same prompts and criteria apply either way.

## Arguments

Usage: `/review-connector [connector-name] [--base branch] [--fresh] [--pr <number|url>]`

- `--fresh` — Force full review, ignore existing report.
- `--pr <number|url>` — Specify PR number or URL. Auto-detects from branch if omitted.

## Step 1: Determine Context

1. **Project root:** If connector name given, use `~/projects/<name>`. Otherwise use CWD (verify `pkg/connector/connector.go` exists).
2. **Fetch latest remote state:** `git -C <root> fetch origin` — REQUIRED before any diff. Without this, `git diff <base>...HEAD` will include commits already merged to remote main but missing from the stale local ref, producing a wildly inflated diff.
3. **Base branch:** Use `--base` if given, else `git -C <root> symbolic-ref refs/remotes/origin/HEAD 2>/dev/null | sed 's@^refs/remotes/origin/@@'`, fallback `main`.
4. **Current branch:** `git -C <root> branch --show-current` → store as `BRANCH_NAME`.
5. **Changed files:** Use `gh pr diff <number> --name-only` if the PR number is known (most accurate — shows exactly what GitHub will merge). Fall back to `git -C <root> diff origin/<base>...HEAD --name-only` (note: use `origin/<base>`, not bare `<base>`, to diff against the fetched remote ref). Exclude `vendor/`, `conf.gen.go`, non-`.go` files (keep `go.mod`/`go.sum`). Stop if empty.
6. **Full diff:** Similarly prefer `gh pr diff <number>` over local git diff. This avoids stale-main bugs where the local diff includes unrelated merged commits.

## Step 1.1: Fetch PR Context

1. Find PR: if `--pr` given, use `gh pr view <number|url> --repo <owner/repo> --json number,title,body,reviews,url,state`. Otherwise: `gh pr list --head <BRANCH_NAME> --repo <owner/repo> --json number,title,body,reviews,url,state --limit 1`. Derive repo from `git remote get-url origin`.
2. If found, fetch inline review comments: `gh api repos/<owner>/<repo>/pulls/<number>/comments --jq '.[] | {path, line, body, user: .user.login}'`
3. Store PR description and comments as `PR_CONTEXT`. Extract actionable change requests grouped by file as `PR_REQUESTED_CHANGES`.
4. If no PR found, continue without PR context.

## Step 1.5: Resume Detection

Skip if `--fresh`. Check for `<root>/REVIEW_<BRANCH_NAME with / replaced by _>.md`.

If exists: parse previous findings and files reviewed. Compare against current diff. Carry forward findings for unchanged files. Set `FILES_TO_REVIEW` to only new/modified files. If nothing changed, inform user and stop.

If not exists: full review of all changed files.

## Step 2: Classify Changed Files

Classify `FILES_TO_REVIEW` into review domains. This determines which teammates to spawn.

| File Pattern | Category | Reviewer |
|---|---|---|
| `pkg/connector/client*.go`, `pkg/client/*.go` | Client | sync-reviewer |
| `pkg/connector/connector.go` | Connector Core | sync-reviewer |
| `pkg/connector/resource_types.go` | Resource Types | sync-reviewer |
| `pkg/connector/<resource>.go` (users.go, groups.go, etc.) | Resource Builder | sync-reviewer |
| `pkg/connector/*_actions.go`, `pkg/connector/actions.go` | Provisioning | provisioning-reviewer |
| `pkg/config/config.go` | Config | config-reviewer |
| `go.mod`, `go.sum` | Dependencies | config-reviewer |

## Step 3: Create Review Team

Create an agent team to review the connector PR. Spawn up to 3 reviewer teammates in parallel, each with a distinct review lens. Include the project root path, base branch, changed file list, and PR context in each teammate's spawn prompt so they can independently read diffs and full files.

### Teammate Spawning Rules

- If no provisioning files changed → skip provisioning-reviewer
- If no config/dep files changed → skip config-reviewer
- If only config/dep files changed → skip sync-reviewer, only spawn config-reviewer
- Always spawn at least one teammate

### Teammate 1: sync-reviewer

Reviews ALL non-provisioning Go files including breaking change detection. This is the main reviewer.

**Spawn prompt:**

```
You are a sync & correctness reviewer for a Baton connector (Go project syncing identity data from SaaS APIs into ConductorOne).

PROJECT ROOT: <root>
BASE BRANCH: <base>
CHANGED FILES (your scope): <list of sync/client/connector files>
PR CONTEXT: <PR description and PR_REQUESTED_CHANGES if available>

YOUR TASK:
1. Run `git diff <base>...HEAD -- <your files>` to see what changed
2. Read the full file content when the diff suggests a potential issue that requires full-file context (e.g., pagination flow, resource builder structure, surrounding logic)
3. Review against ALL criteria below
4. For each finding, report: file, line range, severity (critical/warning/suggestion), description, and recommendation
5. Check whether any PR review comments (PR_REQUESTED_CHANGES) have been addressed in the current code — flag unaddressed items

IMPORTANT: Only report findings with confidence >= 80%. Verify issues by reading actual code before reporting.

## CLIENT CRITERIA (C1-C7)
- C1: API endpoints documented at top of client.go (endpoints, docs links, required scopes)
- C2: Must use uhttp.BaseHttpClient, not raw http.Client
- C3: Rate limits: return annotations with v2.RateLimitDescription from response headers
- C4: All list functions must paginate
- C5: DRY: central doRequest function; WithQueryParam patterns
- C6: URL construction via url.JoinPath or url.Parse, never string concat
- C7: Endpoint paths as constants, not inline strings
- C8: Pagination math (page token parsing, startIndex defaults, next-page calculation) belongs in pkg/client, not pkg/connector. Connector List methods should pass the raw page token to the client; the client owns the token↔int conversion and default values. Connector-side output chunking (slicing an in-memory list into pages) is fine in pkg/connector.

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
- R12: Service accounts / non-human identities use TRAIT_USER with ACCOUNT_TYPE_SERVICE — NOT TRAIT_APP. TRAIT_APP is for resources that receive access (enterprise apps, databases, licenses), not identities that hold access. This is the established pattern in baton-azure-devops, baton-snyk, baton-microsoft-entra, and baton-google-cloud-platform. For hand-coded connectors: `WithAccountType(v2.UserTrait_ACCOUNT_TYPE_SERVICE)`. For baton-http connectors: `account_type: service` under `user_traits` in config.yaml (also accepts `human`, `system`, or CEL expressions; defaults to `human` if omitted). Verify the field is set before flagging — missing ACCOUNT_TYPE_SERVICE = HIGH.
- R13: WithExternalID is DEPRECATED in baton-sdk (`pkg/types/resource/resource.go:37`: "Deprecated. This field is no longer used."). Do NOT flag missing ExternalID on baton-http connectors — baton-http provisioning uses `principal.Id.Resource` directly, never `GetExternalId()`. For hand-coded connectors (non-baton-http), ExternalID may still be set by convention but is not required by the SDK. Only flag ExternalID issues if the connector's own Grant/Revoke code explicitly calls `GetExternalId()` and depends on it.

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

## BREAKING CHANGES (B1-B9)
- B1: Resource type Id: field changes = CRITICAL (grants orphaned)
- B2: Entitlement slug changes in NewAssignmentEntitlement/NewPermissionEntitlement = CRITICAL
- B3: Resource ID derivation changes (user.ID→user.Email) = CRITICAL
- B4: Parent hierarchy changes (org→workspace) = HIGH
- B5: Removed resource types/entitlements = HIGH
- B6: Trait type changes (NewUserResource→NewAppResource) = MEDIUM
- B7: New required OAuth scopes or permissions = breaking (existing installs lose functionality)
- B8: New API endpoint added to existing sync path = HIGH (may require new scope existing installs don't have)
- B9: SAFE: display name changes, adding new types, adding trait options, adding pagination
- If breaking changes found: must be gated behind config flag (opt-in, never default-on), require lead approval, documented in PR description, docs/connector.mdx updated, DOCS ticket filed

## FORBIDDEN PATTERNS
1. **Conditional resource builder registration based on API probing** = CRITICAL. Never conditionally add/remove ResourceSyncers in `ResourceSyncers()` based on whether an API endpoint returns 200 vs 403/404 at startup (e.g., probing a paid feature endpoint and only registering builders if it succeeds). If the API erroneously returns 403/404 (outage, auth glitch, transient error), the connector silently drops those resource types for the entire sync. C1 interprets missing resources as deletions and **wipes all previously synced data** for those types. Instead: always register all resource builders, and handle API errors gracefully within each builder's List/Grants methods (e.g., return empty results with a warning annotation, or return the error so the sync fails loudly rather than silently deleting data).

## TOP BUG DETECTION PATTERNS
1. Client-side pagination loop: for loop inside List()/Entitlements()/Grants() or inside the HTTP client that fetches all pages internally = CRITICAL (breaks checkpointing, OOM, bypasses rate limiting, detached contexts). The SDK drives the pagination loop — each SDK method must handle exactly one page per call. HTTP client methods must accept a cursor/token param and return a single page.
2. Pagination: `return resources, "", nil, nil` without conditional = stops after page 1
3. Pagination: `return resources, "next", nil, nil` hardcoded = infinite loop
4. HTTP: defer resp.Body.Close() BEFORE if err != nil = panic
5. HTTP: resp.StatusCode in error path without resp != nil check = panic
6. Type assertion: .(Type) without , ok := = panic
7. Error: log.Print(err) without return = silent data loss
8. Error: fmt.Errorf("...%v", err) should be %w
9. IDs: .Email as 3rd arg to NewUserResource = unstable ID — but only flag as WARNING/suggestion. If the API provides no stable numeric/UUID identifier, email is acceptable. Only flag as CRITICAL if a stable ID exists and is being ignored.
10. ParentResourceId.Resource without nil check = panic
11. New API endpoint in existing sync path without checking scope requirements = breaking change
12. baton-http pagination: sections without an explicit `pagination:` block inherit the global `connect.pagination` config — do NOT flag missing pagination on list/grant sections unless they explicitly set `pagination: strategy: none`. When `strategy: none` IS set, verify whether the upstream API actually supports pagination — if it does, this is HIGH (silent data loss beyond the default page size, typically 20-100 items).

Report your findings when done. Structure as a list grouped by file, with severity, line references, and recommendations.
```

### Teammate 2: provisioning-reviewer

Only spawn if `FILES_TO_REVIEW` contains `*_actions.go`, `actions.go`, or resource builder files with Grant/Revoke methods. This reviewer MUST read full provisioning files (not just diffs) because entity source correctness requires understanding the complete Grant/Revoke flow.

**Spawn prompt:**

```
You are a provisioning reviewer for a Baton connector. Your focus is Grant/Revoke correctness — the #1 source of production reverts.

PROJECT ROOT: <root>
BASE BRANCH: <base>
CHANGED FILES (your scope): <list of provisioning/action files>
PR CONTEXT: <PR description and PR_REQUESTED_CHANGES if available>

YOUR TASK:
1. Read the FULL content of each provisioning file (not just diffs) — entity source correctness requires understanding the complete Grant/Revoke flow
2. Run `git diff <base>...HEAD -- <your files>` to see what specifically changed
3. Review against ALL criteria below, paying special attention to P1 (entity sources)
4. For each finding, report: file, line range, severity (critical/warning/suggestion), description, and recommendation
5. If the sync-reviewer also flagged provisioning issues, cross-validate their findings

CRITICAL CONTEXT — Entity Source Rules (caused 3 production reverts):
- WHO (user/account ID): principal.Id.Resource
- WHAT (group/role): entitlement.Resource.Id.Resource
- WHERE (workspace/org): principal.ParentResourceId.Resource
- NEVER get context from entitlement.Resource.ParentResourceId

In Revoke:
- Principal: grant.Principal.Id.Resource
- Entitlement: grant.Entitlement.Resource.Id.Resource
- Context: grant.Principal.ParentResourceId.Resource

## PROVISIONING CRITERIA (P1-P6)
- P1: CRITICAL — entity source correctness per rules above
- P2: Revoke uses grant.Principal and grant.Entitlement correctly
- P3: Grant handles "already exists" as success; Revoke handles "not found" as success
- P4: Validate params before API calls; wrap errors with gRPC status codes
- P5: API argument order — multiple string params are easy to swap (verify against function signature)
- P6: ParentResourceId nil check before access

## HTTP SAFETY (H1-H5)
- H1: defer resp.Body.Close() AFTER err check (panic if resp nil)
- H2: No resp.StatusCode/resp.Body access when resp might be nil
- H3: Type assertions use two-value form: x, ok := val.(Type)
- H4: No error swallowing (log.Println + continue = silent data loss)
- H5: No secrets in logs (apiKey, password, token values)

IMPORTANT: Only report findings with confidence >= 80%. Verify issues by reading actual code before reporting.

Report your findings when done. Structure as a list grouped by file, with severity, line references, and recommendations.
```

### Teammate 3: config-reviewer

Only spawn if `FILES_TO_REVIEW` contains config or dependency files.

**Spawn prompt:**

```
You are a config & dependency reviewer for a Baton connector.

PROJECT ROOT: <root>
BASE BRANCH: <base>
CHANGED FILES (your scope): <list of config/dep files>
PR CONTEXT: <PR description and PR_REQUESTED_CHANGES if available>

YOUR TASK:
1. Run `git diff <base>...HEAD -- <your files>` to see what changed
2. Review against criteria below
3. For each finding, report: file, line range, severity (critical/warning/suggestion), description, and recommendation

## CONFIG CRITERIA (G1-G4)
- G1: conf.gen.go must NEVER be manually edited
- G2: Fields use field.StringField/BoolField from SDK
- G3: Required fields: WithRequired(true); secrets: WithIsSecret(true)
- G4: No hardcoded credentials/URLs; base URL configurable

## DEPENDENCY CHECKS
- Is baton-sdk at a recent version?
- Any unexpected new dependencies?
- Any removed deps still needed?
- Do go.mod changes match the code changes?

IMPORTANT: Only report findings with confidence >= 80%.

Report your findings when done. Structure as a list with severity and recommendations.
```

## Step 4: Synthesize and Cross-Validate

Wait for all teammates to complete their reviews. Then the lead:

1. Collect findings from all teammates.
2. Merge with `PREVIOUS_FINDINGS` (if resumed review).
3. Deduplicate: same file + line range → keep most detailed finding.
4. **Cross-validate entity sources** (if provisioning changed): If the provisioning-reviewer flagged entity source issues, verify by reading the Grant/Revoke code directly. This is the #1 bug — do not rely solely on teammate output.
5. **Cross-validate PR feedback**: Check `PR_REQUESTED_CHANGES` against all findings. Add missing unaddressed items as PR-N warnings.
6. Downgrade breaking changes gated behind config flags from critical → suggestion.
7. Filter any findings with confidence < 80.

## Step 5: Build and Test

Run in parallel (these are independent):

1. `cd <root> && make` — capture pass/fail.
2. `cd <root> && go test ./...` — capture pass/fail.

## Step 6: Write Report

Write to `<root>/REVIEW_<sanitized_branch>.md`.

**REQUIRED:** The review doc MUST include a clickable GitHub PR URL near the top. Use `gh pr view <number> --json url --jq .url` if needed. Never omit this — it is the primary way reviewers navigate from the doc to the PR.

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

## Step 7: Clean Up and Present Results

1. Shut down all reviewer teammates and clean up the team.
2. Print concise summary to the user: severity counts, breaking changes detected (y/n), build/test status, critical findings with file:line, path to report.
