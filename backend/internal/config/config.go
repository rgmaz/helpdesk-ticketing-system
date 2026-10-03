package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Config holds all runtime configuration for the ITSM backend. Every value can
// be overridden with an environment variable so the same binary works in
// development and production without a rebuild.
type Config struct {
	// Network
	Host string
	Port int

	// Storage
	DBPath string

	// Auth
	JWTSecret     string
	AccessTTL     time.Duration
	AllowedOrigin []string

	// SLA engine
	SLAScanInterval time.Duration
	SLAWarnBefore   time.Duration

	// Bootstrap admin (created on first run only)
	AdminUsername string
	AdminEmail    string
	AdminPassword string

	// DemoData records that the process was asked to seed demonstration
	// accounts and tickets. GET /meta publishes it so the sign-in screen can
	// offer those credentials only where they exist; without the signal the UI
	// would either always advertise a login that cannot work or never mention
	// one that does.
	DemoData bool

	// Shutdown
	ShutdownTimeout time.Duration
}

func Load() (*Config, error) {
	cfg := &Config{
		Host:            env("ITSM_HOST", "127.0.0.1"),
		Port:            envInt("ITSM_PORT", 8080),
		DBPath:          env("ITSM_DB_PATH", defaultDBPath()),
		JWTSecret:       env("ITSM_JWT_SECRET", ""),
		AccessTTL:       envDuration("ITSM_ACCESS_TTL", 12*time.Hour),
		AllowedOrigin:   envList("ITSM_ALLOWED_ORIGINS", "http://localhost:5173,http://127.0.0.1:5173"),
		SLAScanInterval: envDuration("ITSM_SLA_SCAN_INTERVAL", time.Minute),
		SLAWarnBefore:   envDuration("ITSM_SLA_WARN_BEFORE", 15*time.Minute),
		AdminUsername:   env("ITSM_ADMIN_USERNAME", "admin"),
		AdminEmail:      env("ITSM_ADMIN_EMAIL", "admin@itsm.local"),
		// Deliberately no default: leaving it unset makes the bootstrap generate a
		// random password and log it once, rather than shipping a credential that
		// is already published in the source.
		AdminPassword:   env("ITSM_ADMIN_PASSWORD", ""),
		DemoData:        envBool("ITSM_DEMO", false),
		ShutdownTimeout: envDuration("ITSM_SHUTDOWN_TIMEOUT", 10*time.Second),
	}

	if len(cfg.JWTSecret) == 0 {
		return nil, fmt.Errorf("ITSM_JWT_SECRET must be set (generate one with: openssl rand -hex 32)")
	}
	if len(cfg.JWTSecret) < 16 {
		return nil, fmt.Errorf("ITSM_JWT_SECRET must be at least 16 characters")
	}
	if cfg.SLAScanInterval < time.Second {
		return nil, fmt.Errorf("ITSM_SLA_SCAN_INTERVAL must be >= 1s")
	}
	return cfg, nil
}

// Addr is the listen address for the HTTP server.
func (c *Config) Addr() string {
	return fmt.Sprintf("%s:%d", c.Host, c.Port)
}

// Foreign keys are enforced per connection, so the pragmas live in the DSN built
// by db.Open rather than being applied once here.

func defaultDBPath() string {
	wd, err := os.Getwd()
	if err != nil {
		return "itsm.db"
	}
	return filepath.Join(wd, "data", "itsm.db")
}

func env(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return n
}

func envDuration(key string, fallback time.Duration) time.Duration {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return fallback
	}
	return d
}

// envBool accepts the spellings people actually type. strconv.ParseBool alone
// rejects "yes", which is a confusing way for a boolean to behave.
func envBool(key string, fallback bool) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	if v == "" {
		return fallback
	}
	switch v {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return fallback
	}
}

func envList(key, fallback string) []string {
	raw := env(key, fallback)
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
