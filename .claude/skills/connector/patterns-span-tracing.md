# patterns-span-tracing

Span creation, naming, attributes, and error recording for connector observability.

---

## When Connectors Create Spans

Most connectors do not need to create spans manually. The baton-sdk and `uhttp.BaseHttpClient` handle tracing for HTTP calls automatically. You only need to create custom spans when:

- Making API calls through a vendor SDK (not uhttp)
- Performing significant local processing (batch transforms, file parsing)
- Adding connector-specific diagnostic context to a sync phase

If you are using `uhttp.BaseHttpClient` for all requests, **you likely don't need custom spans at all**.

---

## Rule 1: Always Defer span.End()

Every span must be ended. Use `defer` immediately after creation to guarantee cleanup on all code paths, including panics and early returns.

```go
ctx, span := tracer.Start(ctx, "list_users")
defer span.End()
```

**Wrong — span leak on error path:**
```go
ctx, span := tracer.Start(ctx, "list_users")
users, err := client.ListUsers(ctx)
if err != nil {
    return nil, err  // span never ended!
}
span.End()
```

---

## Rule 2: Use Static Snake_Case Span Names

Span names must be static, low-cardinality, snake_case strings. Dynamic values go in span attributes, never in the name. High-cardinality span names break APM grouping and make dashboards unusable.

**Wrong — dynamic span name:**
```go
ctx, span := tracer.Start(ctx, fmt.Sprintf("get_user_%s", userID))
```

**Wrong — camelCase:**
```go
ctx, span := tracer.Start(ctx, "getUser")
```

**Correct:**
```go
ctx, span := tracer.Start(ctx, "get_user",
    trace.WithAttributes(attribute.String("user.id", userID)),
)
defer span.End()
```

---

## Rule 3: Use Dot-Separated Attribute Keys

Span attribute keys follow dot-separated namespacing. Use OTel semantic conventions where applicable.

```go
// CORRECT — dot-separated namespacing
span.SetAttributes(
    attribute.String("tenant.id", tenantID),
    attribute.String("connector.id", connectorID),
    attribute.String("resource.type", resourceType),
    attribute.Int("page.size", pageSize),
)

// WRONG — inconsistent naming
span.SetAttributes(
    attribute.String("tenantID", tenantID),      // camelCase
    attribute.String("connector_id", connectorID), // underscore
    attribute.String("ResourceType", resourceType), // PascalCase
)
```

Common connector attribute keys:
- `resource.type`, `resource.id` — the resource being synced
- `connector.id` — the connector instance
- `tenant.id` — the tenant being synced
- `page.token`, `page.size` — pagination context
- `user.id`, `group.id`, `role.id` — specific entity IDs

---

## Rule 4: Always Set Span Status on Errors

Calling `span.RecordError(err)` alone records the error as an event but does **not** set the span status to Error. APM tools like Datadog will show the span as OK. You must also call `span.SetStatus()`.

**Wrong — span shows as OK despite error:**
```go
if err != nil {
    span.RecordError(err)
    return nil, err
}
```

**Correct — span shows as Error:**
```go
if err != nil {
    span.RecordError(err)
    span.SetStatus(otelcodes.Error, err.Error())
    return nil, err
}
```

If you have access to the `ctxotel` package, use `ctxotel.RecordError(span, err)` which does both in one call.

---

## Rule 5: Never Put Secrets or PII in Span Attributes

Spans are exported to APM backends (Datadog, etc.) where they may be retained indefinitely and visible to operators. Never include:

- API keys, tokens, passwords, or credentials
- User email addresses, names, or other PII
- Request/response bodies that may contain sensitive data
- Tool parameters or function arguments from user input

```go
// WRONG — leaks API key
span.SetAttributes(attribute.String("api.key", apiKey))

// WRONG — leaks user PII
span.SetAttributes(attribute.String("user.email", user.Email))

// CORRECT — use stable IDs only
span.SetAttributes(attribute.String("user.id", user.ID))
```

---

## Rule 6: Break Traces at Loop Boundaries for Per-Resource API Calls

When your connector makes API calls inside a loop (e.g., fetching details per resource in a Grants() call), the resulting trace can grow to thousands of spans, causing APM performance issues and hitting span limits.

For high-volume loops, create a new trace root per iteration with a span link back to the parent:

```go
for _, role := range roles {
    // Create a new trace root with a link to the parent span
    iterCtx, iterSpan := tracer.Start(ctx, "get_role_grants",
        trace.WithNewRoot(),
        trace.WithLinks(trace.Link{
            SpanContext: trace.SpanContextFromContext(ctx),
        }),
        trace.WithAttributes(
            attribute.String("role.id", role.ID),
            attribute.String("role.name", role.Name),
        ),
    )

    grants, err := fetchGrantsForRole(iterCtx, role)
    iterSpan.End()

    if err != nil {
        l.Warn("failed to get grants for role, skipping",
            zap.String("role_id", role.ID), zap.Error(err))
        continue
    }
    allGrants = append(allGrants, grants...)
}
```

**When to apply this:** Only when the loop can exceed ~100 iterations with external calls per iteration. For small loops (< 20 items), normal child spans are fine.

**Why span links:** The link preserves the causal relationship between the parent sync and each per-item trace, so operators can still navigate from the parent to any child trace in their APM tool.
