package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/joho/godotenv"

	"tgmail-relay/internal/config"
	"tgmail-relay/internal/imapwatcher"
)

func main() {
	_ = godotenv.Load(".env")

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("load config: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		log.Fatalf("invalid config: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 信号优雅退出
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	// 为每个邮箱启动一个 watcher
	errCh := make(chan error, len(cfg.Mailboxes))
	for i := range cfg.Mailboxes {
		mb := cfg.Mailboxes[i] // 避免闭包坑
		go func() {
			errCh <- imapwatcher.Run(ctx, mb, cfg)
		}()
	}

	// 监控错误 & 信号
	for {
		select {
		case <-stop:
			log.Println("received stop signal, shutting down...")
			cancel()
			time.Sleep(2 * time.Second)
			return
		case err := <-errCh:
			if err != nil {
				log.Printf("watcher error: %v", err)
				// 不退出：保持健壮；可以考虑告警
			}
		}
	}
}
