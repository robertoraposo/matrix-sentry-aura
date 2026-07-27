# Ollama bounded invocation v1

## Problem

Matrix used a two-minute non-streaming HTTP timeout for Ollama without
bounding generated output. CPU-only inference could continue generating until
the client deadline, causing a healthy Ollama service to surface as an
unavailable provider.

## Design

- Keep the official non-streaming `POST /api/chat` transport.
- Send Ollama `options.num_predict` on every request.
- Default the output ceiling to 256 tokens.
- Make the ceiling configurable with `SENTRY_OLLAMA_MAX_TOKENS`.
- Make the HTTP deadline configurable with
  `SENTRY_OLLAMA_INVOKE_TIMEOUT_SEC`.
- Default the deadline to 240 seconds.
- Clamp both settings to at least 1.
- Preserve tenant isolation and never expose internal endpoints or secrets.

The output ceiling provides the actual safety bound; the longer timeout
absorbs model loading and slow CPU generation.
