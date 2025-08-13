# Project review guidelines

## Excluded Paths

### The following directories and files should be excluded from review:
- `vendor/` - Third-party dependencies managed by Go modules

## Configuration
- configuration fields in config.go should include a WithDisplayName
- field relationships should be defined in the config.go file

## Resource Types
Resource types that do not list entitlements or grants should have the SkipEntitlementsAndGrants annotation in the ResourceType definition.
