// Package envfile loads local dotenv configuration without executing shell code.
package envfile

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
)

var keyPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Load leaves existing environment variables, including explicitly empty ones,
// unchanged. The default .env is optional; other requested paths must exist.
// An empty path disables file loading. File contents never appear in errors.
func Load(path string) error {
	if path == "" {
		return nil
	}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) && path == ".env" {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	values, err := parse(f)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	for key, value := range values {
		if _, exists := os.LookupEnv(key); !exists {
			if err := os.Setenv(key, value); err != nil {
				return fmt.Errorf("cannot set environment variable %s", key)
			}
		}
	}
	return nil
}

func parse(reader io.Reader) (map[string]string, error) {
	values := make(map[string]string)
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := strings.TrimSpace(strings.TrimPrefix(scanner.Text(), "\ufeff"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "export ") || strings.HasPrefix(line, "export\t") {
			line = strings.TrimSpace(line[len("export"):])
		}
		key, raw, ok := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if !ok || !keyPattern.MatchString(key) {
			return nil, fmt.Errorf("line %d: expected KEY=value", lineNumber)
		}
		value, err := parseValue(strings.TrimSpace(raw))
		if err != nil || strings.ContainsRune(value, '\x00') {
			return nil, fmt.Errorf("line %d: invalid value or quoting", lineNumber)
		}
		values[key] = value
	}
	if scanner.Err() != nil {
		return nil, fmt.Errorf("cannot read dotenv file")
	}
	return values, nil
}

func parseValue(raw string) (string, error) {
	if raw == "" {
		return "", nil
	}
	if raw[0] != '\'' && raw[0] != '"' {
		return strings.TrimSpace(stripComment(raw)), nil
	}
	quote := raw[0]
	escaped := false
	for end := 1; end < len(raw); end++ {
		if escaped {
			escaped = false
			continue
		}
		if quote == '"' && raw[end] == '\\' {
			escaped = true
			continue
		}
		if raw[end] != quote {
			continue
		}
		tail := strings.TrimSpace(raw[end+1:])
		if tail != "" && !strings.HasPrefix(tail, "#") {
			return "", fmt.Errorf("unexpected text after quote")
		}
		if quote == '\'' {
			return raw[1:end], nil
		}
		return strconv.Unquote(raw[:end+1])
	}
	return "", fmt.Errorf("unterminated quote")
}

func stripComment(value string) string {
	for index := range value {
		if value[index] == '#' && (index == 0 || value[index-1] == ' ' || value[index-1] == '\t') {
			return value[:index]
		}
	}
	return value
}
