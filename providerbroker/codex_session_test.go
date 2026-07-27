package providerbroker

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"matrixsentry/sentry"
)

func TestCodexAppServerHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_CODEX_HELPER") != "1" {
		return
	}

	home := os.Getenv("CODEX_HOME")
	scanner := bufio.NewScanner(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)

	for scanner.Scan() {
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &request); err != nil {
			continue
		}
		if len(request.ID) == 0 {
			continue
		}

		var id int64
		_ = json.Unmarshal(request.ID, &id)

		switch request.Method {
		case "initialize":
			_ = encoder.Encode(map[string]any{
				"id": id,
				"result": map[string]any{
					"codexHome":      home,
					"platformFamily": "unix",
					"platformOs":     "test",
				},
			})

		case "thread/start":
			var params struct {
				Ephemeral      bool   `json:"ephemeral"`
				ApprovalPolicy string `json:"approvalPolicy"`
				Sandbox        string `json:"sandbox"`
			}
			_ = json.Unmarshal(request.Params, &params)
			if !params.Ephemeral ||
				params.ApprovalPolicy != "never" ||
				params.Sandbox != "read-only" {
				_ = encoder.Encode(map[string]any{
					"id": id,
					"error": map[string]any{
						"code":    -32602,
						"message": "unsafe thread settings",
					},
				})
				continue
			}
			_ = encoder.Encode(map[string]any{
				"id": id,
				"result": map[string]any{
					"thread": map[string]any{
						"id":    "thread-test",
						"model": "codex-test-model",
					},
				},
			})

		case "turn/start":
			_ = encoder.Encode(map[string]any{
				"id": id,
				"result": map[string]any{
					"turn": map[string]any{
						"id":     "turn-test",
						"status": "inProgress",
					},
				},
			})
			_ = encoder.Encode(map[string]any{
				"method": "item/agentMessage/delta",
				"params": map[string]any{
					"threadId": "thread-test",
					"turnId":   "turn-test",
					"itemId":   "message-test",
					"delta":    "CODEX_",
				},
			})
			_ = encoder.Encode(map[string]any{
				"method": "item/completed",
				"params": map[string]any{
					"threadId": "thread-test",
					"turnId":   "turn-test",
					"item": map[string]any{
						"type": "agentMessage",
						"id":   "message-test",
						"text": "CODEX_INVOKE_OK",
					},
				},
			})
			_ = encoder.Encode(map[string]any{
				"method": "turn/completed",
				"params": map[string]any{
					"threadId": "thread-test",
					"turn": map[string]any{
						"id":     "turn-test",
						"status": "completed",
						"items": []any{
							map[string]any{
								"type": "agentMessage",
								"id":   "message-test",
								"text": "CODEX_INVOKE_OK",
							},
						},
					},
				},
			})

		case "thread/unsubscribe", "thread/delete", "turn/interrupt":
			_ = encoder.Encode(map[string]any{
				"id":     id,
				"result": map[string]any{},
			})

		case "account/read":
			account := any(nil)
			if _, err := os.Stat(filepath.Join(home, "connected")); err == nil {
				account = map[string]any{
					"type":     "chatgpt",
					"email":    "owner@example.com",
					"planType": "plus",
				}
			}
			_ = encoder.Encode(map[string]any{
				"id": id,
				"result": map[string]any{
					"account":            account,
					"requiresOpenaiAuth": true,
				},
			})

		case "account/login/start":
			_ = encoder.Encode(map[string]any{
				"id": id,
				"result": map[string]any{
					"type":            "chatgptDeviceCode",
					"loginId":         "login-test",
					"verificationUrl": "https://auth.openai.com/codex/device",
					"userCode":        "ABCD-1234",
				},
			})

		case "account/login/cancel":
			_ = encoder.Encode(map[string]any{"id": id, "result": map[string]any{}})

		case "account/logout":
			_ = os.Remove(filepath.Join(home, "connected"))
			_ = encoder.Encode(map[string]any{"id": id, "result": map[string]any{}})
		}
	}

	os.Exit(0)
}

func codexHelperFactory(
	ctx context.Context,
	_ string,
	_ ...string,
) *exec.Cmd {
	cmd := exec.CommandContext(
		ctx,
		os.Args[0],
		"-test.run=TestCodexAppServerHelperProcess",
		"--",
	)
	cmd.Env = append(os.Environ(), "GO_WANT_CODEX_HELPER=1")
	return cmd
}

