# Codex Admin Bridge v1

## Scope

This phase connects Matrix MCP and Matrix Admin to the tenant-isolated
`sentryproviderd` Codex session service.

## Request path

```text
Matrix Admin
  -> authenticated Matrix MCP tool call
  -> sentryproviderd on the internal Docker network
  -> official Codex app-server
```

The dashboard never calls the provider daemon directly.

## MCP tools

- `provider_connect`
- `provider_connect_cancel`
- `provider_disconnect`
- `provider_status`
- `provider_list`

For Codex, `provider_connect` returns the official device-code verification URL
and short-lived user code. It never returns access tokens, refresh tokens,
cookies, browser storage, or files from `CODEX_HOME`.

## Tenant isolation

The MCP resolves the tenant from its authenticated bearer or OAuth token and
passes only the numeric tenant identifier to `sentryproviderd`. The provider
daemon maps that tenant to its private `CODEX_HOME`.

The tenant selector currently present in Matrix Admin remains a visualization
control. It is not treated as an authentication or session-switching mechanism.

## Configuration

Matrix MCP:

- `SENTRY_PROVIDERD_URL`
- `SENTRY_PROVIDERD_TOKEN` (optional, must match the daemon when enabled)

The provider daemon remains internal-only and must not publish port 8811.
