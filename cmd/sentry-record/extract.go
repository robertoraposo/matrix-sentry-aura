package main

import (
	"os"
	"path/filepath"
	"strings"
)

// extractPaths returns the existing regular files an agent touched in a single
// tool use, as absolute paths, deduplicated in first-seen order.
//
// The "exists as a regular file" gate (os.Stat) is what tames noisy tools: a
// Bash command or Grep result yields many tokens, but only those that resolve
// to a real file on disk become accesses. Flags, subcommands, directories and
// glob patterns fall away on their own.
func extractPaths(toolName string, input map[string]any, response any, cwd string) []string {
	var candidates []string
	switch toolName {
	case "Read", "Edit", "Write", "MultiEdit":
		candidates = strField(input, "file_path")
	case "NotebookEdit":
		candidates = strField(input, "notebook_path")
	case "Bash":
		candidates = strings.Fields(str(input["command"]))
	case "Grep", "Glob":
		for _, s := range collectStrings(response) {
			candidates = append(candidates, strings.Fields(s)...)
		}
	default:
		return nil
	}

	seen := make(map[string]bool)
	var out []string
	for _, c := range candidates {
		p := resolveFile(c, cwd)
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

// resolveFile turns a candidate token into an absolute path if it (or its
// path-prefix before a "file:line" colon) is an existing regular file. Relative
// paths are resolved against cwd. Returns "" if nothing real matches.
func resolveFile(tok, cwd string) string {
	for _, cand := range []string{tok, beforeColon(tok)} {
		if cand == "" {
			continue
		}
		p := cand
		if !filepath.IsAbs(p) {
			p = filepath.Join(cwd, p)
		}
		p = filepath.Clean(p)
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
			return p
		}
	}
	return ""
}

// beforeColon returns the substring before the first ':' (e.g. the path in a
// "path:line:match" grep line), or "" if there is no interior colon.
func beforeColon(s string) string {
	if i := strings.IndexByte(s, ':'); i > 0 {
		return s[:i]
	}
	return ""
}

func strField(input map[string]any, key string) []string {
	if s := str(input[key]); s != "" {
		return []string{s}
	}
	return nil
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

// collectStrings recursively gathers every string value in a decoded JSON value
// (string, array, or object).
func collectStrings(v any) []string {
	switch x := v.(type) {
	case string:
		return []string{x}
	case []any:
		var out []string
		for _, e := range x {
			out = append(out, collectStrings(e)...)
		}
		return out
	case map[string]any:
		var out []string
		for _, e := range x {
			out = append(out, collectStrings(e)...)
		}
		return out
	}
	return nil
}
