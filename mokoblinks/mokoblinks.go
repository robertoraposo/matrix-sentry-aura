// Package mokoblinks is a tiny, fire-and-forget client for the MokoBlinks log
// ingestion platform (POST /v1/ingest). It batches lines, never blocks the
// caller, never panics if MokoBlinks is unreachable, and is a no-op unless
// MOKOBLINKS_URL + MOKOBLINKS_API_KEY are set — so the engine can be instrumented
// for live debugging without coupling to the platform or leaking credentials.
package mokoblinks

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"sync"
	"time"
)

// Line is one structured log entry (matches the MokoBlinks ingest schema).
type Line struct {
	Line      string            `json:"line"`
	Level     string            `json:"level"`
	App       string            `json:"app"`
	Host      string            `json:"host"`
	Timestamp string            `json:"timestamp"`
	Meta      map[string]string `json:"meta,omitempty"`
}

// Client batches and ships log lines. The zero value is a safe no-op.
type Client struct {
	url, key, app, host string
	enabled             bool
	batchSize           int
	mu                  sync.Mutex
	buf                 []Line
	// send is the transport; swappable in tests. nil => no-op.
	send func(lines []Line)
}

// FromEnv builds a Client from MOKOBLINKS_URL/API_KEY/APP_NAME/ENABLED. It is
// disabled (no-op) unless URL and API_KEY are both set and ENABLED != "false".
func FromEnv() *Client {
	url := os.Getenv("MOKOBLINKS_URL")
	key := os.Getenv("MOKOBLINKS_API_KEY")
	app := os.Getenv("MOKOBLINKS_APP_NAME")
	if app == "" {
		app = "matrix-sentry"
	}
	host, _ := os.Hostname()
	c := &Client{
		url: url, key: key, app: app, host: host,
		batchSize: 50,
		enabled:   url != "" && key != "" && os.Getenv("MOKOBLINKS_ENABLED") != "false",
	}
	if c.enabled {
		c.send = c.httpSend
	}
	return c
}

// Log buffers a line and flushes (async, fire-and-forget) when the batch fills.
// A no-op when the client is disabled.
func (c *Client) Log(level, msg string, meta map[string]string) {
	if c == nil || !c.enabled {
		return
	}
	c.mu.Lock()
	c.buf = append(c.buf, Line{
		Line: msg, Level: level, App: c.app, Host: c.host,
		Timestamp: time.Now().UTC().Format(time.RFC3339), Meta: meta,
	})
	full := len(c.buf) >= c.batchSize
	c.mu.Unlock()
	if full {
		go c.Flush()
	}
}

func (c *Client) Info(msg string, meta map[string]string)  { c.Log("info", msg, meta) }
func (c *Client) Warn(msg string, meta map[string]string)  { c.Log("warn", msg, meta) }
func (c *Client) Error(msg string, meta map[string]string) { c.Log("error", msg, meta) }
func (c *Client) Debug(msg string, meta map[string]string) { c.Log("debug", msg, meta) }

// Flush ships any buffered lines. Safe to call on a disabled/empty client.
func (c *Client) Flush() {
	if c == nil {
		return
	}
	c.mu.Lock()
	if len(c.buf) == 0 || c.send == nil {
		c.mu.Unlock()
		return
	}
	lines := c.buf
	c.buf = nil
	c.mu.Unlock()
	c.send(lines)
}

func (c *Client) httpSend(lines []Line) {
	body, err := json.Marshal(map[string]any{"lines": lines})
	if err != nil {
		return
	}
	req, err := http.NewRequest("POST", c.url+"/v1/ingest", bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return // fire-and-forget: never crash if MokoBlinks is unreachable
	}
	resp.Body.Close()
}
