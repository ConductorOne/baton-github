# Connector Skills Index

Skills for building and reviewing ConductorOne Baton connectors.

---

## Available Sections

### Concepts (Understanding)

| File | Covers |
|------|--------|
| `concepts-sync-lifecycle.md` | Four sync phases, SDK orchestration, checkpointing |
| `concepts-identifiers.md` | ResourceId vs ExternalId, SourceConnectorIds, match_baton_id |
| `concepts-access-model.md` | Resources, entitlements, grants, traits |

### Building (Implementation)

| File | Covers |
|------|--------|
| `build-syncer.md` | ResourceSyncer interface, List/Entitlements/Grants methods |
| `build-pagination.md` | Token strategies, pagination.Bag, termination conditions |
| `build-provisioning.md` | Grant/Revoke implementation, idempotency, AccountManager |
| `build-config.md` | Configuration schema, CLI flags, environment variables |

### Patterns (Best Practices)

| File | Covers |
|------|--------|
| `patterns-entity-sources.md` | Principal vs entitlement data extraction (CRITICAL) |
| `patterns-http-safety.md` | Nil checks, error handling, response processing |
| `patterns-error-handling.md` | Error wrapping, prefixes, retryable vs fatal |
| `patterns-json-safety.md` | JSON type mismatches, flexible ID/bool types |

### Review (Code Review)

| File | Covers |
|------|--------|
| `review-checklist.md` | Pre-merge verification checklist |
| `review-breaking-changes.md` | What constitutes breaking changes, guardrails |
| `review-common-bugs.md` | Top 5 common bug patterns |

### Reference

| File | Covers |
|------|--------|
| `ref-traits.md` | User/Group/Role/App trait selection |
| `ref-unused-features.md` | SDK features C1 ignores (don't waste effort) |
| `ref-antipatterns.md` | What NOT to do |

---

## Selection Guidelines

### User is building a connector

**"How do I start?"**
- `concepts-access-model.md` - Understand what connectors sync
- `build-syncer.md` - Implement ResourceSyncer

**"How do I handle pagination?"**
- `build-pagination.md` - Token strategies and termination

**"How do I implement Grant/Revoke?"**
- `build-provisioning.md` - Provisioning patterns
- `patterns-entity-sources.md` - Which entity provides which data (CRITICAL)

**"What traits should I use?"**
- `ref-traits.md` - User vs App vs Group vs Role

**"What should I avoid?"**
- `ref-antipatterns.md` - Common mistakes
- `ref-unused-features.md` - Don't waste effort on dead code

### User is reviewing connector code

**"Is this PR safe to merge?"**
- `review-checklist.md` - Verification checklist
- `review-breaking-changes.md` - Breaking change detection

**"What bugs should I look for?"**
- `review-common-bugs.md` - Top 5 bug patterns
- `patterns-entity-sources.md` - Entity confusion detection
- `patterns-http-safety.md` - Nil pointer patterns

### User has a bug

**"Sync hangs forever"**
- `build-pagination.md` - Pagination termination issues
- `review-common-bugs.md` - Infinite loop patterns

**"Grant gives access to wrong user"**
- `patterns-entity-sources.md` - Entity confusion

**"Panic in production"**
- `patterns-http-safety.md` - Nil pointer safety

---

## Quick Reference

**Three resource types every connector needs:**
- User (TRAIT_USER) - principals who receive grants
- Group (TRAIT_GROUP) - collections with "member" entitlement
- Role (TRAIT_ROLE) - permissions with "assigned" entitlement

**Four sync phases (SDK orchestrates):**
1. ResourceType() - discover what types exist
2. List() - fetch all resources
3. Entitlements() - fetch available permissions
4. Grants() - fetch who has what

**Top 3 mistakes:**
1. Entity confusion - getting data from wrong entity in Grant/Revoke
2. Pagination infinite loop - wrong termination condition
3. Nil pointer on HTTP response - accessing resp.Body when resp is nil

**SDK features - usage notes:**
- `WithExternalID()` - **REQUIRED for provisioning** (stores native ID for API calls)
- `WithMFAStatus()`, `WithSSOStatus()` - Only needed for IDP connectors
- `WithStructuredName()` - Rarely needed; DisplayName usually sufficient
