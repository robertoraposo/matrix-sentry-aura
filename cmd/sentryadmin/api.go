package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
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

// handleComms returns empty live comms for now (real wiring is a follow-up).
func (a *apiServer) handleComms(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"areas":[],"messages":[]}`))
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
	Key    string `json:"key"`
	Name   string `json:"name"`
	Glyph  string `json:"glyph"`
	Accent string `json:"accent"`
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
		Tenant:   tenantInfo{Key: tm.Key, Name: tm.Name, Glyph: tm.Glyph, Accent: tm.Accent},
		Clusters: clusters, Points: pts,
	}
}
