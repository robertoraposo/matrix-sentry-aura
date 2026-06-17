package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// apiServer fetches a tenant's corpus from the MCP (server-side, with the
// bearer) and serves it projected for the dashboard's galaxy.
type apiServer struct {
	mcpURL string
	token  string
	client *http.Client
}

func newAPIServer(mcpURL, token string) *apiServer {
	return &apiServer{mcpURL: mcpURL, token: token, client: &http.Client{Timeout: 30 * time.Second}}
}

type corpusItem struct {
	ID   uint64    `json:"id"`
	Text string    `json:"text"`
	Tags []string  `json:"tags"`
	Src  string    `json:"src"`
	Vec  []float32 `json:"vec"`
}
type corpusResp struct {
	Tenant   int          `json:"tenant"`
	Dim      int          `json:"dim"`
	Count    int          `json:"count"`
	Memories []corpusItem `json:"memories"`
}

// palette mirrors the dashboard's cluster hues.
var palette = []string{"#35E6FF", "#FF3DCB", "#FFB23E", "#5B8CFF", "#9D7BFF", "#9DEE4E", "#FF6B8B"}

var tenantMeta = map[string]struct{ Key, Name, Glyph, Accent string }{
	"personal":    {"personal", "Personal", "✦", "#FFB23E"},
	"blazesphere": {"blazesphere", "BlazeSphere", "◆", "#35E6FF"},
	"kuadre":      {"kuadre", "Kuadre", "▲", "#FF3DCB"},
	"roundplay":   {"roundplay", "Round PlayGames", "●", "#9DEE4E"},
}

func (a *apiServer) fetchCorpus() (*corpusResp, error) {
	req, err := http.NewRequest(http.MethodGet, a.mcpURL+"/admin/corpus", nil)
	if err != nil {
		return nil, err
	}
	if a.token != "" {
		req.Header.Set("Authorization", "Bearer "+a.token)
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("mcp /admin/corpus: status %d", resp.StatusCode)
	}
	var cr corpusResp
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		return nil, err
	}
	return &cr, nil
}

func (a *apiServer) handleGalaxy(w http.ResponseWriter, r *http.Request) {
	tk := r.URL.Query().Get("tenant")
	if tk == "" {
		tk = "personal"
	}
	cr, err := a.fetchCorpus()
	if err != nil {
		http.Error(w, `{"error":"upstream"}`, http.StatusBadGateway)
		return
	}
	out := buildGalaxy(tk, cr)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

type commsMsg struct {
	Seq    uint64 `json:"seq"`
	Area   string `json:"area"`
	From   string `json:"from"`
	Kind   string `json:"kind"`
	Text   string `json:"text"`
	Target string `json:"target"`
	Ref    uint64 `json:"ref"`
	TS     int64  `json:"ts"`
}

var emptyComms = []byte(`{"columns":[],"agents":[]}`)

func (a *apiServer) handleComms(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	req, err := http.NewRequest(http.MethodGet, a.mcpURL+"/admin/comms", nil)
	if err != nil {
		w.Write(emptyComms)
		return
	}
	if a.token != "" {
		req.Header.Set("Authorization", "Bearer "+a.token)
	}
	resp, err := a.client.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		if resp != nil {
			resp.Body.Close()
		}
		w.Write(emptyComms)
		return
	}
	defer resp.Body.Close()
	var in struct {
		Messages []commsMsg `json:"messages"`
	}
	if json.NewDecoder(resp.Body).Decode(&in) != nil {
		w.Write(emptyComms)
		return
	}
	w.Write(buildComms(in.Messages))
}

type commsCard struct {
	ID        uint64 `json:"id"`
	Author    string `json:"author"`
	Type      string `json:"type"`
	TypeColor string `json:"typeColor"`
	Target    string `json:"target"`
	Text      string `json:"text"`
	Mins      int    `json:"mins"`
	Reply     bool   `json:"reply"`
	Promotable bool  `json:"promotable"`
}
type commsColumn struct {
	Key   string      `json:"key"`
	Label string      `json:"label"`
	Color string      `json:"color"`
	Cards []commsCard `json:"cards"`
}

