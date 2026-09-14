<!-- This file is managed by baton-admin. DO NOT EDIT. -->
# build-openapi-spec

Maintain `specs/openapi.json` -- an OpenAPI document describing the endpoints this connector calls.

The spec covers the connector's API surface only, not the vendor's whole API. It is built from two things: `client.go`, which proves *which* endpoints and fields the connector depends on, and the vendor's documentation, which proves what the provider actually returns.

---

## When To Run This

**Run this whenever a change touches the connector's API surface.** It is not optional and not a one-time task -- the spec is maintained alongside `client.go`, the same way tests are.

A change touches the API surface when it:

- adds, removes, or renames a request site
- changes a path, path parameter, query parameter, or request body
- adds or removes a response field the connector reads
- changes auth, base URL, or pagination handling

Check before you open a PR:

```bash
# did this branch touch the request surface?
git diff --stat main... -- '**/client*.go' '**/models.go' '**/*_types.go'
```

If that diff is non-empty and `specs/openapi.json` is unchanged, the spec is stale. Update it in the same PR.

If the connector has no `specs/openapi.json` yet, the first endpoint change creates it -- covering the whole current surface, not just the endpoints you touched.

---

## What Proves What

Three sources, and they are not interchangeable. Using the wrong one is how specs go wrong.

| Question | Authority |
|------|------|
| Which endpoints belong in the spec? | `client.go` -- nothing else. Spec scope is connector scope. |
| Which fields does the connector read? | `client.go` -- the struct tags. |
| What types, formats, and enums does the provider return? | Vendor docs or a recorded response. |
| Is a field required, nullable, or omitted? | Vendor docs or a recorded response. |
| Anything docs and code both leave unclear | Ask the user. Do not guess. |

`client.go` is strong evidence of the contract this connector runs against in production and weak evidence of the provider's actual schema. Vendor docs are the reverse. The spec needs both.

---

## Step 1: Find Every Request Site

Start at the client, then follow the types. Response structs usually live in a sibling file (`models.go`, `resource_types.go`), not in `client.go`.

```bash
# every client method that issues a request
grep -n "func (c \*Client)" pkg/<vendor>/*.go

# every request construction site, including ones outside client.go
grep -rn "NewRequest\|http.Method" pkg/ --include="*.go"

# how requests are configured (auth, body, response target)
grep -rn "uhttp.With" pkg/ --include="*.go"
```

The client file is not always `pkg/client/client.go` -- it may be `pkg/<vendor>/client.go`, or the request sites may be spread across `user.go`, `group.go`, and friends. Grep; do not assume a path.

Include the validation probe (`AuthCheck`, `Validate`) and any capability probe (`CheckScimAccess`). Those are operations too.

This list is the spec's scope. Everything after this step is about proving the shapes.

---

## Step 2: Find The Authoritative Source

Work down this list. Stop at the first level that answers the question -- do not skip to guessing.

**1. URLs already in the repo.** Many clients cite the vendor page next to the call:

```go
// GET /api/members — https://docs.workato.com/workato-api/team.html
```

```bash
grep -rn "// .*https\?://" pkg/ --include="*.go"
```

Check the README's credential section too. These were written by whoever read those pages while building the client, so start here before searching.

**2. A published machine-readable spec.** Search for one: `<vendor> openapi spec`, `<vendor> swagger.json`, `<vendor> api reference`. Also check APIs.guru and the vendor's GitHub org. If one exists, use it as the base and narrow it to the operations from Step 1 -- do not re-derive what the vendor already published.

Validate before trusting it:

- fetched successfully, not a 404 page
- JSON or YAML, not HTML
- carries an `openapi:` or `swagger:` marker
- non-empty `paths`, and it actually contains your endpoints

**3. The vendor's API reference pages.** Search for the specific endpoint: `<vendor> api <resource> endpoint`. Read the request and response tables for types, required fields, enums, and nullability -- the things `client.go` cannot prove.

**4. Ask the user.** When docs are missing, behind a login, contradictory, or silent on a field, ask. Be specific, and say what you will do otherwise:

> `GET /api/members` returns `role` -- the docs don't list its possible values.
> Do you have a sample response or the enum? Otherwise I'll model it as an
> unconstrained string and note it as unproven.

A user who has run this connector can answer in seconds what an hour of searching will not.

Record what you used per operation: the source URL and the date you accessed it. A spec whose provenance is unrecorded cannot be rechecked later.

---

## Step 3: Reconstruct The Path

Paths are assembled at runtime, not written literally. A format verb is a path parameter -- name it from the Go argument, never `%s`.

```go
membershipsUrl, err := getPath(BaseUrl, fmt.Sprintf("/workspaces/%s/workspace_memberships", vars.WorkspaceId))
```

```json
"/workspaces/{workspace_id}/workspace_memberships": {
  "get": {
    "operationId": "getWorkspaceMemberships",
    "parameters": [
      { "name": "workspace_id", "in": "path", "required": true, "schema": { "type": "string" } }
    ]
  }
}
```

**Record which base URL each operation uses.** A connector with more than one (`BaseUrl` and `ScimBaseUrl`) needs separate `servers` entries or fully qualified paths. Getting this wrong silently points half the operations at the wrong host.

---

## Step 4: Capture Every Query Parameter

Including the hardcoded ones. They are not boilerplate -- the response shape is conditional on them.

