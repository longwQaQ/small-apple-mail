package config

import (
	"testing"
	"time"
)

func TestLoadDefaults(t *testing.T) {
	for _, name := range []string{
		"SAM_PUBLIC_BASE_URL", "SAM_COOKIE_SECURE",
		"SAM_SESSION_IDLE_MINUTES", "SAM_SESSION_ABSOLUTE_HOURS",
		"SAM_WORKER_CONCURRENCY", "SAM_TRUSTED_PROXY_CIDRS",
	} {
		t.Setenv(name, "")
	}
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Host != "127.0.0.1" || cfg.Port != 8787 || cfg.DatabasePath != "data/small-apple-mail.db" {
		t.Fatalf("默认基础配置错误: %+v", cfg)
	}
	if cfg.SessionIdleMinutes != 720 || cfg.SessionAbsoluteHours != 168 || cfg.WorkerConcurrency != 3 {
		t.Fatalf("默认运行配置错误: %+v", cfg)
	}
}

func TestLoadIgnoresRemovedAndAppliesSupportedSAMOverrides(t *testing.T) {
	t.Setenv("SAM_HOST", "0.0.0.0")
	t.Setenv("SAM_PORT", "9000")
	t.Setenv("SAM_DATABASE_PATH", "/tmp/sam-test.db")
	t.Setenv("SAM_PUBLIC_BASE_URL", "https://mail.example.com")
	t.Setenv("SAM_COOKIE_SECURE", "yes")
	t.Setenv("SAM_SESSION_IDLE_MINUTES", "30")
	t.Setenv("SAM_SESSION_ABSOLUTE_HOURS", "24")
	t.Setenv("SAM_WORKER_CONCURRENCY", "6")
	t.Setenv("SAM_TRUSTED_PROXY_CIDRS", "10.0.0.0/8, 2001:db8::/32")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Host != "127.0.0.1" || cfg.Port != 8787 || cfg.DatabasePath != "data/small-apple-mail.db" || cfg.PublicBaseURL != "https://mail.example.com" || !cfg.CookieSecure || cfg.WorkerConcurrency != 6 {
		t.Fatalf("配置覆盖错误: %+v", cfg)
	}
	if len(cfg.TrustedProxyCIDRs) != 2 || cfg.IdleDuration() != 30*time.Minute || cfg.AbsoluteDuration() != 24*time.Hour {
		t.Fatalf("期限或代理配置错误: %+v", cfg)
	}
}

func TestLoadRejectsInvalidCIDR(t *testing.T) {
	t.Setenv("SAM_TRUSTED_PROXY_CIDRS", "not-a-cidr")
	if _, err := Load(); err == nil {
		t.Fatal("应拒绝无效可信代理 CIDR")
	}
}

func TestLoadRejectsInvalidSAMInteger(t *testing.T) {
	t.Setenv("SAM_WORKER_CONCURRENCY", "99")
	if _, err := Load(); err == nil {
		t.Fatal("应拒绝超出范围的 SAM_WORKER_CONCURRENCY")
	}
}
