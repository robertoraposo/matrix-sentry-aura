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
		if r.URL.Path != "/admin/comms" { http.NotFound(w, r); return }
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
	if rec.Code != http.StatusOK { t.Fatalf("want 200, got %d", rec.Code) }
	var d struct {
		Columns []struct {
			Label string `json:"label"`
			Cards []struct {
				Author, Type, TypeColor, Text string
				Promotable bool `json:"promotable"`
				Reply      bool `json:"reply"`
			} `json:"cards"`
		} `json:"columns"`
		Agents []string `json:"agents"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil { t.Fatal(err) }
	if len(d.Columns) != 2 { t.Fatalf("want 2 area columns, got %d", len(d.Columns)) }
	foundUnescaped := false
	for _, c := range d.Columns {
		if c.Label == "ASHLEY/COMMS/09->08" {
			foundUnescaped = true
			if len(c.Cards) != 2 { t.Fatalf("that area should have 2 cards, got %d", len(c.Cards)) }
			if c.Cards[0].Type != "pregunta" || c.Cards[0].Promotable { t.Fatalf("question card mapping wrong: %+v", c.Cards[0]) }
			if c.Cards[1].Type != "respuesta" || !c.Cards[1].Reply { t.Fatalf("answer card mapping wrong: %+v", c.Cards[1]) }
		}
	}
	if !foundUnescaped { t.Fatal("area label was not HTML-unescaped") }
	if len(d.Agents) != 2 { t.Fatalf("want 2 distinct agents, got %d: %v", len(d.Agents), d.Agents) }
}

func TestAPICommsUpstreamErrorFallsBackEmpty(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	defer bad.Close()
	srv := &apiServer{mcpURL: bad.URL, token: "x", client: http.DefaultClient}
	rec := httptest.NewRecorder()
	srv.handleComms(rec, httptest.NewRequest(http.MethodGet, "/api/comms?tenant=personal", nil))
	if rec.Code != http.StatusOK { t.Fatalf("UI-safe fallback should still be 200, got %d", rec.Code) }
	if !strings.Contains(rec.Body.String(), `"columns":[]`) { t.Fatalf("expected empty fallback, got %s", rec.Body.String()) }
}
