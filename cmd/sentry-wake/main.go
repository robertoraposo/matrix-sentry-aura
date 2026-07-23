// Command sentry-wake is the single Claude Code / Codex Stop hook that carries
// all turn-end jobs in priority order: wake-on-reply > reflect > close, with a
// block-alert fallback. At turn end it fetches the agent's Matrix Sentry inbox
// and, when new directed messages exist, wakes the turn ({"decision":"block"})
// so an intra-session loop survives across turns; else it runs the reflection
// cadence; else it posts a close confirmation.
//
// It is best-effort and unkillable from the agent's point of view: any problem
// ends in a clean exit 0, it is a no-op without SENTRY_MCP_URL, and
// stop_hook_active prevents wake/reflect loops (the cursor advance is the guard).
//
//	go build -o sentry-wake ./cmd/sentry-wake
//	# register as the Stop hook (async:false), replacing sentry-reflect's entry
//
// This file (Task 2) holds the PURE decision core + per-session state only. The
// I/O wiring (inbox fetch/parse, block output, main) lands in Task 3.
package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// reflectEvery is the activity threshold (tool-uses per reflection), reused from
// sentry-reflect's Phase-0 yield measurement (K=40).
const reflectEvery = 40

// action is the turn-end decision sentry-wake takes, in priority order.
type action int

const (
	actWake       action = iota // new directed messages: wake the turn (decision:block)
	actReflect                  // no messages, enough activity: run the reflection cadence
	actClose                    // caught up, nothing to do: post a close note, let the turn end
	actBlockAlert               // inside a continuation with undelivered messages: alert, allow stop
)

// state is the per-session persisted cursor + reflection counter (JSON file).
type state struct {
	Cursor       uint64 `json:"cursor"`
	ReflectCount int    `json:"reflectCount"`
}

// inboxMsg is one directed message parsed from the inbox tool reply.
type inboxMsg struct {
	Seq    uint64
	From   string
	Kind   string
	Text   string
	Target string
}

// decide is the pure turn-end policy. It never does I/O so it is unit-testable
// without a server. Priority (all real paths ultimately exit 0 in main):
//
//  1. Wake:       !stopActive && any msg.Seq > st.Cursor  (new Cursor = maxSeq)
//  2. Reflect:    !stopActive && toolDelta >= reflectEvery
//  3. BlockAlert: stopActive  && any msg.Seq > st.Cursor  (won't wake again)
//  4. Close:      otherwise
//
// The returned state is the state to persist for this decision: on Wake the
// cursor advances to maxSeq (the loop guard so the same messages never wake
// twice); every other action leaves the cursor untouched. The ReflectCount is
// left to the caller to advance on a Reflect (main knows the current count).
func decide(msgs []inboxMsg, st state, stopActive bool, toolDelta int) (action, state) {
	var maxSeq uint64
	hasNew := false
	for _, m := range msgs {
		if m.Seq > maxSeq {
			maxSeq = m.Seq
		}
		if m.Seq > st.Cursor {
			hasNew = true
		}
	}
	switch {
	case !stopActive && hasNew:
		ns := st
		ns.Cursor = maxSeq
		return actWake, ns
	case !stopActive && toolDelta >= reflectEvery:
		return actReflect, st
	case stopActive && hasNew:
		return actBlockAlert, st
	default:
		return actClose, st
	}
}

// stateDir is where per-session wake state lives.
func stateDir() string {
	base, err := os.UserCacheDir()
	if err != nil {
		base = os.TempDir()
	}
	return filepath.Join(base, "matrix-sentry", "wake")
}

// safeName reduces a session id to a single path-safe file name so a crafted id
// can never write outside the state dir. Copied verbatim from sentry-reflect.
func safeName(sid string) string {
	if sid == "" {
		return "default"
	}
	name := filepath.Base(filepath.Clean("/" + strings.ReplaceAll(sid, string(os.PathSeparator), "_")))
	if name == "/" || name == "." || name == "" {
		return "default"
	}
	return name
}

// readState loads the per-session state; a missing/corrupt file reads as the
// zero state (best-effort, never errors).
func readState(dir, sid string) state {
	var st state
	b, err := os.ReadFile(filepath.Join(dir, safeName(sid)))
	if err != nil {
		return st
	}
	_ = json.Unmarshal(b, &st)
	return st
}

// writeState persists the per-session state as JSON.
func writeState(dir, sid string, st state) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, safeName(sid)), b, 0o644)
}

// main is a no-op placeholder for Task 2: it drains stdin and exits 0 so the
// hook is already safe to register. Task 3 fills in the inbox fetch, decide
// wiring, block output, and best-effort posts.
func main() {
	_, _ = io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
}