```go
q := url.Values{}
q.Add("workspace", vars.WorkspaceId)
q.Add("opt_fields", "email,name")   // <- without this, the API omits email
q = paginationQuery(q, vars.Limit, vars.Offset)
```

`opt_fields`, `expand`, `include`, `$select`, and `fields` change which properties come back. Model the parameter with the hardcoded value as its default and say so in the description.

**Trace shared helpers into each caller.** `paginationQuery` adds `limit` and `offset` to five operations; those params belong on all five, not in a helper the spec cannot express.

---

## Step 5: Model The Envelope As Written

If the Go struct nests, the spec nests. Do not flatten to the item schema -- anything consuming this spec has to walk the same path the Go code walks.

```go
type UsersResponse struct {
    Data     []User         `json:"data"`
    NextPage PaginationData `json:"next_page"`
}
```

```json
"200": {
  "content": {
    "application/json": {
      "schema": {
        "type": "object",
        "properties": {
          "data": { "type": "array", "items": { "$ref": "#/components/schemas/User" } },
          "next_page": { "$ref": "#/components/schemas/PaginationData" }
        }
      }
    }
  }
}
```

Record the pagination style per operation: which request param carries the cursor, which response field supplies the next one, and what terminates the loop. Read it off the return statement:

```go
if (res.NextPage != PaginationData{}) {
    return res.Data, res.NextPage.Offset, resp, nil   // cursor: next_page.offset -> offset
}
return res.Data, "", resp, nil                        // terminates on empty next_page
```

Link-header pagination has no OpenAPI representation. Note it alongside the spec instead of inventing a field for it.

---

## Step 6: Derive Schemas From JSON Tags, Then Settle Them Against Docs

The tag name is the property name and the Go type gives the JSON type. Include only the fields the structs declare -- that subset is the whole point.

Then check each field against the source from Step 2. The Go struct proves the connector *reads* a field; it does not prove the shape:

| Go evidence | What it proves | What it does not prove |
|------|------|------|
| `json:"email"` on a string | the connector reads `email` | that the API always sends it, or never sends null |
| `int` ID field | the connector decodes it as a number | that the API never returns a string ID |
| `omitempty` on a request field | the field is optional | nothing at all on a response field |
| non-pointer field | nothing about nullability | that the field is non-nullable |

Mark a field `required` only on documented or recorded evidence. Unproven fields are optional, and say so in the provenance table rather than quietly upgrading them.

---

## Step 7: Model Writes From Their Request Bodies

Provisioning methods build a body struct or a form. Record the content type as written -- JSON body vs form encoding is not interchangeable downstream.

```go
body := baseMutationBody{
    Data: struct {
        User string `json:"user"`
    }{User: userId},
}
req, err := c.httpClient.NewRequest(ctx, http.MethodPost, addUserUrl,
    uhttp.WithBearerToken(c.accessToken),
    uhttp.WithJSONBody(body),
)
```

Map the auth option to a security scheme: `uhttp.WithBearerToken` -> `http`/`bearer`, a header option -> `apiKey` in header, basic auth -> `http`/`basic`.

---

## Step 8: Upgrade Evidence From Fixtures

`*_test.go` files and `test/` fixtures hold real response payloads. A field confirmed by a fixture is proven; a field known only from a struct tag is inferred. Check fixtures before marking anything required.

```bash
grep -rln "httptest\|testdata\|golden" pkg/ --include="*_test.go"
```

---

## Output

Write `specs/openapi.json` at the repo root (`specs/openapi.yaml` is equally acceptable if the repo already uses YAML), plus a provenance table in the PR description:

| Operation | Method / Path | Client | Source | Accessed | Evidence |
|------|------|------|------|------|------|
| `getUsers` | `GET /users` | `pkg/asana/client.go:104` | `developers.asana.com/reference/getusers` | 2026-09-14 | docs + fixture |
| `addUserToWorkspace` | `POST /workspaces/{workspace_id}/addUser` | `pkg/asana/client.go:375` | — | — | client code only |

Validate before handing it off:

```bash
npx @redocly/cli lint specs/openapi.json
```

Call out separately, because the spec cannot hold them:

- Endpoints whose request or response shape could not be proven (blockers -- do not guess them into the spec)
- Retry, rate-limit, and error-response handling the client performs
- Filtering the connector applies after the response returns
- Fields used for resource IDs, entitlement IDs, grant IDs, parent IDs, and `ExternalId` -- these are the parity-critical ones

An Axiomatic reimplementation vendors this file into `specs/source/<namespace>/<version>/openapi.json` and records the source URL and accessed date in that directory's `api.yaml`, so keep both.

---

## Do Not

- **Do not skip this on an endpoint change.** A spec that silently drifts from `client.go` is worse than no spec.
- **Do not add endpoints the client never calls.** Spec scope is connector scope, even when the vendor's published spec offers more.
- **Do not invent fields** the structs do not declare, and do not drop declared fields for looking unused. A field the connector reads is load-bearing.
- **Do not normalize away a hardcoded query parameter.** Dropping `opt_fields` silently changes the response.
- **Do not treat Go types as authoritative** for nullability, format, or required.
- **Do not guess a shape docs do not cover.** Ask the user, or record it as a blocker.
- **Do not flatten response envelopes** to make the schema tidier.
