// Reading and updating the .env file. Updates keep every comment
// and the order of the file: a key's commented-out template line is
// switched on in place, so the explanation stays above it.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var (
	activeLine    = regexp.MustCompile(`^\s*(?:export\s+)?([A-Za-z_][A-Za-z0-9_]*)\s*=`)
	commentedLine = regexp.MustCompile(`^\s*#\s?([A-Z_][A-Z0-9_]*)=`)
)

// parseEnv reads the active KEY=VALUE lines the way the app does:
// the first value of a key wins and one pair of quotes is removed.
func parseEnv(data string) map[string]string {
	out := map[string]string{}
	for _, l := range strings.Split(data, "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		l = strings.TrimPrefix(l, "export ")
		k, v, ok := strings.Cut(l, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		if _, seen := out[k]; seen || k == "" {
			continue
		}
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
			v = v[1 : len(v)-1]
		}
		out[k] = v
	}
	return out
}

// quoteEnv writes a value so the app's loader and systemd's
// EnvironmentFile both read back exactly the same text.
func quoteEnv(v string) string {
	if v == "" || !strings.ContainsAny(v, " \t#'\"\\$`;&|<>()") {
		return v
	}
	if strings.Contains(v, "'") {
		return `"` + v + `"`
	}
	return "'" + v + "'"
}

// applyEnvEdits returns the file with edits applied. A value sets
// the key; an empty value turns its line back into a blank comment,
// so no old secret is left behind. Repeated active lines of a key
// are retired, because the app reads the first and systemd the last.
func applyEnvEdits(data string, edits map[string]string) string {
	lines := strings.Split(data, "\n")
	done := map[string]bool{}

	// Active lines first, so a key that is already on stays put.
	for i, l := range lines {
		m := activeLine.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		v, ok := edits[m[1]]
		if !ok {
			continue
		}
		switch {
		case done[m[1]]:
			lines[i] = "# " + m[1] + "="
		case v == "":
			lines[i] = "# " + m[1] + "="
			done[m[1]] = true
		default:
			lines[i] = m[1] + "=" + quoteEnv(v)
			done[m[1]] = true
		}
	}
	// Then the template's commented-out line for each remaining key.
	for i, l := range lines {
		m := commentedLine.FindStringSubmatch(l)
		if m == nil || done[m[1]] {
			continue
		}
		if v, ok := edits[m[1]]; ok && v != "" {
			lines[i] = m[1] + "=" + quoteEnv(v)
			done[m[1]] = true
		}
	}
	out := strings.Join(lines, "\n")
	// Anything else is appended, in catalog order so a run is stable.
	var extra strings.Builder
	for _, s := range settingsCatalog {
		if v, ok := edits[s.Key]; ok && v != "" && !done[s.Key] {
			fmt.Fprintf(&extra, "%s=%s\n", s.Key, quoteEnv(v))
			done[s.Key] = true
		}
	}
	if extra.Len() > 0 {
		if out != "" && !strings.HasSuffix(out, "\n") {
			out += "\n"
		}
		out += "\n# Set by the installer.\n" + extra.String()
	}
	return out
}

// updateEnvFile applies edits to the file atomically, keeping its
// owner and mode. A missing file is created readable by its owner
// and group only, since it holds secrets.
func updateEnvFile(path string, edits map[string]string) error {
	if len(edits) == 0 {
		return nil
	}
	mode := os.FileMode(0o640)
	uid, gid := -1, -1
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if fi, statErr := os.Stat(path); statErr == nil {
		mode = fi.Mode().Perm()
		if u, g, ok := fileOwner(fi); ok {
			uid, gid = u, g
		}
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".env.new-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	// Restrict the temp file before any secret is written to it.
	if err := os.Chmod(tmp.Name(), mode); err != nil {
		tmp.Close()
		return err
	}
	if uid >= 0 {
		if err := tmp.Chown(uid, gid); err != nil {
			tmp.Close()
			return err
		}
	}
	if _, err := tmp.WriteString(applyEnvEdits(string(data), edits)); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