func buildComms(msgs []commsMsg) []byte {
	typeColor := map[string]string{"pregunta": "#35E6FF", "respuesta": "#34E5A0", "info": "#9B6CFF", "nota": "#7C8AA5"}
	kindToType := func(k string) string {
		switch k {
		case "question":
			return "pregunta"
		case "answer":
			return "respuesta"
		case "info":
			return "info"
		default:
			return "nota"
		}
	}
	now := time.Now().UnixMilli()
	order := []string{}
	cols := map[string]*commsColumn{}
	agentsSeen := map[string]bool{}
	agents := []string{}
	for _, m := range msgs {
		area := unescapeHTML(m.Area)
		col, ok := cols[area]
		if !ok {
			col = &commsColumn{Key: fmt.Sprintf("a%d", len(order)), Label: area, Color: palette[len(order)%len(palette)]}
			cols[area] = col
			order = append(order, area)
		}
		typ := kindToType(m.Kind)
		mins := int((now - m.TS) / 60000)
		if mins < 0 {
			mins = 0
		}
		col.Cards = append(col.Cards, commsCard{
			ID: m.Seq, Author: m.From, Type: typ, TypeColor: typeColor[typ], Target: m.Target,
			Text: m.Text, Mins: mins, Reply: m.Ref != 0, Promotable: typ != "pregunta",
		})
		if m.From != "" && !agentsSeen[m.From] {
			agentsSeen[m.From] = true
			agents = append(agents, m.From)
		}
	}
	out := struct {
		Columns []commsColumn `json:"columns"`
		Agents  []string      `json:"agents"`
	}{Columns: make([]commsColumn, 0, len(order)), Agents: agents}
	for _, area := range order {
		out.Columns = append(out.Columns, *cols[area])
	}
	b, err := json.Marshal(out)
	if err != nil {
		return emptyComms
	}
	return b
}

func unescapeHTML(s string) string {
	s = strings.ReplaceAll(s, "&gt;", ">")
	s = strings.ReplaceAll(s, "&lt;", "<")
	s = strings.ReplaceAll(s, "&amp;", "&")
	return s
}

// handleJournal proxies the tenant's recent semantic journal events from the
// MCP (server-side, with the bearer) straight through to the dashboard.
func (a *apiServer) handleJournal(w http.ResponseWriter, r *http.Request) {
	limit := r.URL.Query().Get("limit")
	url := a.mcpURL + "/admin/journal"
	if limit != "" {
		url += "?limit=" + limit
	}
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		http.Error(w, `{"error":"bad request"}`, http.StatusBadGateway)
		return
	}
	if a.token != "" {
		req.Header.Set("Authorization", "Bearer "+a.token)
	}
	resp, err := a.client.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		if resp != nil {
			resp.Body.Close()
		}
		http.Error(w, `{"error":"upstream"}`, http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "application/json")
	io.Copy(w, resp.Body)
}

type galaxyPoint struct {
	ID           string     `json:"id"`
	Tenant       string     `json:"tenant"`
	Cluster      int        `json:"cluster"`
	ClusterKey   string     `json:"clusterKey"`
	ClusterLabel string     `json:"clusterLabel"`
	Color        string     `json:"color"`
	Pos          [3]float32 `json:"pos"`
	Text         string     `json:"text"`
	Tags         []string   `json:"tags"`
	Source       string     `json:"source"`
	Access       int        `json:"access"`
	Heat         float64    `json:"heat"`
	CreatedAt    int64      `json:"createdAt"`
	Dim          int        `json:"dim"`
}
type galaxyCluster struct {
	Key    string     `json:"key"`
	Label  string     `json:"label"`
	Color  string     `json:"color"`
	Center [3]float32 `json:"center"`
	Count  int        `json:"count"`
}
type tenantInfo struct {
	Key    string   `json:"key"`
	Name   string   `json:"name"`
	Glyph  string   `json:"glyph"`
	Accent string   `json:"accent"`
	// Agents must be non-empty: the dashboard's simulated journal indexes
	// tenant.agents at random, so an empty/absent list crashes it. We surface
	// the corpus's distinct sources as stand-in agent labels.
	Agents []string `json:"agents"`
}

