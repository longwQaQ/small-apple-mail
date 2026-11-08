package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"
	"small-apple-mail/internal/app/config"
	"small-apple-mail/internal/app/domain"
	"small-apple-mail/internal/app/events"
	"small-apple-mail/internal/app/httpserver"
	"small-apple-mail/internal/app/providers"
	"small-apple-mail/internal/app/repository"
	"small-apple-mail/internal/app/security"
	"small-apple-mail/internal/app/worker"
)

var version = "1.0.0"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "操作失败：", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	command := "serve"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		command = args[0]
		args = args[1:]
	}
	switch command {
	case "serve":
		return serve(args)
	case "admin":
		return adminCommand(args)
	case "store":
		return storeCommand(args)
	default:
		return fmt.Errorf("未知命令 %q；可用命令：serve、admin、store", command)
	}
}

func commonFlags(name string, args []string) (config.Config, *flag.FlagSet, error) {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	database := flags.String("database", "", "SQLite 数据库路径")
	if err := flags.Parse(args); err != nil {
		return config.Config{}, flags, err
	}
	cfg, err := config.Load()
	if err != nil {
		return cfg, flags, err
	}
	if *database != "" {
		cfg.DatabasePath = *database
	}
	return cfg, flags, nil
}

func serve(args []string) error {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	database := flags.String("database", "", "SQLite 数据库路径")
	host := flags.String("host", "", "监听地址")
	port := flags.Int("port", 0, "监听端口")
	if err := flags.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if *database != "" {
		cfg.DatabasePath = *database
	}
	if *host != "" {
		cfg.Host = *host
	}
	if *port > 0 {
		cfg.Port = *port
	}
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	store, err := repository.Open(ctx, cfg.DatabasePath)
	if err != nil {
		return err
	}
	defer store.Close()
	if err = store.BootstrapAdmin(ctx, os.Getenv("SAM_ADMIN_USERNAME"), os.Getenv("SAM_ADMIN_PASSWORD")); err != nil {
		return fmt.Errorf("初始化管理员: %w", err)
	}
	hub := events.New()
	pool := worker.New(store, hub, logger, cfg.WorkerConcurrency)
	connections := providers.Register(pool, store, hub)
	if err = pool.Start(ctx); err != nil {
		return err
	}
	defer pool.Stop()
	providers.NewIMAPManager(store, hub).Start(ctx)
	scheduleMaintenance(ctx, store, hub)
	handler := httpserver.New(cfg, store, hub, logger, pool, connections)
	server := &http.Server{Addr: fmt.Sprintf("%s:%d", cfg.Host, cfg.Port), Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 180 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 1 << 20}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	logger.Info("小苹果邮箱已启动", "addr", "http://"+server.Addr, "database", cfg.DatabasePath, "version", version)
	err = server.ListenAndServe()
	if errorsIsServerClosed(err) {
		return nil
	}
	return err
}

func errorsIsServerClosed(err error) bool { return err == nil || err == http.ErrServerClosed }

func scheduleMaintenance(ctx context.Context, store *repository.Store, hub *events.Hub) {
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		lastLeaseSweep := time.Time{}
		_, _ = store.DeleteJobsBefore(ctx, time.Now().UTC().Add(-24*time.Hour))
		lastJobSweep := time.Now()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				maintenance, err := store.EnqueueConnectionMaintenance(ctx)
				if err == nil {
					for _, job := range maintenance {
						hub.Publish("job.created", job)
					}
				}
				jobs, err := store.EnqueueDueSchedules(ctx)
				if err == nil {
					for _, job := range jobs {
						hub.Publish("job.created", job)
					}
				}
				if time.Since(lastLeaseSweep) >= time.Minute {
					expired, err := store.ExpireLeases(ctx)
					if err != nil {
						_ = store.CreateActivity(ctx, domain.Activity{Source: "system", Type: "lease.reclaim", Result: "failed", EntityType: "lease", Detail: err.Error()})
					} else if expired > 0 {
						hub.Publish("lease.changed", map[string]any{"expired": expired})
						_ = store.CreateActivity(ctx, domain.Activity{Source: "system", Type: "lease.reclaim", Result: "success", EntityType: "lease", Detail: fmt.Sprintf("已回收 %d 个过期租约", expired)})
					}
					lastLeaseSweep = time.Now()
				}
				if time.Since(lastJobSweep) >= time.Hour {
					_, _ = store.DeleteJobsBefore(ctx, time.Now().UTC().Add(-24*time.Hour))
					lastJobSweep = time.Now()
				}
			}
		}
	}()
}

func adminCommand(args []string) error {
	if len(args) == 0 || args[0] != "reset-password" {
		return fmt.Errorf("用法：small-apple-mail admin reset-password [--database data/small-apple-mail.db]")
	}
	cfg, _, err := commonFlags("admin reset-password", args[1:])
	if err != nil {
		return err
	}
	ctx := context.Background()
	store, err := repository.Open(ctx, cfg.DatabasePath)
	if err != nil {
		return err
	}
	defer store.Close()
	admin, err := store.Admin(ctx)
	if err != nil {
		return err
	}
	first, err := readSecret("请输入新密码：")
	if err != nil {
		return err
	}
	second, err := readSecret("请再次输入新密码：")
	if err != nil {
		return err
	}
	if first != second {
		return fmt.Errorf("两次输入的密码不一致")
	}
	hash, err := security.HashPassword(first)
	if err != nil {
		return err
	}
	if err = store.ChangePassword(ctx, admin.ID, hash, ""); err != nil {
		return err
	}
	fmt.Println("管理员密码已重置，全部 Web 会话已失效。")
	return nil
}

func storeCommand(args []string) error {
	if len(args) == 0 || args[0] != "verify" {
		return fmt.Errorf("用法：small-apple-mail store verify [--database data/small-apple-mail.db]")
	}
	flags := flag.NewFlagSet("store verify", flag.ContinueOnError)
	database := flags.String("database", "", "SQLite 数据库路径")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if *database != "" {
		cfg.DatabasePath = *database
	}
	ctx := context.Background()
	if _, err := os.Stat(cfg.DatabasePath); err != nil {
		return fmt.Errorf("数据库文件不存在或不可访问: %w", err)
	}
	store, err := repository.Open(ctx, cfg.DatabasePath)
	if err != nil {
		return err
	}
	defer store.Close()
	if err = store.IntegrityCheck(ctx); err != nil {
		return err
	}
	v, _ := store.SchemaVersion(ctx)
	fmt.Printf("数据库校验通过：schema=%d，文件=%s\n", v, cfg.DatabasePath)
	return nil
}

func readSecret(prompt string) (string, error) {
	fmt.Fprint(os.Stderr, prompt)
	data, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	return string(data), err
}
