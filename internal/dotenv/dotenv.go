// Package dotenv reads a .env file into the process environment. The
// server uses it for its own configuration, and the self-updater
// uses it to see the settings of the install it is updating.
package dotenv

import (
	"bufio"
	"os"
	"strings"
)

// Load is a small hand-rolled .env loader. Format: KEY=VALUE per
// line, blank lines and #-comments skipped, an optional leading
// "export " tolerated, optional surrounding quotes stripped. A key
// already present in the real environment always wins — the file only
// fills in gaps. A file that is not there is not an error:
// configuration then comes from the environment only.
func Load(path string) {
	f, err := os.Open(path)
	if err != nil {
		return // no .env file: configuration comes from the environment only
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if len(value) >= 2 {
			first, last := value[0], value[len(value)-1]
			if (first == '"' && last == '"') || (first == '\'' && last == '\'') {
				value = value[1 : len(value)-1]
			}
		}
		if key == "" {
			continue
		}
		if _, present := os.LookupEnv(key); present {
			continue // real environment wins
		}
		_ = os.Setenv(key, value)
	}
}
