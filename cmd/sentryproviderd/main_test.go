package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"matrixsentry/providerbroker"
	"matrixsentry/sentry"
)

type fakeCodexSessions struct {
	status providerbroker.CodexSessionStatus
	login  providerbroker.CodexDeviceCodeLogin
	invoke providerbroker.CodexInvokeResult
}

func (f *fakeCodexSessions) Status(
	context.Context,
	sentry.TenantID,
) (providerbroker.CodexSessionStatus, error) {
	return f.status, nil
}

func (f *fakeCodexSessions) StartLogin(
	context.Context,
	sentry.TenantID,
) (providerbroker.CodexDeviceCodeLogin, error) {
	return f.login, nil
}

func (f *fakeCodexSessions) CancelLogin(
	context.Context,
	sentry.TenantID,
	string,
) error {
	return nil
}

func (f *fakeCodexSessions) Logout(
	context.Context,
	sentry.TenantID,
) error {
	return nil
}

func (f *fakeCodexSessions) Invoke(
	context.Context,
	sentry.TenantID,
	providerbroker.CodexInvokeRequest,
) (providerbroker.CodexInvokeResult, error) {
	return f.invoke, nil
}

func TestProviderAPIStatusAndDeviceLogin(t *testing.T) {
	fake := &fakeCodexSessions{
		status: providerbroker.CodexSessionStatus{
			Provider:           "codex",
			State:              providerbroker.StateDisconnected,
			RequiresOpenAIAuth: true,
		},
		login: providerbroker.CodexDeviceCodeLogin{
			Provider:        "codex",
			State:           providerbroker.StateConnecting,
			Type:            "chatgptDeviceCode",
			LoginID:         "login-1",
			VerificationURL: "https://auth.openai.com/codex/device",
			UserCode:        "ABCD-1234",
		},
	}
	server := &apiServer{sessions: fake, token: "internal-token"}
	handler := server.routes()

	request := httptest.NewRequest(
		http.MethodGet,
		"/v1/tenants/1/providers/codex",
		nil,
	)
	request.Header.Set("Authorization", "Bearer internal-token")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status code = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	var status providerbroker.CodexSessionStatus
	if err := json.Unmarshal(recorder.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.State != providerbroker.StateDisconnected || !status.RequiresOpenAIAuth {
		t.Fatalf("status = %+v", status)
	}

	request = httptest.NewRequest(
		http.MethodPost,
		"/v1/tenants/1/providers/codex/login",
		nil,
	)
	request.Header.Set("Authorization", "Bearer internal-token")
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("login code = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	var login providerbroker.CodexDeviceCodeLogin
	if err := json.Unmarshal(recorder.Body.Bytes(), &login); err != nil {
		t.Fatal(err)
	}
	if login.LoginID != "login-1" || login.UserCode != "ABCD-1234" {
		t.Fatalf("login = %+v", login)
	}
}

func TestProviderAPIRejectsUnauthorizedAndInvalidTenant(t *testing.T) {
	server := &apiServer{
		sessions: &fakeCodexSessions{},
		token:    "internal-token",
	}
	handler := server.routes()

	request := httptest.NewRequest(
		http.MethodGet,
		"/v1/tenants/1/providers/codex",
		nil,
	)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized code = %d", recorder.Code)
	}

	request = httptest.NewRequest(
		http.MethodPost,
		"/v1/tenants/invalid/providers/codex/login/cancel",
		bytes.NewBufferString(`{"loginId":"x"}`),
	)
	request.Header.Set("Authorization", "Bearer internal-token")
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("invalid tenant code = %d", recorder.Code)
	}
}

func TestProviderAPIInvokesCodex(t *testing.T) {
	fake := &fakeCodexSessions{
		invoke: providerbroker.CodexInvokeResult{
			Provider:      "codex",
			Model:         "default",
			Content:       "CODEX_HTTP_OK",
			Done:          true,
			DoneReason:    "stop",
			TotalDuration: 10,
		},
	}
	server := &apiServer{sessions: fake, token: "internal-token"}
	handler := server.routes()

	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/tenants/1/providers/codex/invoke",
		bytes.NewBufferString(
			`{"model":"default","system":"brief","prompt":"hello"}`,
		),
	)
	request.Header.Set("Authorization", "Bearer internal-token")
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf(
			"invoke code = %d, body=%s",
			recorder.Code,
			recorder.Body.String(),
		)
	}

	var result providerbroker.CodexInvokeResult
	if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Content != "CODEX_HTTP_OK" || !result.Done {
		t.Fatalf("invoke result = %+v", result)
	}
}
