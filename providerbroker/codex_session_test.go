package providerbroker

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
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
