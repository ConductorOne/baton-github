# Project review guidelines

## Excluded Paths

### The following directories and files should be excluded from review:
- `vendor/` - Third-party dependencies managed by Go modules

## Configuration
- configuration fields in config.go should include a WithDisplayName
- field relationships should be defined in the config.go file

## Resource Types
Resource types that do not list entitlements or grants should have the SkipEntitlementsAndGrants annotation in the ResourceType definition.

## Breaking Change Considerations

All connectors should be considered potentially in-use, and the data they expose should be considered a stable API.

### Resource Type Changes

- **NEVER remove a resource type**
- **NEVER change how resource IDs are calculated** - the ID of a given resource must remain stable across all versions of the connector
- **EXERCISE EXTREME CAUTION** when filtering out previously included resources
- **EXERCISE EXTREME CAUTION** when changing how any values associated with a resource are calculated

### Entitlement Changes

- **NEVER remove an entitlement**

### User Profile Changes

Resources implementing the `User` trait may have an associated user profile, typically set using `WithUserProfile`. All changes to user profiles must remain backwards compatible:

- **NEVER remove keys from user profiles**
- **NEVER change the type of a value in a user profile**
- **NEVER change the value how values are represented in a user profile** - eg `alice` should always be `alice`, not `Alice` or `alice@example.com`