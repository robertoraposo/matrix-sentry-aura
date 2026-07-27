package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"matrixsentry/providerbroker"
	"matrixsentry/sentry"
)

type codexSessions interface {
	Status(context.Context, sentry.TenantID) (providerbroker.CodexSessionStatus, error)
	StartLogin(context.Context, sentry.TenantID) (providerbroker.CodexDeviceCodeLogin, error)
	CancelLogin(context.Context, sentry.TenantID, string) error
	Logout(context.Context, sentry.TenantID) error
	Invoke(
		context.Context,
		sentry.TenantID,
		providerbroker.CodexInvokeRequest,
	) (providerbroker.CodexInvokeResult, error)
}

type apiServer struct {
	sessions codexSessions
	token    string
}

func main() {
	httpAddr := flag.String(
		"http",
		envOr("SENTRY_PROVIDERD_HTTP", "127.0.0.1:8811"),
		"internal HTTP listen address",
	)
	root := flag.String(
		"root",
		envOr("SENTRY_PROVIDERD_ROOT", "data/providers"),
		"tenant-isolated provider session root",
	)
	codexBin := flag.String(
		"codex",
		envOr("SENTRY_CODEX_BIN", "codex"),
		"official Codex CLI executable",
	)
	requestTimeout := flag.Duration(
		"timeout",
		envDuration("SENTRY_CODEX_TIMEOUT", 20*time.Second),
		"Codex app-server request timeout",
	)
	invokeTimeout := flag.Duration(
		"invoke-timeout",
		envDuration("SENTRY_CODEX_INVOKE_TIMEOUT", 3*time.Minute),
		"maximum duration for one brokered Codex turn",
	)
	flag.Parse()

	manager, err := providerbroker.NewCodexSessionManager(
		providerbroker.CodexSessionManagerConfig{
			Root:           *root,
			Executable:     *codexBin,
			RequestTimeout: *requestTimeout,
			InvokeTimeout:  *invokeTimeout,
		},
	)
	if err != nil {
		log.Fatal(err)
	}
	defer manager.Close()

	api := &apiServer{
		sessions: manager,
		token:    strings.TrimSpace(os.Getenv("SENTRY_PROVIDERD_TOKEN")),
	}

	server := &http.Server{
		Addr:              *httpAddr,
		Handler:           api.routes(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      *invokeTimeout + 30*time.Second,
		IdleTimeout:       60 * time.Second,
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-stop
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	}()

	log.Printf("sentryproviderd: internal HTTP listening on %s", *httpAddr)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

func (a *apiServer) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	protected := func(handler http.HandlerFunc) http.Handler {
		return a.authorize(handler)
	}

	mux.Handle(
		"GET /v1/tenants/{tenant}/providers/codex",
		protected(a.handleStatus),
	)
	mux.Handle(
		"POST /v1/tenants/{tenant}/providers/codex/login",
		protected(a.handleLogin),
	)
	mux.Handle(
		"POST /v1/tenants/{tenant}/providers/codex/login/cancel",
		protected(a.handleCancelLogin),
	)
	mux.Handle(
		"POST /v1/tenants/{tenant}/providers/codex/logout",
		protected(a.handleLogout),
	)
	mux.Handle(
		"POST /v1/tenants/{tenant}/providers/codex/invoke",
		protected(a.handleInvoke),
	)
	return mux
}

func (a *apiServer) authorize(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.token == "" {
			next(w, r)
			return
		}

		provided := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if len(provided) != len(a.token) ||
			subtle.ConstantTimeCompare([]byte(provided), []byte(a.token)) != 1 {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next(w, r)
	})
}

func (a *apiServer) handleStatus(w http.ResponseWriter, r *http.Request) {
	tenant, ok := tenantFromRequest(w, r)
	if !ok {
		return
	}

	status, err := a.sessions.Status(r.Context(), tenant)
	if err != nil {
		writeError(w, http.StatusBadGateway, "Codex session unavailable")
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (a *apiServer) handleLogin(w http.ResponseWriter, r *http.Request) {
	tenant, ok := tenantFromRequest(w, r)
	if !ok {
		return
	}

	login, err := a.sessions.StartLogin(r.Context(), tenant)
	if err != nil {
		if errors.Is(err, providerbroker.ErrCodexAlreadyConnected) {
			writeError(w, http.StatusConflict, "Codex is already connected")
			return
		}
		writeError(w, http.StatusBadGateway, "unable to start Codex login")
		return
	}
	writeJSON(w, http.StatusOK, login)
}

func (a *apiServer) handleCancelLogin(w http.ResponseWriter, r *http.Request) {
	tenant, ok := tenantFromRequest(w, r)
	if !ok {
		return
	}

	var input struct {
		LoginID string `json:"loginId"`
	}
	if err := decodeJSON(w, r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}

	if err := a.sessions.CancelLogin(r.Context(), tenant, input.LoginID); err != nil {
		if errors.Is(err, providerbroker.ErrCodexNoPendingLogin) {
			writeError(w, http.StatusConflict, "no matching Codex login is pending")
			return
		}
		writeError(w, http.StatusBadGateway, "unable to cancel Codex login")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"provider": "codex",
		"state":    providerbroker.StateDisconnected,
	})
}

func (a *apiServer) handleLogout(w http.ResponseWriter, r *http.Request) {
	tenant, ok := tenantFromRequest(w, r)
	if !ok {
		return
	}

	if err := a.sessions.Logout(r.Context(), tenant); err != nil {
		writeError(w, http.StatusBadGateway, "unable to disconnect Codex")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"provider": "codex",
		"state":    providerbroker.StateDisconnected,
	})
}

func (a *apiServer) handleInvoke(w http.ResponseWriter, r *http.Request) {
	tenant, ok := tenantFromRequest(w, r)
	if !ok {
		return
	}

	var input providerbroker.CodexInvokeRequest
	if err := decodeJSON(w, r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}

	result, err := a.sessions.Invoke(r.Context(), tenant, input)
	if err != nil {
		switch {
		case errors.Is(err, providerbroker.ErrCodexNotConnected):
			writeError(w, http.StatusConflict, "Codex is not connected")
		case errors.Is(err, providerbroker.ErrCodexInputTooLarge):
			writeError(w, http.StatusRequestEntityTooLarge, "Codex invocation input is too large")
		case errors.Is(err, context.DeadlineExceeded):
			writeError(w, http.StatusGatewayTimeout, "Codex invocation timed out")
		default:
			writeError(w, http.StatusBadGateway, "Codex invocation failed")
		}
		return
	}

	writeJSON(w, http.StatusOK, result)
}

func tenantFromRequest(
	w http.ResponseWriter,
	r *http.Request,
) (sentry.TenantID, bool) {
	value, err := strconv.ParseUint(r.PathValue("tenant"), 10, 16)
	if err != nil || value == 0 {
		writeError(w, http.StatusBadRequest, "invalid tenant")
		return 0, false
	}
	return sentry.TenantID(value), true
}

func decodeJSON(w http.ResponseWriter, r *http.Request, out any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("multiple JSON values")
	}
	return nil
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func envOr(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func envDuration(key string, fallback time.Duration) time.Duration {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sentryproviderd: invalid %s, using %s\n", key, fallback)
		return fallback
	}
	return parsed
}