func TestCodexSessionManagerDeviceLoginAndIsolation(t *testing.T) {
	root := t.TempDir()
	manager, err := NewCodexSessionManager(CodexSessionManagerConfig{
		Root:           root,
		Executable:     "codex-test",
		RequestTimeout: 2 * time.Second,
		CommandFactory: codexHelperFactory,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()

	ctx := context.Background()
	tenantA := sentry.TenantID(1)
	tenantB := sentry.TenantID(2)

	status, err := manager.Status(ctx, tenantA)
	if err != nil {
		t.Fatal(err)
	}
	if status.State != StateDisconnected || !status.RequiresOpenAIAuth {
		t.Fatalf("initial status = %+v", status)
	}

	login, err := manager.StartLogin(ctx, tenantA)
	if err != nil {
		t.Fatal(err)
	}
	if login.State != StateConnecting ||
		login.LoginID != "login-test" ||
		login.UserCode != "ABCD-1234" {
		t.Fatalf("login = %+v", login)
	}

	status, err = manager.Status(ctx, tenantA)
	if err != nil {
		t.Fatal(err)
	}
	if status.State != StateConnecting || status.LoginID != "login-test" {
		t.Fatalf("connecting status = %+v", status)
	}

	other, err := manager.Status(ctx, tenantB)
	if err != nil {
		t.Fatal(err)
	}
	if other.State != StateDisconnected || other.LoginID != "" {
		t.Fatalf("tenant B inherited tenant A login: %+v", other)
	}

	if err := manager.CancelLogin(ctx, tenantA, login.LoginID); err != nil {
		t.Fatal(err)
	}
	status, err = manager.Status(ctx, tenantA)
	if err != nil {
		t.Fatal(err)
	}
	if status.State != StateDisconnected {
		t.Fatalf("status after cancel = %+v", status)
	}

	home := filepath.Join(root, "tenant-1", "codex")
	if err := os.WriteFile(filepath.Join(home, "connected"), []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	status, err = manager.Status(ctx, tenantA)
	if err != nil {
		t.Fatal(err)
	}
	if status.State != StateConnected ||
		status.Account != "owner@example.com" ||
		status.AccountType != "chatgpt" ||
		status.PlanType != "plus" {
		t.Fatalf("connected status = %+v", status)
	}

	if err := manager.Logout(ctx, tenantA); err != nil {
		t.Fatal(err)
	}
	status, err = manager.Status(ctx, tenantA)
	if err != nil {
		t.Fatal(err)
	}
	if status.State != StateDisconnected {
		t.Fatalf("status after logout = %+v", status)
	}

	info, err := os.Stat(home)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("Codex home mode = %o, want 700", info.Mode().Perm())
	}
}

func TestCodexSessionManagerInvokeTextOnly(t *testing.T) {
	root := t.TempDir()
	manager, err := NewCodexSessionManager(CodexSessionManagerConfig{
		Root:           root,
		Executable:     "codex-test",
		RequestTimeout: 2 * time.Second,
		InvokeTimeout:  5 * time.Second,
		CommandFactory: codexHelperFactory,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()

	tenant := sentry.TenantID(7)
	ctx := context.Background()

	if _, err := manager.Status(ctx, tenant); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(root, "tenant-7", "codex")
	if err := os.WriteFile(
		filepath.Join(home, "connected"),
		[]byte("ok"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}

	result, err := manager.Invoke(ctx, tenant, CodexInvokeRequest{
		Model:  "default",
		System: "Responde brevemente.",
		Prompt: "Devuelve la marca solicitada.",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Provider != "codex" ||
		result.Model != "codex-test-model" ||
		result.Content != "CODEX_INVOKE_OK" ||
		!result.Done ||
		result.DoneReason != "stop" {
		t.Fatalf("unexpected invoke result: %+v", result)
	}

	configPath := filepath.Join(home, "config.toml")
	config, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		"shell_tool = false",
		"unified_exec = false",
		"web_search = false",
	} {
		if !strings.Contains(string(config), expected) {
			t.Fatalf("hardened config missing %q:\n%s", expected, config)
		}
	}
	info, err := os.Stat(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("config mode = %o, want 600", info.Mode().Perm())
	}
}

func TestCodexSessionManagerInvokeRequiresConnectedAccount(t *testing.T) {
	manager, err := NewCodexSessionManager(CodexSessionManagerConfig{
		Root:           t.TempDir(),
		Executable:     "codex-test",
		RequestTimeout: 2 * time.Second,
		InvokeTimeout:  5 * time.Second,
		CommandFactory: codexHelperFactory,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()

	_, err = manager.Invoke(
		context.Background(),
		sentry.TenantID(1),
		CodexInvokeRequest{
			Model:  "default",
			Prompt: "hola",
		},
	)
	if !errors.Is(err, ErrCodexNotConnected) {
		t.Fatalf("error = %v, want ErrCodexNotConnected", err)
	}
}
