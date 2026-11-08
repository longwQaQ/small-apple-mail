package config

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Host                 string
	Port                 int
	DatabasePath         string
	PublicBaseURL        string
	CookieSecure         bool
	SessionIdleMinutes   int
	SessionAbsoluteHours int
	WorkerConcurrency    int
	TrustedProxyCIDRs    []string
}

func Load() (Config, error) {
	cfg := Config{Host: "127.0.0.1", Port: 8787, DatabasePath: filepath.Join("data", "small-apple-mail.db"), SessionIdleMinutes: 720, SessionAbsoluteHours: 168, WorkerConcurrency: 3}
	setString(&cfg.PublicBaseURL, "SAM_PUBLIC_BASE_URL")
	for _, item := range []struct {
		dst      *int
		name     string
		min, max int
	}{
		{&cfg.SessionIdleMinutes, "SAM_SESSION_IDLE_MINUTES", 5, 43200},
		{&cfg.SessionAbsoluteHours, "SAM_SESSION_ABSOLUTE_HOURS", 1, 8760},
		{&cfg.WorkerConcurrency, "SAM_WORKER_CONCURRENCY", 1, 16},
	} {
		if err := setInt(item.dst, item.name, item.min, item.max); err != nil {
			return cfg, err
		}
	}
	if value := strings.TrimSpace(os.Getenv("SAM_TRUSTED_PROXY_CIDRS")); value != "" {
		cfg.TrustedProxyCIDRs = splitNonEmpty(value)
	}
	if value := strings.TrimSpace(os.Getenv("SAM_COOKIE_SECURE")); value != "" {
		cfg.CookieSecure = isTrue(value)
	}
	if cfg.DatabasePath == "" {
		return cfg, fmt.Errorf("database_path 不能为空")
	}
	for _, item := range []struct {
		value    int
		name     string
		min, max int
	}{
		{cfg.Port, "port", 1, 65535},
		{cfg.SessionIdleMinutes, "session_idle_minutes", 5, 43200},
		{cfg.SessionAbsoluteHours, "session_absolute_hours", 1, 8760},
		{cfg.WorkerConcurrency, "worker_concurrency", 1, 16},
	} {
		if item.value < item.min || item.value > item.max {
			return cfg, fmt.Errorf("%s 必须在 %d-%d 之间", item.name, item.min, item.max)
		}
	}
	for _, cidr := range cfg.TrustedProxyCIDRs {
		if _, _, err := net.ParseCIDR(cidr); err != nil {
			return cfg, fmt.Errorf("trusted_proxy_cidrs 包含无效 CIDR %q", cidr)
		}
	}
	return cfg, nil
}

func splitNonEmpty(value string) []string {
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func setString(dst *string, name string) {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		*dst = v
	}
}
func setInt(dst *int, name string, min, max int) error {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < min || n > max {
		return fmt.Errorf("%s 必须是 %d-%d 之间的整数", name, min, max)
	}
	*dst = n
	return nil
}
func isTrue(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
func (c Config) IdleDuration() time.Duration {
	return time.Duration(c.SessionIdleMinutes) * time.Minute
}
func (c Config) AbsoluteDuration() time.Duration {
	return time.Duration(c.SessionAbsoluteHours) * time.Hour
}
