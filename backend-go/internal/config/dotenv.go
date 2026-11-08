package config

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// LoadLocalDotEnv loads the first .env found in the working directory, next
// to the executable, or in the executable's parent directory (bin/..). It
// returns the file used ("" when none exists).
func LoadLocalDotEnv() (string, error) {
	candidates := []string{".env"}
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		candidates = append(candidates, filepath.Join(dir, ".env"), filepath.Join(dir, "..", ".env"))
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err != nil {
			continue
		}
		if _, err := LoadDotEnv(p); err != nil {
			return p, err
		}
		abs, _ := filepath.Abs(p)
		return abs, nil
	}
	return "", nil
}

// LoadDotEnv loads KEY=VALUE lines from path into the process environment for
// local development. Variables already set in the environment win (so the
// platform's values are never overridden), missing files are ignored, and
// values are never logged. Supports comments (#), blank lines, optional
// "export " prefixes and single/double quoted values.
func LoadDotEnv(path string) (loaded int, err error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("opening %s: %w", path, err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, value, ok := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" || strings.ContainsAny(key, " \t") {
			return loaded, fmt.Errorf("%s:%d: expected KEY=VALUE", path, n)
		}
		value = strings.TrimSpace(value)
		if len(value) >= 2 && (value[0] == '"' || value[0] == '\'') && value[len(value)-1] == value[0] {
			value = value[1 : len(value)-1]
		} else if i := strings.Index(value, " #"); i >= 0 {
			value = strings.TrimSpace(value[:i]) // inline comment on unquoted values
		}
		if _, set := os.LookupEnv(key); set {
			continue
		}
		if err := os.Setenv(key, value); err != nil {
			return loaded, fmt.Errorf("%s:%d: setting %s: %w", path, n, key, err)
		}
		loaded++
	}
	return loaded, sc.Err()
}
