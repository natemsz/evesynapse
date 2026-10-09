// Package dotenv reads a .env file into the process environment. The
// server uses it for its own configuration, and the self-updater
// uses it to see the settings of the install it is updating.
package dotenv

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strings"
)

// dotenvMaxLine is the longest .env line accepted: far past the
// scanner's 64KB default, so a long key or URL never silently
// truncates the file mid-line and drops the rest of it.
const dotenvMaxLine = 1 << 20

// Load is a small .env loader. Format: KEY=VALUE per line, blank lines
// and #-comments skipped, an optional leading "export " tolerated,
// optional surrounding quotes stripped. A key already in the real
// environment wins: the file only fills in gaps. A missing file is not
// an error; anything else that goes wrong (unreadable file, I/O
// failure, an over-long line) is, because silently running on defaults
// can point the app at the wrong database.
func Load(path string) error {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil // no .env file: configuration comes from the environment only
		}
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), dotenvMaxLine)
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
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	return nil
}
