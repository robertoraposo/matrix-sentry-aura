package providerbroker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestInvokeOllamaChat(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/api/chat" {
			t.Fatalf("path = %s, want /api/chat", r.URL.Path)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Fatalf("content-type = %q", got)
		}

		var got struct {
			Model    string `json:"model"`
			Stream   bool   `json:"stream"`
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}

		if got.Model != "qwen3:8b" {
			t.Fatalf("model = %q", got.Model)
		}
		if got.Stream {
			t.Fatal("stream must be false")
		}
		if len(got.Messages) != 2 {
			t.Fatalf("messages = %#v", got.Messages)
		}
		if got.Messages[0].Role != "system" ||
			got.Messages[0].Content != "Responde brevemente." {
			t.Fatalf("system message = %#v", got.Messages[0])
		}
		if got.Messages[1].Role != "user" ||
			got.Messages[1].Content != "Di hola." {
			t.Fatalf("user message = %#v", got.Messages[1])
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"model":                "qwen3:8b",
			"message":              map[string]any{"role": "assistant", "content": "Hola."},
			"done":                 true,
			"done_reason":          "stop",
			"total_duration":       123456,
			"load_duration":        2345,
			"prompt_eval_count":    12,
			"prompt_eval_duration": 3456,
			"eval_count":           3,
			"eval_duration":        4567,
		})
	}))
	defer srv.Close()

	got, err := InvokeOllama(
		context.Background(),
		srv.Client(),
		srv.URL+"/",
		ChatRequest{
			Model:  "qwen3:8b",
			System: "Responde brevemente.",
			Prompt: "Di hola.",
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	if got.Model != "qwen3:8b" ||
		got.Content != "Hola." ||
		!got.Done ||
		got.DoneReason != "stop" {
		t.Fatalf("unexpected result: %+v", got)
	}
	if got.TotalDuration != 123456 ||
		got.PromptEvalCount != 12 ||
		got.EvalCount != 3 {
		t.Fatalf("unexpected metrics: %+v", got)
	}
}

func TestInvokeOllamaRejectsMissingInput(t *testing.T) {
	t.Parallel()

	_, err := InvokeOllama(
		context.Background(),
		http.DefaultClient,
		"http://ollama:11434",
		ChatRequest{},
	)
	if err == nil {
		t.Fatal("empty request was accepted")
	}
}

func TestInvokeOllamaReturnsUpstreamError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "model not found", http.StatusNotFound)
	}))
	defer srv.Close()

	_, err := InvokeOllama(
		context.Background(),
		srv.Client(),
		srv.URL,
		ChatRequest{
			Model:  "missing",
			Prompt: "hola",
		},
	)
	if err == nil {
		t.Fatal("upstream error was ignored")
	}
}
