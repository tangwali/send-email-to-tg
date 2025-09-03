package imapwatcher

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/emersion/go-imap"
	idleExt "github.com/emersion/go-imap-idle"
	"github.com/emersion/go-imap/client"

	"tgmail-relay/internal/config"
	"tgmail-relay/internal/extractor"
	"tgmail-relay/internal/summarizer"
	"tgmail-relay/internal/telegram"
	"tgmail-relay/internal/util"
)

type MailSummary struct {
	UID     uint32
	From    string
	Subject string
	Date    time.Time
	Preview string
}

func isDeadConnErr(err error) bool {
	if err == nil {
		return false
	}
	// 兼容多种实现与不同平台文案
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "broken pipe") ||
		strings.Contains(s, "use of closed network connection") ||
		strings.Contains(s, "connection reset by peer") ||
		strings.Contains(s, "read: connection timed out") ||
		strings.Contains(s, "write: connection timed out") ||
		errors.Is(err, net.ErrClosed)
}

func formatForTG(ms MailSummary, boxName string) string {
	ts := ms.Date.Local().Format("2006-01-02 15:04:05")
	return fmt.Sprintf(
		"<b>%s【%s】</b>\n\n%s\n%s\n\n%s",
		util.EscHTML(boxName),
		util.EscHTML(ms.Subject),
		util.EscHTML(ts),
		util.EscHTML(ms.From),
		util.EscHTML(ms.Preview),
	)
}

