// Package config loads service configuration from the environment.
//
// Twelve-factor, with one addition that matters in a platform that ships to
// customer premises: every value is resolvable from a file as well as an
// environment variable (NAME_FILE=/run/secrets/x), because the Store Gateway
// Unit receives its credentials from a mounted secret on a device that is not
// running Kubernetes and has no External Secrets Operator.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Loader accumulates lookup errors so that a misconfigured service reports
// every missing variable at once instead of one per restart.
type Loader struct {
	prefix string
	errs   []string
}

// New creates a loader. All lookups are prefixed, e.g. New("USSLP") turns
// String("LOG_LEVEL") into a lookup of USSLP_LOG_LEVEL.
func New(prefix string) *Loader {
	return &Loader{prefix: strings.TrimSuffix(strings.ToUpper(prefix), "_")}
}

func (l *Loader) key(name string) string {
	name = strings.ToUpper(name)
	if l.prefix == "" {
		return name
	}
	return l.prefix + "_" + name
}

// lookup resolves a value from NAME_FILE first, then NAME.
func (l *Loader) lookup(name string) (string, bool) {
	k := l.key(name)
	if path := os.Getenv(k + "_FILE"); path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			l.errs = append(l.errs, fmt.Sprintf("%s_FILE=%s: %v", k, path, err))
			return "", false
		}
		return strings.TrimSpace(string(b)), true
	}
	v, ok := os.LookupEnv(k)
	return v, ok
}

// String returns a value or its default.
func (l *Loader) String(name, def string) string {
	if v, ok := l.lookup(name); ok && v != "" {
		return v
	}
	return def
}

// Required returns a value, recording an error if it is absent.
func (l *Loader) Required(name string) string {
	if v, ok := l.lookup(name); ok && v != "" {
		return v
	}
	l.errs = append(l.errs, l.key(name)+" is required")
	return ""
}

// Int returns an integer value or its default.
func (l *Loader) Int(name string, def int) int {
	v, ok := l.lookup(name)
	if !ok || v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		l.errs = append(l.errs, fmt.Sprintf("%s=%q: not an integer", l.key(name), v))
		return def
	}
	return n
}

// Bool returns a boolean value or its default.
func (l *Loader) Bool(name string, def bool) bool {
	v, ok := l.lookup(name)
	if !ok || v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		l.errs = append(l.errs, fmt.Sprintf("%s=%q: not a boolean", l.key(name), v))
		return def
	}
	return b
}

// Duration returns a duration value or its default, e.g. "250ms", "3s".
func (l *Loader) Duration(name string, def time.Duration) time.Duration {
	v, ok := l.lookup(name)
	if !ok || v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		l.errs = append(l.errs, fmt.Sprintf("%s=%q: not a duration", l.key(name), v))
		return def
	}
	return d
}

// StringSlice returns a comma-separated list.
func (l *Loader) StringSlice(name string, def []string) []string {
	v, ok := l.lookup(name)
	if !ok || strings.TrimSpace(v) == "" {
		return def
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Err returns the accumulated configuration errors, or nil.
func (l *Loader) Err() error {
	if len(l.errs) == 0 {
		return nil
	}
	return fmt.Errorf("config: %s", strings.Join(l.errs, "; "))
}

// ServiceConfig is the configuration every USSLP binary shares.
type ServiceConfig struct {
	Service     string
	Version     string
	Region      string
	Environment string // "dev", "staging", "prod"
	LogLevel    string
	LogFormat   string
	AdminAddr   string
	DataDir     string
	EnablePprof bool
	TraceSample int
}

// LoadService reads the shared configuration.
func LoadService(l *Loader, service string) ServiceConfig {
	env := l.String("ENVIRONMENT", "dev")
	return ServiceConfig{
		Service:     service,
		Version:     l.String("VERSION", "dev"),
		Region:      l.String("REGION", "local"),
		Environment: env,
		LogLevel:    l.String("LOG_LEVEL", map[bool]string{true: "info", false: "debug"}[env == "prod"]),
		LogFormat:   l.String("LOG_FORMAT", map[bool]string{true: "json", false: "text"}[env == "prod"]),
		AdminAddr:   l.String("ADMIN_ADDR", ":9090"),
		DataDir:     l.String("DATA_DIR", "./data"),
		EnablePprof: l.Bool("ENABLE_PPROF", env != "prod"),
		TraceSample: l.Int("TRACE_SAMPLE_ONE_IN", 1),
	}
}
