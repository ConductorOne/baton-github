# review-checklist

Pre-merge verification checklist for connector code reviews.

---

## Quick Scan (30 seconds)

Run these checks first to catch obvious issues:

- [ ] **No secrets in code** - Search for `password`, `secret`, `token`, `key` literals
- [ ] **Error prefix present** - All errors start with `baton-{service}:`
- [ ] **Defer after error check** - `defer resp.Body.Close()` never before `if err != nil`
- [ ] **Context passed through** - Every API call receives `ctx`

---

## ResourceSyncer Check

For each resource type (users, groups, roles):

- [ ] Implements `ResourceSyncer` interface correctly
- [ ] Registered with `connectorbuilder.WithResourceSyncers()`
- [ ] Returns `ResourceType()` with correct traits
- [ ] `List()` handles empty results without error
- [ ] `Entitlements()` returns at least one entitlement for membership resources
- [ ] `Grants()` returns grants linking principals to entitlements

---

## Pagination Check

**Termination:**
- [ ] Returns `""` (empty string) when no more pages
- [ ] Does NOT return `""` when there ARE more pages
- [ ] Handles API returning fewer items than requested (not necessarily end)

**Token handling:**
- [ ] Uses `pagination.Bag` for multi-resource pagination
- [ ] Extracts token correctly from incoming `*pagination.Token`
- [ ] Sets next token on outgoing bag

**Red flag patterns:**
```go
// WRONG - always terminates after first page
return resources, "", nil, nil

// WRONG - infinite loop
return resources, "next", nil, nil  // hardcoded token

// CORRECT - check API response
if resp.NextPage == "" {
    return resources, "", nil, nil
}
return resources, resp.NextPage, nil, nil
```

---

## HTTP Safety Check

**Nil pointer safety:**
- [ ] `resp.StatusCode` never accessed when `err != nil` without nil check
- [ ] `resp.Body` never accessed when `err != nil` without nil check
- [ ] `defer resp.Body.Close()` placed AFTER error check

**Type assertions:**
- [ ] All type assertions use two-value form: `x, ok := val.(Type)`
- [ ] Direct assertions `val.(Type)` flagged as potential panic

**ParentResourceId:**
- [ ] `resource.ParentResourceId.Resource` has nil check first
- [ ] Or uses helper: `if resource.ParentResourceId != nil { ... }`

---

## Grant/Revoke Check (if provisioning)

**Entity sources (CRITICAL - #1 bug pattern):**
- [ ] User/account ID comes from `principal.Id.Resource`
- [ ] Context (workspace/org) comes from `principal.ParentResourceId.Resource`
- [ ] Role/group ID comes from `entitlement.Resource.Id.Resource`
- [ ] NO use of `entitlement.Resource.ParentResourceId` for context

**Idempotency:**
- [ ] Grant handles "already exists" as success
- [ ] Revoke handles "not found" as success

**Revoke specifically:**
- [ ] Uses `grant.Principal` for principal info
- [ ] Uses `grant.Entitlement` for entitlement info
- [ ] Context still from `grant.Principal.ParentResourceId`

---

## Error Handling Check

**Wrapping:**
- [ ] All errors use `%w` not `%v` for wrapping
- [ ] All errors include connector prefix: `baton-{service}:`
- [ ] Error messages include relevant IDs (user, resource, page)

**No swallowing:**
- [ ] No `log.Println(err)` followed by continuing
- [ ] All errors either returned or explicitly handled
- [ ] No empty `if err != nil { }` blocks

**Context cancellation:**
- [ ] Long loops check `ctx.Done()` periodically
- [ ] API calls receive context

---

## ID Stability Check

**ResourceId requirements:**
- [ ] IDs are stable across syncs (same user = same ID)
- [ ] IDs don't change when resource is renamed
- [ ] IDs are unique within resource type
- [ ] IDs are deterministic (no random components)

**Common mistakes:**
```go
// WRONG - email can change
rs.NewUserResource(user.Name, userType, user.Email, ...)

// CORRECT - use stable ID
rs.NewUserResource(user.Name, userType, user.ID, ...)
```

---

## Configuration Check

- [ ] Required fields marked with `field.WithRequired(true)`
- [ ] Secrets marked with `field.WithIsSecret(true)`
- [ ] Default values sensible
- [ ] Environment variable naming follows `BATON_` convention
- [ ] No hardcoded credentials or URLs

---

## Testing Indicators

Look for:
- [ ] `_test.go` files exist for non-trivial logic
- [ ] Mock server or API fixtures for integration tests
- [ ] Testability flags (`--base-url`, `--insecure`) in config
- [ ] No tests that require real credentials

---

## Documentation Check

- [ ] README includes required environment variables
- [ ] README shows example usage
- [ ] Complex logic has comments explaining WHY (not what)
- [ ] No TODO comments for critical functionality

---

## Final Sanity Check

Ask yourself:
1. "If this runs against production, what's the worst that happens?"
2. "If an API call fails, will we know what failed and why?"
3. "If pagination breaks, will it infinite loop or stop early?"
4. "If Grant/Revoke runs, will it affect the right user in the right context?"

---

## Review Comment Templates

**For entity source bugs:**
```
This gets the workspace from `entitlement.Resource.ParentResourceId` but it should come from `principal.ParentResourceId`. The principal's context determines where the grant happens.
```

**For pagination bugs:**
```
This always returns an empty next token, which will stop pagination after the first page. The next token should come from the API response.
```

**For nil pointer risks:**
```
If the HTTP request fails, `resp` may be nil. This will panic when accessing `resp.StatusCode`. Add a nil check before accessing response fields in error paths.
```

**For error swallowing:**
```
This logs the error but continues execution. If this fails, the sync will report success with incomplete data. Return the error instead.
```