func Run(ctx context.Context, mb config.Mailbox, cfg *config.Config) error {
	telegram.New(cfg.TGBotToken, cfg.TGChatID)

	idlePoll := cfg.IdleFallbackPollSec
	if idlePoll < 30 {
		idlePoll = 60
	}
	backoff := cfg.ReconnectBackoff
	if backoff <= 0 {
		backoff = 10 * time.Second
	}

	lastSeenUID := uint32(0)

	host := mb.IMAP
	if strings.TrimSpace(host) == "" {
		host = util.GuessIMAPHost(mb.Email) // 自动推导
	}
	serverName := strings.Split(host, ":")[0]

outer: // ←—— 关键：外层重连标签
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		log.Printf("[%s] connect %s ...", mb.Email, host)

		// 使用自定义 Dialer 打开 TCP KeepAlive 和拨号超时
		dialer := &net.Dialer{
			Timeout:   15 * time.Second,
			KeepAlive: 30 * time.Second,
		}
		tlsCfg := &tls.Config{
			ServerName:         serverName,
			MinVersion:         tls.VersionTLS12,
			InsecureSkipVerify: false,
		}

		// go-imap/client 没有直接暴露 Dialer 版本的 TLS 入口，这里用底层 tls.DialWithDialer
		conn, err := tls.DialWithDialer(dialer, "tcp", host, tlsCfg)
		if err != nil {
			log.Printf("[%s] dial error: %v", mb.Email, err)
			sleep(ctx, backoff)
			backoff = minDuration(backoff*2, 10*time.Minute)
			continue outer
		}

		c, err := client.New(conn)
		if err != nil {
			log.Printf("[%s] make client error: %v", mb.Email, err)
			_ = conn.Close()
			sleep(ctx, backoff)
			backoff = minDuration(backoff*2, 10*time.Minute)
			continue outer
		}
		// 可选：开启 Debug
		// c.SetDebug(os.Stdout)

		// 登录
		if err := c.Login(mb.Email, mb.Password); err != nil {
			_ = c.Logout()
			log.Printf("[%s] login error: %v", mb.Email, err)
			sleep(ctx, backoff)
			backoff = minDuration(backoff*2, 10*time.Minute)
			continue outer
		}
		backoff = cfg.ReconnectBackoff
		log.Printf("[%s] logged in", mb.Email)

		// 选择 INBOX
		if _, err := c.Select(imap.InboxName, false); err != nil {
			_ = c.Logout()
			log.Printf("[%s] select inbox: %v", mb.Email, err)
			sleep(ctx, backoff)
			backoff = minDuration(backoff*2, 10*time.Minute)
			continue outer
		}

		// 启动抓取一次
		if err := fetchAndPush(ctx, c, mb, cfg, &lastSeenUID); err != nil {
			if isDeadConnErr(err) {
				log.Printf("[%s] initial fetch dead conn: %v", mb.Email, err)
				_ = c.Logout()
				sleep(ctx, backoff)
				continue outer // 立刻重连
			}
			log.Printf("[%s] initial fetch error: %v", mb.Email, err)
		}

		// IDLE
		idler := idleExt.NewClient(c)
		updates := make(chan client.Update, 10)
		c.Updates = updates

		keepalive := time.Duration(max(30, cfg.IdleKeepaliveSec)) * time.Second
		startIdle := func(stop <-chan struct{}, errCh chan<- error) {
			go func() {
				errCh <- idler.IdleWithFallback(stop, keepalive)
			}()
		}

		stop := make(chan struct{})
		idleErrCh := make(chan error, 1)
		startIdle(stop, idleErrCh)
		log.Printf("[%s] entering IDLE ...", mb.Email)

	idleLoop:
		for {
			select {
			case <-ctx.Done():
				close(stop)
				_ = c.Logout()
				return nil

			case err := <-idleErrCh:
				// 任何 IDLE 结束都兜底抓一次
				if err != nil {
					log.Printf("[%s] IDLE ended: %v", mb.Email, err)
				} else {
					log.Printf("[%s] IDLE ended normally", mb.Email)
				}
				if err := fetchAndPush(ctx, c, mb, cfg, &lastSeenUID); err != nil {
					if isDeadConnErr(err) {
						log.Printf("[%s] fetch after IDLE dead conn: %v", mb.Email, err)
						_ = c.Logout()
						sleep(ctx, backoff)
						continue outer // 重连
					}
					log.Printf("[%s] fetch after IDLE error: %v", mb.Email, err)
				}
				break idleLoop // 回退到轮询（有些服务商不稳定）

			case u := <-updates:
				// 收到更新：退出本轮 IDLE -> 抓取 -> 重启 IDLE
				select {
				case <-stop:
				default:
					close(stop)
				}
				if err := fetchAndPush(ctx, c, mb, cfg, &lastSeenUID); err != nil {
					if isDeadConnErr(err) {
						log.Printf("[%s] fetch in IDLE dead conn: %v", mb.Email, err)
						_ = c.Logout()
						sleep(ctx, backoff)
						continue outer // 重连
					}
					log.Printf("[%s] fetch in IDLE error: %v", mb.Email, err)
				}
				// 重启 IDLE
				stop = make(chan struct{})
				idleErrCh = make(chan error, 1)
				startIdle(stop, idleErrCh)

				_ = u // 不用具体类型分支，统一处理
			}
		}

		// ========== 轮询回退 ==========
	polling:
		for {
			select {
			case <-ctx.Done():
				_ = c.Logout()
				return nil
			case <-time.After(time.Duration(jitterSec(idlePoll)) * time.Second):
				if err := fetchAndPush(ctx, c, mb, cfg, &lastSeenUID); err != nil {
					log.Printf("[%s] fetch error (polling): %v", mb.Email, err)
					_ = c.Logout()
					sleep(ctx, backoff)
					continue outer // ←—— 关键：真正重连，而不是 break 掉 select
				}
				// 可选：发送 NOOP 作为 keepalive，降低被动超时概率
				if err := c.Noop(); err != nil {
					if isDeadConnErr(err) {
						log.Printf("[%s] NOOP dead conn: %v", mb.Email, err)
						_ = c.Logout()
						sleep(ctx, backoff)
						continue outer
					}
					log.Printf("[%s] NOOP error: %v", mb.Email, err)
				}
				continue polling
			}
		}
	}
}

