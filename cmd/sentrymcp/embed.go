package main

import (
	"fmt"

	"matrixsentry/memory"
)

// resolveEmbedder builds the embedder for the configured provider, or returns a
// nil embedder (memory disabled) for the ollama provider when no URL is set —
// preserving the long-standing "no -ollama ⇒ memory tools off" behavior. dim is
// the caller-resolved dimension (1024 for mistral, 768 for ollama by default).
func resolveEmbedder(provider, ollamaURL, model string, dim int, mistralKey string) (memory.Embedder, error) {
	switch provider {
	case "", "ollama":
		if ollamaURL == "" {
			return nil, nil // memory disabled; not an error
		}
		return memory.NewOllamaEmbedder(ollamaURL, model, dim), nil
	case "mistral":
		if mistralKey == "" {
			return nil, fmt.Errorf("provider mistral requires SENTRY_MISTRAL_API_KEY")
		}
		return memory.NewMistralEmbedder(mistralKey, model, dim), nil
	default:
		return nil, fmt.Errorf("unknown SENTRY_EMBED_PROVIDER %q (valid: ollama, mistral)", provider)
	}
}

// oauthSigningKey returns the JWT signing/consent secret: SENTRY_OAUTH_KEY when
// set, else the owner token (back-compat for the single-tenant 8808 server),
// else "" (the caller fatals — OAuth needs a signing key).
func oauthSigningKey(oauthKey, ownerToken string) string {
	if oauthKey != "" {
		return oauthKey
	}
	return ownerToken
}
