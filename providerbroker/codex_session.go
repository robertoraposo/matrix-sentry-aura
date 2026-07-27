package providerbroker

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"matrixsentry/sentry"
)

const maxCodexMessageBytes = 8 << 20

var (
	ErrCodexClosed           = errors.New("providerbroker: Codex app-server is closed")
	ErrCodexAlreadyConnected = errors.New("providerbroker: Codex account is already connected")
	ErrCodexNoPendingLogin   = errors.New("providerbroker: no matching Codex login is pending")
	ErrCodexInvalidTenant    = errors.New("providerbroker: invalid tenant")
)

type CodexCommandFactory func(context.Context, string, ...string) *exec.Cmd

type CodexAppServerConfig struct {
	Executable          string
	Home                string
	RequestTimeout      time.Duration
	CommandFactory      CodexCommandFactory
	NotificationHandler func(string, json.RawMessage)
}

type codexRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type codexRPCMessage struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *codexRPCError  `json:"error,omitempty"`
}

type codexRPCResult struct {
	result json.RawMessage
	rpcErr *codexRPCError
	err    error
}

type CodexAppServer struct {
	cmd            *exec.Cmd
	stdin          io.WriteCloser
	cancel         context.CancelFunc
	requestTimeout time.Duration
	onNotification func(string, json.RawMessage)

	nextID atomic.Int64
	write  sync.Mutex

	pendingMu sync.Mutex
	pending   map[int64]chan codexRPCResult
	deadErr   error
	deadOnce  sync.Once
	done      chan struct{}
}

func StartCodexAppServer(
	ctx context.Context,
	cfg CodexAppServerConfig,
) (*CodexAppServer, error) {
	executable := strings.TrimSpace(cfg.Executable)
	if executable == "" {
		executable = "codex"
	}

	home := strings.TrimSpace(cfg.Home)
	if home == "" {
		return nil, errors.New("providerbroker: Codex home is required")
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		return nil, fmt.Errorf("providerbroker: create Codex home: %w", err)
	}
	if err := os.Chmod(home, 0o700); err != nil {
		return nil, fmt.Errorf("providerbroker: secure Codex home: %w", err)
	}

	requestTimeout := cfg.RequestTimeout
	if requestTimeout <= 0 {
		requestTimeout = 20 * time.Second
	}

	processCtx, cancel := context.WithCancel(context.Background())
	factory := cfg.CommandFactory
	if factory == nil {
		factory = exec.CommandContext
	}

	cmd := factory(processCtx, executable, "app-server", "--stdio")
	cmd.Env = codexEnvironment(cmd.Env, home)
	cmd.Stderr = io.Discard

	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("providerbroker: Codex stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("providerbroker: Codex stdout: %w", err)
	}
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("providerbroker: start Codex app-server: %w", err)
	}

	client := &CodexAppServer{
		cmd:            cmd,
		stdin:          stdin,
		cancel:         cancel,
		requestTimeout: requestTimeout,
		onNotification: cfg.NotificationHandler,
		pending:        make(map[int64]chan codexRPCResult),
		done:           make(chan struct{}),
	}

	go client.readLoop(stdout)
	go func() {
		client.markDead(cmd.Wait())
	}()

	initCtx, initCancel := context.WithTimeout(ctx, requestTimeout)
	defer initCancel()

	var initialized struct {
		CodexHome      string `json:"codexHome"`
		PlatformFamily string `json:"platformFamily"`
		PlatformOS     string `json:"platformOs"`
	}
	if err := client.call(initCtx, "initialize", map[string]any{
		"clientInfo": map[string]any{
			"name":    "matrix_sentry",
			"title":   "Matrix Sentry Provider Broker",
			"version": "0.1.0",
		},
	}, &initialized); err != nil {
		client.Close()
		return nil, fmt.Errorf("providerbroker: initialize Codex app-server: %w", err)
	}
	if err := client.notify("initialized", map[string]any{}); err != nil {
		client.Close()
		return nil, fmt.Errorf("providerbroker: acknowledge Codex initialization: %w", err)
	}

	return client, nil
}

func codexEnvironment(base []string, home string) []string {
	if base == nil {
		base = os.Environ()
	}

	filtered := make([]string, 0, len(base)+3)
	for _, item := range base {
		key, _, ok := strings.Cut(item, "=")
		if ok && (key == "CODEX_HOME" || key == "RUST_LOG" || key == "LOG_FORMAT") {
			continue
		}
		filtered = append(filtered, item)
	}

	return append(
		filtered,
		"CODEX_HOME="+home,
		"RUST_LOG=warn",
		"LOG_FORMAT=json",
	)
}

