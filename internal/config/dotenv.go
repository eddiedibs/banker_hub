package config

import (
	"bufio"
	"os"
	"strings"
)

// ReadEnvFile parses KEY=VALUE lines. It accepts the single-quoted values
// that writeEnvFile produces, double quotes, and trailing " # comments"
// on unquoted values. A missing file returns an empty map.
func ReadEnvFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	out := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(strings.TrimPrefix(line, "export "), "=")
		if !ok {
			continue
		}
		out[strings.TrimSpace(key)] = unquote(strings.TrimSpace(val))
	}
	return out, sc.Err()
}

func unquote(v string) string {
	switch {
	case strings.HasPrefix(v, "'"):
		// Shell single quotes: 'it'\''s' means it's.
		var b strings.Builder
		for _, part := range strings.Split(v, `'\''`) {
			b.WriteString(strings.Trim(part, "'"))
			b.WriteString("'")
		}
		return strings.TrimSuffix(b.String(), "'")
	case strings.HasPrefix(v, `"`) && strings.HasSuffix(v, `"`) && len(v) >= 2:
		return v[1 : len(v)-1]
	default:
		if i := strings.Index(v, " #"); i >= 0 {
			v = v[:i]
		}
		return strings.TrimSpace(v)
	}
}

// LoadDotEnv sets variables from the env file at path. Variables already
// set in the process environment win. A missing file is not an error.
func LoadDotEnv(path string) error {
	values, err := ReadEnvFile(path)
	if err != nil {
		return err
	}
	for k, v := range values {
		if _, set := os.LookupEnv(k); !set {
			if err := os.Setenv(k, v); err != nil {
				return err
			}
		}
	}
	return nil
}
