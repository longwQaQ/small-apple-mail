package main

import (
	"strings"
	"testing"
)

func TestCommonFlagsUsesDefaultAndDatabaseOverride(t *testing.T) {
	cfg, _, err := commonFlags("test", nil)
	if err != nil || cfg.DatabasePath != "data/small-apple-mail.db" {
		t.Fatalf("默认数据库配置错误: cfg=%+v err=%v", cfg, err)
	}
	cfg, _, err = commonFlags("test", []string{"--database", "data/from-flag.db"})
	if err != nil || cfg.DatabasePath != "data/from-flag.db" {
		t.Fatalf("命令参数覆盖未生效: cfg=%+v err=%v", cfg, err)
	}
}

func TestCommandsRejectRemovedConfigFlag(t *testing.T) {
	for name, run := range map[string]func() error{
		"serve": func() error { return serve([]string{"--config", "config.json"}) },
		"admin": func() error {
			_, _, err := commonFlags("admin reset-password", []string{"--config", "config.json"})
			return err
		},
		"store": func() error { return storeCommand([]string{"verify", "--config", "config.json"}) },
	} {
		t.Run(name, func(t *testing.T) {
			err := run()
			if err == nil || !strings.Contains(err.Error(), "flag provided but not defined") {
				t.Fatalf("应拒绝已删除的 --config 参数: %v", err)
			}
		})
	}
}

func TestStoreCommandOnlySupportsVerify(t *testing.T) {
	for _, action := range []string{"backup", "restore"} {
		if err := storeCommand([]string{action}); err == nil || !strings.Contains(err.Error(), "store verify") {
			t.Fatalf("action=%s err=%v", action, err)
		}
	}
}