func (c *CodexAppServer) readLoop(r io.Reader) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64<<10), maxCodexMessageBytes)

	for scanner.Scan() {
		var message codexRPCMessage
		if err := json.Unmarshal(scanner.Bytes(), &message); err != nil {
			continue
		}

		if len(message.ID) > 0 &&
			string(message.ID) != "null" &&
			message.Method != "" {
			c.rejectServerRequest(message)
			continue
		}

		if len(message.ID) > 0 && string(message.ID) != "null" {
			var id int64
			if err := json.Unmarshal(message.ID, &id); err != nil {
				continue
			}

			c.pendingMu.Lock()
			ch := c.pending[id]
			delete(c.pending, id)
			c.pendingMu.Unlock()

			if ch != nil {
				ch <- codexRPCResult{
					result: message.Result,
					rpcErr: message.Error,
				}
			}
			continue
		}

		if message.Method != "" && c.onNotification != nil {
			c.onNotification(message.Method, append(json.RawMessage(nil), message.Params...))
		}
	}

	if err := scanner.Err(); err != nil {
		c.markDead(fmt.Errorf("providerbroker: read Codex app-server: %w", err))
	}
}

func (c *CodexAppServer) markDead(err error) {
	c.deadOnce.Do(func() {
		if err == nil {
			err = ErrCodexClosed
		}

		c.pendingMu.Lock()
		c.deadErr = err
		pending := c.pending
		c.pending = make(map[int64]chan codexRPCResult)
		c.pendingMu.Unlock()

		for _, ch := range pending {
			ch <- codexRPCResult{err: err}
		}
		close(c.done)
	})
}

func (c *CodexAppServer) call(
	ctx context.Context,
	method string,
	params any,
	out any,
) error {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.requestTimeout)
		defer cancel()
	}

	id := c.nextID.Add(1)
	ch := make(chan codexRPCResult, 1)

	c.pendingMu.Lock()
	if c.deadErr != nil {
		err := c.deadErr
		c.pendingMu.Unlock()
		return err
	}
	c.pending[id] = ch
	c.pendingMu.Unlock()

	message := map[string]any{
		"id":     id,
		"method": method,
	}
	if params != nil {
		message["params"] = params
	}

	if err := c.writeMessage(message); err != nil {
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
		return err
	}

	select {
	case result := <-ch:
		if result.err != nil {
			return result.err
		}
		if result.rpcErr != nil {
			return fmt.Errorf(
				"providerbroker: Codex RPC %s failed (%d): %s",
				method,
				result.rpcErr.Code,
				result.rpcErr.Message,
			)
		}
		if out == nil || len(result.result) == 0 || string(result.result) == "null" {
			return nil
		}
		if err := json.Unmarshal(result.result, out); err != nil {
			return fmt.Errorf("providerbroker: decode Codex RPC %s: %w", method, err)
		}
		return nil

	case <-ctx.Done():
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
		return ctx.Err()

	case <-c.done:
		c.pendingMu.Lock()
		err := c.deadErr
		c.pendingMu.Unlock()
		if err == nil {
			err = ErrCodexClosed
		}
		return err
	}
}

func (c *CodexAppServer) notify(method string, params any) error {
	message := map[string]any{"method": method}
	if params != nil {
		message["params"] = params
	}
	return c.writeMessage(message)
}

func (c *CodexAppServer) writeMessage(message any) error {
	payload, err := json.Marshal(message)
	if err != nil {
		return fmt.Errorf("providerbroker: encode Codex message: %w", err)
	}
	payload = append(payload, '\n')

	c.write.Lock()
	defer c.write.Unlock()

	if _, err := c.stdin.Write(payload); err != nil {
		return fmt.Errorf("providerbroker: write Codex app-server: %w", err)
	}
	return nil
}

func (c *CodexAppServer) Close() error {
	if c == nil {
		return nil
	}

	c.cancel()
	if c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}

	select {
	case <-c.done:
	case <-time.After(2 * time.Second):
	}
	return nil
}

type CodexAccount struct {
	Type             string  `json:"type"`
	Email            *string `json:"email,omitempty"`
	PlanType         *string `json:"planType,omitempty"`
	CredentialSource string  `json:"credentialSource,omitempty"`
}

type CodexAccountReadResult struct {
	Account            *CodexAccount `json:"account"`
	RequiresOpenAIAuth bool          `json:"requiresOpenaiAuth"`
}

func (c *CodexAppServer) AccountRead(
	ctx context.Context,
) (CodexAccountReadResult, error) {
	var result CodexAccountReadResult
	err := c.call(ctx, "account/read", map[string]any{
		"refreshToken": false,
	}, &result)
	return result, err
}

