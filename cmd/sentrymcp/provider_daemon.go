package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"matrixsentry/providerbroker"
	"matrixsentry/sentry"
)

const providerDaemonMaxResponseBytes = 1 << 20

type providerDaemonClient struct {
	baseURL string
	token   string
	client  *http.Client
}

type providerDaemonStatus struct {
	Provider           string               `json:"provider"`
	State              providerbroker.State `json:"state"`
	Account            string               `json:"account"`
	AccountType        string               `json:"accountType,omitempty"`
	PlanType           string               `json:"planType,omitempty"`
	RequiresOpenAIAuth bool                 `json:"requiresOpenaiAuth"`
	LoginID            string               `json:"loginId,omitempty"`
}

type providerDaemonLogin struct {
	Provider        string               `json:"provider"`
	State           providerbroker.State `json:"state"`
	Type            string               `json:"type"`
	LoginID         string               `json:"loginId"`
	VerificationURL string               `json:"verificationUrl"`
	UserCode        string               `json:"userCode"`
}

func newProviderDaemonClient(baseURL, token string) *providerDaemonClient {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		return nil
	}

	return &providerDaemonClient{
		baseURL: baseURL,
		token:   strings.TrimSpace(token),
		client: &http.Client{
			Timeout: 25 * time.Second,
		},
	}
}

func (c *providerDaemonClient) status(
	ctx context.Context,
	tenant sentry.TenantID,
) (providerDaemonStatus, error) {
	var result providerDaemonStatus
	err := c.doJSON(
		ctx,
		http.MethodGet,
		c.codexPath(tenant),
		nil,
		&result,
	)
	return result, err
}

func (c *providerDaemonClient) startLogin(
	ctx context.Context,
	tenant sentry.TenantID,
) (providerDaemonLogin, error) {
	var result providerDaemonLogin
	err := c.doJSON(
		ctx,
		http.MethodPost,
		c.codexPath(tenant)+"/login",
		map[string]any{},
		&result,
	)
	return result, err
}

func (c *providerDaemonClient) cancelLogin(
	ctx context.Context,
	tenant sentry.TenantID,
	loginID string,
) error {
	return c.doJSON(
		ctx,
		http.MethodPost,
		c.codexPath(tenant)+"/login/cancel",
		map[string]string{"loginId": loginID},
		nil,
	)
}

func (c *providerDaemonClient) logout(
	ctx context.Context,
	tenant sentry.TenantID,
) error {
	return c.doJSON(
		ctx,
		http.MethodPost,
		c.codexPath(tenant)+"/logout",
		map[string]any{},
		nil,
	)
}

func (c *providerDaemonClient) codexPath(tenant sentry.TenantID) string {
	return "/v1/tenants/" +
		strconv.FormatUint(uint64(tenant), 10) +
		"/providers/codex"
}

func (c *providerDaemonClient) doJSON(
	ctx context.Context,
	method string,
	path string,
	input any,
	output any,
) error {
	if c == nil {
		return fmt.Errorf("provider session daemon is not configured")
	}

	var body io.Reader
	if input != nil {
		payload, err := json.Marshal(input)
		if err != nil {
			return fmt.Errorf("encode provider daemon request: %w", err)
		}
		body = bytes.NewReader(payload)
	}

	req, err := http.NewRequestWithContext(
		ctx,
		method,
		c.baseURL+path,
		body,
	)
	if err != nil {
		return fmt.Errorf("create provider daemon request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if input != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("provider session daemon unavailable: %w", err)
	}
	defer resp.Body.Close()

	limited := io.LimitReader(resp.Body, providerDaemonMaxResponseBytes)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var remote struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(limited).Decode(&remote)
		message := strings.TrimSpace(remote.Error)
		if message == "" {
			message = http.StatusText(resp.StatusCode)
		}
		return fmt.Errorf(
			"provider session daemon returned %d: %s",
			resp.StatusCode,
			message,
		)
	}

	if output == nil {
		_, _ = io.Copy(io.Discard, limited)
		return nil
	}
	if err := json.NewDecoder(limited).Decode(output); err != nil {
		return fmt.Errorf("decode provider daemon response: %w", err)
	}
	return nil
}
