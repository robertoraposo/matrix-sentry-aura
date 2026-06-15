package main

import (
	"testing"

	"matrixsentry/memory"
)

func TestResolveEmbedderOllama(t *testing.T) {
	e, err := resolveEmbedder("ollama", "http://x:11434", "nomic-embed-text", 768, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := e.(*memory.OllamaEmbedder); !ok {
		t.Fatalf("want *OllamaEmbedder, got %T", e)
	}
}

func TestResolveEmbedderOllamaNoURL(t *testing.T) {
	e, err := resolveEmbedder("ollama", "", "nomic-embed-text", 768, "")
	if err != nil {
		t.Fatal(err)
	}
	if e != nil {
		t.Fatalf("ollama with no URL means memory disabled → nil embedder, got %T", e)
	}
}

func TestResolveEmbedderMistral(t *testing.T) {
	e, err := resolveEmbedder("mistral", "", "mistral-embed", 1024, "sk-key")
	if err != nil {
		t.Fatal(err)
	}
	m, ok := e.(*memory.MistralEmbedder)
	if !ok {
		t.Fatalf("want *MistralEmbedder, got %T", e)
	}
	if m.Dim() != 1024 {
		t.Fatalf("dim = %d, want 1024", m.Dim())
	}
}

func TestResolveEmbedderMistralNoKey(t *testing.T) {
	if _, err := resolveEmbedder("mistral", "", "mistral-embed", 1024, ""); err == nil {
		t.Fatal("mistral without api key must error")
	}
}

func TestResolveEmbedderUnknown(t *testing.T) {
	if _, err := resolveEmbedder("pinecone", "", "m", 768, ""); err == nil {
		t.Fatal("unknown provider must error")
	}
}

func TestOAuthSigningKey(t *testing.T) {
	if got := oauthSigningKey("oauthkey", "ownertoken"); got != "oauthkey" {
		t.Fatalf("SENTRY_OAUTH_KEY should win, got %q", got)
	}
	if got := oauthSigningKey("", "ownertoken"); got != "ownertoken" {
		t.Fatalf("fallback to owner token, got %q", got)
	}
	if got := oauthSigningKey("", ""); got != "" {
		t.Fatalf("both empty → empty, got %q", got)
	}
}