// fetchAndPush 搜索窗口内新 UID，按需要过滤+摘要+推送
func fetchAndPush(ctx context.Context, c *client.Client, mb config.Mailbox, cfg *config.Config, lastUID *uint32) error {
	crit := imap.NewSearchCriteria()
	cutoff := time.Now().Add(-time.Duration(max(1, mb.WindowHours)) * time.Hour)
	crit.Since = cutoff
	if mb.UnreadOnly {
		crit.WithoutFlags = []string{imap.SeenFlag}
	}
	uids, err := c.UidSearch(crit)
	if err != nil {
		return fmt.Errorf("UidSearch: %w", err)
	}
	if len(uids) == 0 {
		return nil
	}

	// 过滤新 UID
	fetchUIDs := make([]uint32, 0, len(uids))
	for _, id := range uids {
		if id > *lastUID {
			fetchUIDs = append(fetchUIDs, id)
		}
	}
	if len(fetchUIDs) == 0 {
		return nil
	}
	// 升序，截断到 MaxFetchPerBatch
	sort.Slice(fetchUIDs, func(i, j int) bool { return fetchUIDs[i] < fetchUIDs[j] })
	if len(fetchUIDs) > cfg.MaxFetchPerBatch {
		fetchUIDs = fetchUIDs[len(fetchUIDs)-cfg.MaxFetchPerBatch:]
	}

	seq := new(imap.SeqSet)
	seq.AddNum(fetchUIDs...)

	section := &imap.BodySectionName{}
	items := []imap.FetchItem{imap.FetchEnvelope, imap.FetchUid, imap.FetchInternalDate, section.FetchItem()}

	msgCh := make(chan *imap.Message, len(fetchUIDs))
	done := make(chan error, 1)
	go func() {
		done <- c.UidFetch(seq, items, msgCh)
	}()

	var summaries []MailSummary
	maxUID := *lastUID

	for msg := range msgCh {
		if msg == nil || msg.Envelope == nil || msg.Uid == 0 {
			continue
		}

		// 域名白名单过滤
		ok := false
		for _, from := range msg.Envelope.From {
			if util.DomainAllowed(from.HostName, cfg.SenderDomains) {
				ok = true
				break
			}
		}
		if !ok {
			continue
		}
		if msg.Uid > maxUID {
			maxUID = msg.Uid
		}

		from := formatFrom(msg.Envelope.From)
		preview := ""
		if body := msg.GetBody(section); body != nil {
			if text, err := extractor.ExtractTextPreferPlain(body); err == nil {
				preview = extractor.NormalizeSpaces(text)
				preview = extractor.RemoveEmptyLines(preview)
			}
		}

		// AI 摘要（可选）
		if cfg.EnableSummary && cfg.SummaryAPIKey != "" {
			mailCtx := fmt.Sprintf("主题: %s\n发件人: %s\n时间: %s\n\n正文:\n%s",
				strings.TrimSpace(msg.Envelope.Subject),
				from,
				msg.InternalDate.Local().Format("2006-01-02 15:04:05"),
				preview,
			)
			preview = summarizer.Summarize(ctx, summarizer.Config{
				APIKey: cfg.SummaryAPIKey,
				Model:  cfg.SummaryModel,
				Enable: true,
			}, mailCtx)
			preview = extractor.NormalizeSpaces(preview)
			preview = extractor.RemoveEmptyLines(preview)
		}
		// 兜底截断（防止 TG 4096 限制）
		if len([]rune(preview)) > 3800 {
			preview = string([]rune(preview)[:3800]) + "…"
		}

		summaries = append(summaries, MailSummary{
			UID:     msg.Uid,
			From:    from,
			Subject: msg.Envelope.Subject,
			Date:    msg.InternalDate,
			Preview: preview,
		})
	}

	if err := <-done; err != nil {
		return fmt.Errorf("UidFetch: %w", err)
	}

	// 最终按 Date 排序（更贴近用户感知）
	sort.Slice(summaries, func(i, j int) bool { return summaries[i].Date.Before(summaries[j].Date) })

	// 推送
	tg := telegram.New(cfg.TGBotToken, cfg.TGChatID)
	for _, ms := range summaries {
		html := formatForTG(ms, mb.Name)
		for _, chunk := range splitForTelegram(html) {
			if err := tg.SendHTML(chunk); err != nil {
				log.Printf("[%s] TG push failed uid=%d: %v", mb.Email, ms.UID, err)
				break
			}
		}
		log.Printf("[%s] pushed uid=%d %s", mb.Email, ms.UID, ms.Subject)
	}
	*lastUID = maxUID
	return nil
}

func formatFrom(addrs []*imap.Address) string {
	if len(addrs) == 0 {
		return "(unknown)"
	}
	a := addrs[0]
	return fmt.Sprintf("%s@%s", a.MailboxName, a.HostName)
}

func splitForTelegram(s string) []string {
	const max = 4096
	rs := []rune(s)
	if len(rs) <= max {
		return []string{s}
	}
	var parts []string
	start := 0
	for start < len(rs) {
		end := start + max
		if end > len(rs) {
			end = len(rs)
		} else {
			k := end
			for i := end - 1; i > start && i > end-400; i-- {
				if rs[i] == '\n' {
					k = i + 1
					break
				}
			}
			end = k
		}
		parts = append(parts, string(rs[start:end]))
		start = end
	}
	return parts
}

func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

func jitterSec(n int) int {
	if n <= 2 {
		return n
	}
	return n - 5 + rand.Intn(10) // ±5 秒抖动，避免整点风暴
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
