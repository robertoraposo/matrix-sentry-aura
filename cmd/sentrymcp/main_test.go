package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"matrixsentry/mokoblinks"
	"matrixsentry/sentry"
)

func newTestServer(t *testing.T) *server {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "sentry")
	store, err := sentry.Open(dir, sentry.Options{FsyncEvery: 0})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	reg, err := sentry.NewRegistry(store)
	if err != nil {
		t.Fatal(err)
	}
	return &server{store: store, reg: reg, moko: mokoblinks.FromEnv(), tenant: 1}
}

func callRecord(t *testing.T, s *server, args map[string]any) rpcResp {
	t.Helper()
	params, _ := json.Marshal(map[string]any{"name": "record_access", "arguments": args})
	return s.callTool(rpcReq{ID: json.RawMessage("1"), Params: params})
}

func respText(t *testing.T, r rpcResp) string {
	t.Helper()
	m, ok := r.Result.(map[string]any)
	if !ok {
		t.Fatalf("result not a map: %#v", r.Result)
	}
	if m["isError"] == true {
		t.Fatalf("tool returned error: %#v", m["content"])
	}
	content := m["content"].([]map[string]any)
	return content[0]["text"].(string)
}

func accessItems(t *testing.T, s *server) []sentry.AccessPayload {
	t.Helper()
	tenant := s.tenant
	etype := sentry.EventAccess
	var out []sentry.AccessPayload
	s.store.Scan(sentry.Filter{Tenant: &tenant, Type: &etype}, func(r sentry.Record) bool {
		var p sentry.AccessPayload
		sentry.UnmarshalPayload(r.Payload, &p)
		out = append(out, p)
		return true
	})
	return out
}

func TestRecordAccessByPath(t *testing.T) {
	s := newTestServer(t)
	resp := callRecord(t, s, map[string]any{"path": "/repo/a.go", "src": "Read"})
	respText(t, resp) // asserts non-error

	items := accessItems(t, s)
	if len(items) != 1 {
		t.Fatalf("expected 1 access, got %d", len(items))
	}
	if items[0].ItemID != 1 || items[0].Source != "Read" {
		t.Errorf("access = %+v", items[0])
	}
}

func TestRecordAccessBatch(t *testing.T) {
	s := newTestServer(t)
	resp := callRecord(t, s, map[string]any{
		"paths": []any{"/repo/a.go", "/repo/b.go", "/repo/a.go"},
		"src":   "Bash",
	})
	respText(t, resp)

	items := accessItems(t, s)
	if len(items) != 3 {
		t.Fatalf("expected 3 accesses, got %d", len(items))
	}
	if items[0].ItemID != 1 || items[1].ItemID != 2 || items[2].ItemID != 1 {
		t.Errorf("expected ids 1,2,1; got %d,%d,%d", items[0].ItemID, items[1].ItemID, items[2].ItemID)
	}
}

func TestRecordAccessByItemBackCompat(t *testing.T) {
	s := newTestServer(t)
	resp := callRecord(t, s, map[string]any{"item": float64(5)})
	txt := respText(t, resp)
	if !strings.Contains(txt, "5") {
		t.Errorf("expected response to mention item 5, got %q", txt)
	}
	items := accessItems(t, s)
	if len(items) != 1 || items[0].ItemID != 5 {
		t.Errorf("expected single access item 5, got %+v", items)
	}
}
