# Codex brokered invocation v1

## Scope

This phase adds one-shot, tenant-isolated Codex text generation to the existing
`provider_invoke` MCP tool.

```text
AURA / MCP client
  -> Matrix MCP provider_invoke
  -> sentryproviderd internal HTTP
  -> tenant-owned official Codex app-server
  -> ChatGPT-managed Codex session
```

## Protocol

Each invocation uses the official app-server lifecycle:

1. `thread/start` with an ephemeral thread;
2. `turn/start` with one text input;
3. consume `item/*` and `turn/completed` notifications;
4. unsubscribe and delete the ephemeral thread best-effort.

## Security boundary

The tenant's `CODEX_HOME` stays inside `sentryproviderd`. Matrix MCP receives
only the final text and operational metadata.

The managed Codex profile disables shell, unified execution, web search, apps,
plugins, computer use, image generation, and multi-agent tools. Every turn uses:

- `approvalPolicy: never`;
- `readOnly` sandbox;
- a private empty temporary working directory;
- fail-closed rejection of server-initiated approval or interactive requests.

If a command, file change, MCP call, collaboration call, web search, or image
view item is observed, Matrix interrupts the turn and returns an error.

## Limits

- model identifier: 256 bytes;
- system text: 8 KiB;
- prompt: 48 KiB;
- output: 64 KiB;
- default invocation timeout: 3 minutes;
- one active invocation per tenant.

Exact token accounting is intentionally deferred. The v1 response returns zero
for token counters rather than estimating or inventing values.
