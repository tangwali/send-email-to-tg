package main

import (
	"context"
	"log"
	"math/rand"
	"os"
	"os/signal"
	"runtime/debug"
	"sync"
	"syscall"
	"time"

	"github.com/joho/godotenv"

	"tgmail-relay/internal/config"
	"tgmail-relay/internal/imapwatcher"
)

type backoff struct {
	cur time.Duration
	max time.Duration
	min time.Duration
}

func (b *backoff) next() time.Duration {
	if b.cur == 0 {
		b.cur = b.min
	} else {
		b.cur *= 2
		if b.cur > b.max {
			b.cur = b.max
		}
	}
	// ±20% 抖动，避免雪崩重连
	jitter := time.Duration(rand.Int63n(int64(b.cur) / 5))
	if rand.Intn(2) == 0 {
		return b.cur - jitter
	}
	return b.cur + jitter
}

func (b *backoff) reset() { b.cur = 0 }

func main() {
	_ = godotenv.Load(".env")

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("load config: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		log.Fatalf("invalid config: %v", err)
	}

	// 进程级 context + 优雅退出
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	// 每个邮箱一个 watcher + 监督循环
	var wg sync.WaitGroup
	wg.Add(len(cfg.Mailboxes))

	for i := range cfg.Mailboxes {
		mb := cfg.Mailboxes[i]
		go func() {
			defer wg.Done()

			mbCtx, mbCancel := context.WithCancel(ctx)
			defer mbCancel()

			bo := backoff{
				min: 3 * time.Second,
				max: 10 * time.Minute,
			}

			for {
				select {
				case <-mbCtx.Done():
					return
				default:
				}

				// panic 保护，防止单个邮箱崩溃拉跨整个进程
				func() {
					defer func() {
						if r := recover(); r != nil {
							log.Printf("[%s] watcher panic: %v\n%s", mb.Email, r, string(debug.Stack()))
						}
					}()
					log.Printf("[%s] watcher starting...", mb.Email)
					// Run 内部会自循环并在需要时重连；只有在 ctx 取消或内部决定退出时才返回
					if err := imapwatcher.Run(mbCtx, mb, cfg); err != nil {
						log.Printf("[%s] watcher returned error: %v", mb.Email, err)
					} else {
						log.Printf("[%s] watcher exited without error", mb.Email)
					}
				}()

				// 若是正常收到上层 ctx 取消，就别重启了
				if mbCtx.Err() != nil || ctx.Err() != nil {
					return
				}

				// 否则指数退避后重启
				d := bo.next()
				log.Printf("[%s] restarting after %s ...", mb.Email, d)
				timer := time.NewTimer(d)
				select {
				case <-mbCtx.Done():
					timer.Stop()
					return
				case <-timer.C:
				}
			}
		}()
	}

	// 主循环：等信号
	select {
	case sig := <-stop:
		log.Printf("received signal %s, shutting down...", sig)
		cancel()
	case <-ctx.Done():
	}

	// 等所有 watcher 收尾
	waitDone := make(chan struct{})
	go func() {
		wg.Wait()
		close(waitDone)
	}()

	// 给 5s 收尾缓冲
	select {
	case <-waitDone:
	case <-time.After(5 * time.Second):
	}

	log.Println("bye.")
}
