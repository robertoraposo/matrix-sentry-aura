# Codex Session v1

## Goal

Matrix Sentry owns an official Codex app-server session for each tenant. AURA
never receives passwords, cookies, OAuth refresh tokens, API keys, or files from
`CODEX_HOME`.

## Official protocol

`sentryproviderd` starts the official `codex app-server --stdio` process and uses
its JSONL request/response protocol:

- `initialize`, followed by the `initialized` notification;
- `account/read` for connection state;
- `account/login/start` with `type=chatgptDeviceCode`;
- `account/login/cancel` with the returned `loginId`;
- `account/logout` for revocation.

The browser receives only the temporary `verificationUrl` and `userCode` needed
by the official device-code flow. These values are never logged.

## Tenant isolation

Each tenant receives a private profile:

```text
<provider-root>/tenant-<id>/codex
```

The provider root and every Codex profile use mode `0700`. Codex persists and
refreshes its own official session inside that profile. Profiles are never shared
between tenants.

## Internal HTTP contract

The daemon is internal-only and defaults to `127.0.0.1:8811`. Containers may bind
it to `0.0.0.0:8811` only on a private Docker network; no host port should be
published.

Endpoints:

- `GET /healthz`
- `GET /v1/tenants/{tenant}/providers/codex`
- `POST /v1/tenants/{tenant}/providers/codex/login`
- `POST /v1/tenants/{tenant}/providers/codex/login/cancel`
- `POST /v1/tenants/{tenant}/providers/codex/logout`

`SENTRY_PROVIDERD_TOKEN`, when configured, protects provider endpoints with an
internal Bearer token. Health checks remain credential-free.

Status responses expose metadata only:

- provider and connection state;
- account label/type and plan type when Codex reports them;
- whether OpenAI authentication is required;
- pending `loginId` while connecting.

No credential material, provider tokens, browser cookies, prompts, or Codex
profile contents are returned.

## Scope

This delivery implements session lifecycle only. Model invocation through Codex
is a later delivery after Matrix MCP is wired to the daemon and approval policy is
specified.