type CodexDeviceCodeLogin struct {
	Provider        string `json:"provider"`
	State           State  `json:"state"`
	Type            string `json:"type"`
	LoginID         string `json:"loginId"`
	VerificationURL string `json:"verificationUrl"`
	UserCode        string `json:"userCode"`
}

func (c *CodexAppServer) StartDeviceCodeLogin(
	ctx context.Context,
) (CodexDeviceCodeLogin, error) {
	var result CodexDeviceCodeLogin
	err := c.call(ctx, "account/login/start", map[string]any{
		"type": "chatgptDeviceCode",
	}, &result)
	if err != nil {
		return CodexDeviceCodeLogin{}, err
	}
	result.Provider = "codex"
	result.State = StateConnecting
	return result, nil
}

func (c *CodexAppServer) CancelLogin(
	ctx context.Context,
	loginID string,
) error {
	return c.call(ctx, "account/login/cancel", map[string]any{
		"loginId": loginID,
	}, nil)
}

func (c *CodexAppServer) Logout(ctx context.Context) error {
	return c.call(ctx, "account/logout", nil, nil)
}

type CodexSessionStatus struct {
	Provider           string `json:"provider"`
	State              State  `json:"state"`
	Account            string `json:"account"`
	AccountType        string `json:"accountType,omitempty"`
	PlanType           string `json:"planType,omitempty"`
	RequiresOpenAIAuth bool   `json:"requiresOpenaiAuth"`
	LoginID            string `json:"loginId,omitempty"`
}

type CodexSessionManagerConfig struct {
	Root           string
	Executable     string
	RequestTimeout time.Duration
	InvokeTimeout  time.Duration
	CommandFactory CodexCommandFactory
}

type managedCodexSession struct {
	opMu     sync.Mutex
	invokeMu sync.Mutex
	mu       sync.Mutex

	client     *CodexAppServer
	pending    *CodexDeviceCodeLogin
	completed  map[string]struct{}
	invocation *codexInvocation
}

type CodexSessionManager struct {
	mu       sync.Mutex
	closed   bool
	sessions map[sentry.TenantID]*managedCodexSession
	cfg      CodexSessionManagerConfig
}

func NewCodexSessionManager(
	cfg CodexSessionManagerConfig,
) (*CodexSessionManager, error) {
	root := strings.TrimSpace(cfg.Root)
	if root == "" {
		return nil, errors.New("providerbroker: provider session root is required")
	}

	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("providerbroker: resolve provider session root: %w", err)
	}
	if err := os.MkdirAll(absoluteRoot, 0o700); err != nil {
		return nil, fmt.Errorf("providerbroker: create provider session root: %w", err)
	}
	if err := os.Chmod(absoluteRoot, 0o700); err != nil {
		return nil, fmt.Errorf("providerbroker: secure provider session root: %w", err)
	}

	cfg.Root = absoluteRoot
	if strings.TrimSpace(cfg.Executable) == "" {
		cfg.Executable = "codex"
	}
	if cfg.RequestTimeout <= 0 {
		cfg.RequestTimeout = 20 * time.Second
	}
	if cfg.InvokeTimeout <= 0 {
		cfg.InvokeTimeout = 3 * time.Minute
	}

	return &CodexSessionManager{
		sessions: make(map[sentry.TenantID]*managedCodexSession),
		cfg:      cfg,
	}, nil
}

func (m *CodexSessionManager) session(
	ctx context.Context,
	tenant sentry.TenantID,
) (*managedCodexSession, error) {
	if tenant == 0 {
		return nil, ErrCodexInvalidTenant
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, ErrCodexClosed
	}
	if existing := m.sessions[tenant]; existing != nil {
		m.mu.Unlock()
		return existing, nil
	}
	m.mu.Unlock()

	home := filepath.Join(
		m.cfg.Root,
		fmt.Sprintf("tenant-%d", tenant),
		"codex",
	)
	if err := ensureCodexBrokerProfile(home); err != nil {
		return nil, err
	}
	created := &managedCodexSession{}

	client, err := StartCodexAppServer(ctx, CodexAppServerConfig{
		Executable:     m.cfg.Executable,
		Home:           home,
		RequestTimeout: m.cfg.RequestTimeout,
		CommandFactory: m.cfg.CommandFactory,
		NotificationHandler: func(method string, params json.RawMessage) {
			created.handleNotification(method, params)
		},
	})
	if err != nil {
		return nil, err
	}
	created.client = client

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		client.Close()
		return nil, ErrCodexClosed
	}
	if existing := m.sessions[tenant]; existing != nil {
		client.Close()
		return existing, nil
	}
	m.sessions[tenant] = created
	return created, nil
}