type galaxyData struct {
	Tenant   tenantInfo      `json:"tenant"`
	Clusters []galaxyCluster `json:"clusters"`
	Points   []galaxyPoint   `json:"points"`
}

func buildGalaxy(tenantKey string, cr *corpusResp) galaxyData {
	n := len(cr.Memories)
	vecs := make([][]float32, n)
	for i, m := range cr.Memories {
		vecs[i] = m.Vec
	}
	pos := pca3(vecs)
	k := n / 12
	if k < 2 {
		k = 2
	}
	if k > 6 {
		k = 6
	}
	if k > n && n > 0 {
		k = n
	}
	assign, centers := kmeans(pos, k)

	labelOf := make([]string, len(centers))
	for c := range centers {
		freq := map[string]int{}
		for i := range cr.Memories {
			if i < len(assign) && assign[i] == c && len(cr.Memories[i].Tags) > 0 {
				freq[cr.Memories[i].Tags[0]]++
			}
		}
		best, bestN := "", 0
		for tag, f := range freq {
			if f > bestN || (f == bestN && tag < best) {
				best, bestN = tag, f
			}
		}
		if best == "" {
			best = fmt.Sprintf("grupo %d", c+1)
		}
		labelOf[c] = best
	}

	tm, ok := tenantMeta[tenantKey]
	if !ok {
		tm = struct{ Key, Name, Glyph, Accent string }{tenantKey, tenantKey, "✦", "#35E6FF"}
	}

	clusters := make([]galaxyCluster, len(centers))
	for c := range centers {
		clusters[c] = galaxyCluster{
			Key: fmt.Sprintf("c%d", c), Label: labelOf[c],
			Color: palette[c%len(palette)], Center: centers[c],
		}
	}
	now := time.Now().UnixMilli()
	pts := make([]galaxyPoint, n)
	for i, m := range cr.Memories {
		c := 0
		if i < len(assign) {
			c = assign[i]
		}
		heat := 0.0
		if n > 1 {
			heat = float64(i) / float64(n-1)
		}
		pts[i] = galaxyPoint{
			ID: fmt.Sprintf("m%d", m.ID), Tenant: tm.Key, Cluster: c,
			ClusterKey: clusters[c].Key, ClusterLabel: labelOf[c], Color: palette[c%len(palette)],
			Pos: pos[i], Text: m.Text, Tags: m.Tags, Source: m.Src,
			Access: int(heat*1000) + 1, Heat: heat, CreatedAt: now, Dim: cr.Dim,
		}
		clusters[c].Count++
	}
	sort.SliceStable(clusters, func(i, j int) bool { return clusters[i].Key < clusters[j].Key })
	return galaxyData{
		Tenant:   tenantInfo{Key: tm.Key, Name: tm.Name, Glyph: tm.Glyph, Accent: tm.Accent, Agents: deriveAgents(cr)},
		Clusters: clusters, Points: pts,
	}
}

// deriveAgents surfaces the corpus's distinct sources as stand-in agent labels
// for the simulated journal (which indexes tenant.agents). Always returns a
// non-empty slice — a default when no sources are tagged.
func deriveAgents(cr *corpusResp) []string {
	seen := map[string]bool{}
	var agents []string
	for _, m := range cr.Memories {
		if m.Src != "" && !seen[m.Src] {
			seen[m.Src] = true
			agents = append(agents, "agent-"+m.Src)
			if len(agents) >= 6 {
				break
			}
		}
	}
	if len(agents) == 0 {
		return []string{"agent-self", "agent-recall"}
	}
	return agents
}
