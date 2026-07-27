package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func stubMCP(t *testing.T, count int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/admin/corpus" {
			http.NotFound(w, r)
			return
		}
		type item struct {
			ID   uint64    `json:"id"`
			Text string    `json:"text"`
			Tags []string  `json:"tags"`
			Vec  []float32 `json:"vec"`
		}
		out := struct {
			Tenant   int    `json:"tenant"`
			Dim      int    `json:"dim"`
			Count    int    `json:"count"`
			Memories []item `json:"memories"`
		}{Tenant: 1, Dim: 4, Count: count}
		for i := 0; i < count; i++ {
			out.Memories = append(out.Memories, item{
				ID: uint64(i + 1), Text: "memory " + strings.Repeat("x", i%3),
				Tags: []string{[]string{"deploy", "auth", "infra"}[i%3]},
				Vec:  []float32{float32(i), float32(i % 4), float32(i % 7), 1},
			})
		}
		json.NewEncoder(w).Encode(out)
	}))
}

func TestAPIGalaxyShape(t *testing.T) {
	mcp := stubMCP(t, 40)
	defer mcp.Close()
	srv := &apiServer{mcpURL: mcp.URL, token: "x", client: http.DefaultClient}
	rec := httptest.NewRecorder()
	srv.handleGalaxy(rec, httptest.NewRequest(http.MethodGet, "/api/galaxy?tenant=personal", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	var d struct {
		Clusters []struct {
			Label  string     `json:"label"`
			Center [3]float32 `json:"center"`
			Count  int        `json:"count"`
		} `json:"clusters"`
		Points []struct {
			Pos          [3]float32 `json:"pos"`
			Text         string     `json:"text"`
			ClusterLabel string     `json:"clusterLabel"`
			Color        string     `json:"color"`
		} `json:"points"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	if len(d.Points) != 40 {
		t.Fatalf("want 40 points, got %d", len(d.Points))
	}
	if len(d.Clusters) == 0 {
		t.Fatal("want clusters")
	}
	if d.Points[0].ClusterLabel == "" || d.Points[0].Color == "" {
		t.Fatal("points must carry clusterLabel + color")
	}
}

func TestAPIGalaxyUpstreamErrorIs502(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer bad.Close()
	srv := &apiServer{mcpURL: bad.URL, token: "x", client: http.DefaultClient}
	rec := httptest.NewRecorder()
	srv.handleGalaxy(rec, httptest.NewRequest(http.MethodGet, "/api/galaxy?tenant=personal", nil))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("upstream 500 → 502, got %d", rec.Code)
	}
}

func TestAPIGalaxyTenantObject(t *testing.T) {
	mcp := stubMCP(t, 10)
	defer mcp.Close()
	srv := &apiServer{mcpURL: mcp.URL, token: "x", client: http.DefaultClient}
	rec := httptest.NewRecorder()
	srv.handleGalaxy(rec, httptest.NewRequest(http.MethodGet, "/api/galaxy?tenant=personal", nil))
	var d struct {
		Tenant struct {
			Key, Name, Glyph, Accent string
		} `json:"tenant"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	if d.Tenant.Key != "personal" || d.Tenant.Name != "Personal" || d.Tenant.Glyph == "" || d.Tenant.Accent == "" {
		t.Fatalf("tenant object not populated: %+v", d.Tenant)
	}
}

func TestAPIJournalPassThrough(t *testing.T) {
	mcp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/admin/journal" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`{"events":[{"seq":7,"ts":1,"type":"Memory","text":"#7 hi"}]}`))
	}))
	defer mcp.Close()
	srv := &apiServer{mcpURL: mcp.URL, token: "x", client: http.DefaultClient}
	rec := httptest.NewRecorder()
	srv.handleJournal(rec, httptest.NewRequest(http.MethodGet, "/api/journal?tenant=personal", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	var d struct {
		Events []struct {
			Seq  int    `json:"seq"`
			Type string `json:"type"`
		} `json:"events"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	if len(d.Events) != 1 || d.Events[0].Seq != 7 || d.Events[0].Type != "Memory" {
		t.Fatalf("passthrough wrong: %+v", d.Events)
	}
}

func TestAPIJournalUpstreamErrorIs502(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	defer bad.Close()
	srv := &apiServer{mcpURL: bad.URL, token: "x", client: http.DefaultClient}
	rec := httptest.NewRecorder()
	srv.handleJournal(rec, httptest.NewRequest(http.MethodGet, "/api/journal?tenant=personal", nil))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("want 502, got %d", rec.Code)
	}
}

func TestAPICommsGroupsByArea(t *testing.T) {
	mcp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/admin/comms" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`{"messages":[
		  {"seq":1,"ts":1,"area":"ASHLEY/COMMS/09-&gt;08","from":"09-voice","kind":"question","text":"q?","ref":0},
		  {"seq":2,"ts":1,"area":"ASHLEY/COMMS/09-&gt;08","from":"08","kind":"answer","text":"a","ref":1},
		  {"seq":3,"ts":1,"area":"ashley/coherence","from":"08","kind":"info","text":"i"}
		]}`))
	}))
	defer mcp.Close()
	srv := &apiServer{mcpURL: mcp.URL, token: "x", client: http.DefaultClient}
	rec := httptest.NewRecorder()
	srv.handleComms(rec, httptest.NewRequest(http.MethodGet, "/api/comms?tenant=personal", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	var d struct {
		Columns []struct {
			Label string `json:"label"`
			Cards []struct {
				Author, Type, TypeColor, Text string
				Promotable                    bool `json:"promotable"`
				Reply                         bool `json:"reply"`
			} `json:"cards"`
		} `json:"columns"`
		Agents []string `json:"agents"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	if len(d.Columns) != 2 {
		t.Fatalf("want 2 area columns, got %d", len(d.Columns))
	}
	foundUnescaped := false
	for _, c := range d.Columns {
		if c.Label == "ASHLEY/COMMS/09->08" {
			foundUnescaped = true
			if len(c.Cards) != 2 {
				t.Fatalf("that area should have 2 cards, got %d", len(c.Cards))
			}
			if c.Cards[0].Type != "pregunta" || c.Cards[0].Promotable {
				t.Fatalf("question card mapping wrong: %+v", c.Cards[0])
			}
			if c.Cards[1].Type != "respuesta" || !c.Cards[1].Reply {
				t.Fatalf("answer card mapping wrong: %+v", c.Cards[1])
			}
		}
	}
	if !foundUnescaped {
		t.Fatal("area label was not HTML-unescaped")
	}
	if len(d.Agents) != 2 {
		t.Fatalf("want 2 distinct agents, got %d: %v", len(d.Agents), d.Agents)
	}
}

func TestAPICommsUpstreamErrorFallsBackEmpty(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	defer bad.Close()
	srv := &apiServer{mcpURL: bad.URL, token: "x", client: http.DefaultClient}
	rec := httptest.NewRecorder()
	srv.handleComms(rec, httptest.NewRequest(http.MethodGet, "/api/comms?tenant=personal", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("UI-safe fallback should still be 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"columns":[]`) {
		t.Fatalf("expected empty fallback, got %s", rec.Body.String())
	}
}

func TestAPIProvidersCallsMCPProviderList(t *testing.T) {
	mcp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/mcp" {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", r.Method)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer secret-token" {
			t.Fatalf("authorization = %q", got)
		}

		var req struct {
			JSONRPC string `json:"jsonrpc"`
			Method  string `json:"method"`
			Params  struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			} `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		if req.JSONRPC != "2.0" ||
			req.Method != "tools/call" ||
			req.Params.Name != "provider_list" {
			t.Fatalf("unexpected MCP request: %+v", req)
		}

		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{
		  "jsonrpc":"2.0",
		  "id":1,
		  "result":{
		    "content":[{"type":"text","text":"providers available: 2"}],
		    "structuredContent":{
		      "count":2,
		      "providers":[
		        {
		          "id":"codex",
		          "name":"Codex CLI",
		          "auth":"cli",
		          "capabilities":["chat","code","tools"],
		          "state":"disconnected",
		          "account":""
		        },
		        {
		          "id":"ollama",
		          "name":"Ollama local",
		          "auth":"none",
		          "capabilities":["chat","embeddings"],
		          "state":"connected",
		          "account":"local"
		        }
		      ]
		    }
		  }
		}`))
	}))
	defer mcp.Close()

	srv := &apiServer{
		mcpURL: mcp.URL,
		token:  "secret-token",
		client: http.DefaultClient,
	}

	rec := httptest.NewRecorder()
	srv.handleProviders(
		rec,
		httptest.NewRequest(http.MethodGet, "/api/providers", nil),
	)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var got struct {
		Count     int `json:"count"`
		Providers []struct {
			ID      string   `json:"id"`
			State   string   `json:"state"`
			Account string   `json:"account"`
			Auth    string   `json:"auth"`
			Caps    []string `json:"capabilities"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}

	if got.Count != 2 || len(got.Providers) != 2 {
		t.Fatalf("unexpected providers response: %+v", got)
	}
	if got.Providers[0].ID != "codex" ||
		got.Providers[1].ID != "ollama" {
		t.Fatalf("provider order/content wrong: %+v", got.Providers)
	}
}

func TestAPIProvidersUpstreamErrorIs502(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer bad.Close()

	srv := &apiServer{
		mcpURL: bad.URL,
		token:  "x",
		client: http.DefaultClient,
	}

	rec := httptest.NewRecorder()
	srv.handleProviders(
		rec,
		httptest.NewRequest(http.MethodGet, "/api/providers", nil),
	)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("want 502, got %d", rec.Code)
	}
}

func TestAPIProviderActionCallsMCPConnect(t *testing.T) {
	mcp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/mcp" {
			http.NotFound(w, r)
			return
		}

		var req struct {
			Method string `json:"method"`
			Params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			} `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		if req.Method != "tools/call" ||
			req.Params.Name != "provider_connect" ||
			req.Params.Arguments["provider"] != "codex" {
			t.Fatalf("unexpected MCP request: %+v", req)
		}

		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{
		  "jsonrpc":"2.0",
		  "id":1,
		  "result":{
		    "structuredContent":{
		      "provider":"codex",
		      "state":"connecting",
		      "type":"chatgptDeviceCode",
		      "loginId":"login-1",
		      "verificationUrl":"https://auth.openai.example/device",
		      "userCode":"ABCD-EFGH"
		    }
		  }
		}`))
	}))
	defer mcp.Close()

	srv := &apiServer{
		mcpURL: mcp.URL,
		token:  "secret-token",
		client: http.DefaultClient,
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(
		http.MethodPost,
		"/api/providers/action",
		strings.NewReader(`{"action":"connect","provider":"codex"}`),
	)
	req.Header.Set("Content-Type", "application/json")
	srv.handleProviderAction(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"userCode":"ABCD-EFGH"`) {
		t.Fatalf("device login missing: %s", rec.Body.String())
	}
}

func TestAPIProviderActionRejectsUnknownAction(t *testing.T) {
	srv := &apiServer{}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(
		http.MethodPost,
		"/api/providers/action",
		strings.NewReader(`{"action":"steal-cookies","provider":"codex"}`),
	)
	srv.handleProviderAction(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", rec.Code)
	}
}