func (s *managedCodexSession) handleNotification(
	method string,
	params json.RawMessage,
) {
	s.handleInvocationNotification(method, params)

	if method != "account/login/completed" {
		return
	}

	var completed struct {
		LoginID string `json:"loginId"`
	}
	if err := json.Unmarshal(params, &completed); err != nil {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pending != nil && s.pending.LoginID == completed.LoginID {
		s.pending = nil
		return
	}
	if completed.LoginID != "" {
		if s.completed == nil {
			s.completed = make(map[string]struct{})
		}
		s.completed[completed.LoginID] = struct{}{}
	}
}

func (m *CodexSessionManager) Status(
	ctx context.Context,
	tenant sentry.TenantID,
) (CodexSessionStatus, error) {
	session, err := m.session(ctx, tenant)
	if err != nil {
		return CodexSessionStatus{}, err
	}

	account, err := session.client.AccountRead(ctx)
	if err != nil {
		return CodexSessionStatus{}, err
	}

	status := CodexSessionStatus{
		Provider:           "codex",
		State:              StateDisconnected,
		RequiresOpenAIAuth: account.RequiresOpenAIAuth,
	}
	if account.Account != nil {
		status.State = StateConnected
		status.AccountType = account.Account.Type
		status.Account = account.Account.Type
		if account.Account.Email != nil && strings.TrimSpace(*account.Account.Email) != "" {
			status.Account = strings.TrimSpace(*account.Account.Email)
		}
		if account.Account.PlanType != nil {
			status.PlanType = strings.TrimSpace(*account.Account.PlanType)
		}
		return status, nil
	}

	session.mu.Lock()
	if session.pending != nil {
		status.State = StateConnecting
		status.LoginID = session.pending.LoginID
	}
	session.mu.Unlock()

	return status, nil
}

func (m *CodexSessionManager) StartLogin(
	ctx context.Context,
	tenant sentry.TenantID,
) (CodexDeviceCodeLogin, error) {
	session, err := m.session(ctx, tenant)
	if err != nil {
		return CodexDeviceCodeLogin{}, err
	}

	session.opMu.Lock()
	defer session.opMu.Unlock()

	session.mu.Lock()
	if session.pending != nil {
		pending := *session.pending
		session.mu.Unlock()
		return pending, nil
	}
	session.mu.Unlock()

	account, err := session.client.AccountRead(ctx)
	if err != nil {
		return CodexDeviceCodeLogin{}, err
	}
	if account.Account != nil {
		return CodexDeviceCodeLogin{}, ErrCodexAlreadyConnected
	}

	login, err := session.client.StartDeviceCodeLogin(ctx)
	if err != nil {
		return CodexDeviceCodeLogin{}, err
	}
	if strings.TrimSpace(login.LoginID) == "" ||
		strings.TrimSpace(login.VerificationURL) == "" ||
		strings.TrimSpace(login.UserCode) == "" {
		return CodexDeviceCodeLogin{}, errors.New(
			"providerbroker: Codex returned an incomplete device-code login",
		)
	}

	session.mu.Lock()
	if _, completed := session.completed[login.LoginID]; completed {
		delete(session.completed, login.LoginID)
	} else {
		session.pending = &login
	}
	session.mu.Unlock()
	return login, nil
}

func (m *CodexSessionManager) CancelLogin(
	ctx context.Context,
	tenant sentry.TenantID,
	loginID string,
) error {
	session, err := m.session(ctx, tenant)
	if err != nil {
		return err
	}

	loginID = strings.TrimSpace(loginID)
	if loginID == "" {
		return ErrCodexNoPendingLogin
	}

	session.opMu.Lock()
	defer session.opMu.Unlock()

	session.mu.Lock()
	pending := session.pending
	if pending == nil || pending.LoginID != loginID {
		session.mu.Unlock()
		return ErrCodexNoPendingLogin
	}
	session.mu.Unlock()

	if err := session.client.CancelLogin(ctx, loginID); err != nil {
		return err
	}

	session.mu.Lock()
	if session.pending != nil && session.pending.LoginID == loginID {
		session.pending = nil
	}
	session.mu.Unlock()
	return nil
}

func (m *CodexSessionManager) Logout(
	ctx context.Context,
	tenant sentry.TenantID,
) error {
	session, err := m.session(ctx, tenant)
	if err != nil {
		return err
	}

	session.opMu.Lock()
	defer session.opMu.Unlock()

	if err := session.client.Logout(ctx); err != nil {
		return err
	}

	session.mu.Lock()
	session.pending = nil
	session.mu.Unlock()
	return nil
}

func (m *CodexSessionManager) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	sessions := make([]*managedCodexSession, 0, len(m.sessions))
	for _, session := range m.sessions {
		sessions = append(sessions, session)
	}
	m.sessions = nil
	m.mu.Unlock()

	for _, session := range sessions {
		_ = session.client.Close()
	}
	return nil
}
