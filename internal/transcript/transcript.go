// Package transcript parses Claude Code .jsonl session transcripts. It exposes
// the two things Matrix Sentry's auto-remember needs: the count of tool_use
// blocks (the activity signal that drives the reflect hook's threshold) and a
// segmentation of a session into windows of K tool-uses with their surrounding
// prose (the material the yield measurement labels). Both the measurement and
// the hook count tool-uses through CountToolUses so their notion of "activity"
// is identical — the calibrated K is only meaningful if they agree.
package transcript

import (
	"bufio"
	"encoding/json"
	"io"
	"strings"
)

type line struct {
	Type    string `json:"type"`
	Message struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

type block struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// parseContent decodes a message's content, which is either a JSON string (a
// plain text message) or an array of typed blocks. Unknown shapes yield nil.
func parseContent(raw json.RawMessage) []block {
	if len(raw) == 0 {
		return nil
	}
	var blocks []block
	if err := json.Unmarshal(raw, &blocks); err == nil {
		return blocks
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return []block{{Type: "text", Text: s}}
	}
	return nil
}

func newScanner(r io.Reader) *bufio.Scanner {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1<<20), 16<<20) // transcript lines can be large
	return sc
}

// CountToolUses returns the number of tool_use blocks in a transcript. Non-JSON
// or partial lines are tolerated (skipped), so a live, still-growing transcript
// never errors the caller.
func CountToolUses(r io.Reader) (int, error) {
	sc := newScanner(r)
	n := 0
	for sc.Scan() {
		var l line
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			continue
		}
		for _, b := range parseContent(l.Message.Content) {
			if b.Type == "tool_use" {
				n++
			}
		}
	}
	return n, sc.Err()
}

// Window is a slice of a session: the prose around K tool-uses, the unit the
// yield measurement labels for "contains a durable fact?".
type Window struct {
	Session  string `json:"session"`
	Index    int    `json:"window"`
	ToolUses int    `json:"tool_uses"`
	Text     string `json:"text"`
}

// Windows segments a transcript into windows that close every k tool-uses,
// collecting the text blocks seen along the way. tool_result content is
// deliberately excluded (it is file/command output — noise for durability).
func Windows(session string, r io.Reader, k int) ([]Window, error) {
	if k <= 0 {
		k = 1
	}
	sc := newScanner(r)
	var out []Window
	var sb strings.Builder
	tu, idx := 0, 0
	flush := func() {
		out = append(out, Window{Session: session, Index: idx, ToolUses: tu, Text: sb.String()})
		idx++
		tu = 0
		sb.Reset()
	}
	for sc.Scan() {
		var l line
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			continue
		}
		for _, b := range parseContent(l.Message.Content) {
			switch b.Type {
			case "text":
				if t := strings.TrimSpace(b.Text); t != "" {
					sb.WriteString(t)
					sb.WriteString("\n")
				}
			case "tool_use":
				tu++
				if tu >= k {
					flush()
				}
			}
		}
	}
	if tu > 0 || sb.Len() > 0 {
		flush()
	}
	return out, sc.Err()
}
