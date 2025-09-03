// 文件路径: imapwatcher.go
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
	"github.com/emersion/go-imap/client"

	"tgmail-relay/internal/config"
	"tgmail-relay/internal/extractor"
	"tgmail-relay/internal/state"
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

// isDeadConnErr 仅在IDLE模式下有意义，轮询模式下可以简化，但保留也无妨
func isDeadConnErr(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "broken pipe") ||
		strings.Contains(s, "use of closed network connection") ||
		strings.Contains(s, "connection reset by peer") ||
		strings.Contains(s, "read: connection timed out") ||
		strings.Contains(s, "write: connection timed out") ||
		strings.Contains(s, "imap: connection closed") ||
		strings.Contains(s, "tls: use of closed connection") ||
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

// ★★★ 全新的 Run 函数 (轮询模式) ★★★
func Run(ctx context.Context, mb config.Mailbox, cfg *config.Config) error {
	// 1. 启动时加载状态，这部分逻辑保持不变
	lastSeenUID, err := state.LoadLastUID(mb.Email)
	if err != nil {
		log.Printf("[%s] 严重错误: 无法加载状态文件: %v", mb.Email, err)
		lastSeenUID = 0
	}
	log.Printf("[%s] 从状态文件加载到上次的 UID: %d", mb.Email, lastSeenUID)

	// 2. 设置轮询间隔。我们复用配置中的 IdleFallbackPollSec 字段。
	// 建议设置为 60-120 秒，避免过于频繁。
	pollIntervalSeconds := max(60, cfg.IdleFallbackPollSec)
	pollInterval := time.Duration(pollIntervalSeconds) * time.Second
	log.Printf("[%s] 已切换到短轮询模式，将每 %d 秒检查一次邮件", mb.Email, pollIntervalSeconds)

	// 使用带抖动的 Ticker，避免所有任务在同一时刻执行
	ticker := NewJitterTicker(pollInterval, 5*time.Second)
	defer ticker.Stop()

	// 3. 立即执行一次，不等第一个 ticker 周期
	log.Printf("[%s] 启动后立即执行首次检查...", mb.Email)
	checkMail(ctx, mb, cfg, &lastSeenUID)

	// 4. 进入无限的定时循环
	for {
		select {
		case <-ctx.Done():
			log.Printf("[%s] 收到退出信号，轮询停止", mb.Email)
			return nil
		case <-ticker.C:
			// 每次 Ticker 触发时，执行一次邮件检查
			checkMail(ctx, mb, cfg, &lastSeenUID)
		}
	}
}

// checkMail 是一个独立的辅助函数，包含了完整的“连接->检查->断开”的原子操作
func checkMail(ctx context.Context, mb config.Mailbox, cfg *config.Config, lastUID *uint32) {
	log.Printf("[%s] [轮询] 开始检查新邮件...", mb.Email)

	host := mb.IMAP
	if strings.TrimSpace(host) == "" {
		host = util.GuessIMAPHost(mb.Email)
	}
	serverName := strings.Split(host, ":")[0]

	// Dialer 用于设置连接超时
	dialer := &net.Dialer{Timeout: 15 * time.Second}

	// 每次都建立一个全新的 TLS 连接
	conn, err := tls.DialWithDialer(dialer, "tcp", host, &tls.Config{ServerName: serverName})
	if err != nil {
		log.Printf("[%s] [轮询] 连接失败: %v", mb.Email, err)
		return
	}

	// 使用 client.New 创建客户端
	c, err := client.New(conn)
	if err != nil {
		log.Printf("[%s] [轮询] 创建客户端失败: %v", mb.Email, err)
		conn.Close()
		return
	}
	// ★ 关键：确保操作结束后一定登出并关闭连接
	defer func() {
		log.Printf("[%s] [轮询] 登出并关闭连接", mb.Email)
		c.Logout()
	}()

	if err := c.Login(mb.Email, mb.Password); err != nil {
		log.Printf("[%s] [轮询] 登录失败: %v", mb.Email, err)
		return
	}

	if _, err := c.Select(imap.InboxName, false); err != nil {
		log.Printf("[%s] [轮询] 选择INBOX失败: %v", mb.Email, err)
		return
	}

	// 调用我们之前写好的抓取和状态保存逻辑，完全复用
	if err := fetchAndProcess(ctx, c, mb, cfg, lastUID); err != nil {
		log.Printf("[%s] [轮询] 抓取邮件时出错: %v", mb.Email, err)
	}

	log.Printf("[%s] [轮询] 检查完成", mb.Email)
}

// fetchAndProcess 函数保持不变，它负责调用 fetchAndPush 并保存状态
func fetchAndProcess(ctx context.Context, c *client.Client, mb config.Mailbox, cfg *config.Config, lastUID *uint32) error {
	uidBeforeFetch := *lastUID
	err := fetchAndPush(ctx, c, mb, cfg, lastUID)
	if err != nil {
		*lastUID = uidBeforeFetch
		return err
	}
	if *lastUID > uidBeforeFetch {
		log.Printf("[%s] 新的 UID 是 %d，正在保存到状态文件...", mb.Email, *lastUID)
		if saveErr := state.SaveLastUID(mb.Email, *lastUID); saveErr != nil {
			log.Printf("[%s] 严重警告: 无法保存最新的 UID %d 到文件: %v", mb.Email, *lastUID, saveErr)
		}
	}
	return nil
}

// fetchAndPush 函数保持不变，它负责抓取和推送
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

	var fetchUIDs []uint32
	for _, id := range uids {
		if id > *lastUID {
			fetchUIDs = append(fetchUIDs, id)
		}
	}
	if len(fetchUIDs) == 0 {
		return nil
	}
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

	sort.Slice(summaries, func(i, j int) bool { return summaries[i].Date.Before(summaries[j].Date) })

	tg := telegram.New(cfg.TGBotToken, cfg.TGChatID)
	for _, ms := range summaries {
		html := formatForTG(ms, mb.Name)
		for _, chunk := range splitForTelegram(html) {
			if err := tg.SendHTML(chunk); err != nil {
				log.Printf("[%s] TG push failed uid=%d: %v", mb.Email, ms.UID, err)
			}
		}
		log.Printf("[%s] pushed uid=%d %s", mb.Email, ms.UID, ms.Subject)
	}
	*lastUID = maxUID
	return nil
}

// 其他辅助函数保持不变
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

// NewJitterTicker 创建一个带抖动的 Ticker，避免所有任务同时执行
func NewJitterTicker(d, jitter time.Duration) *time.Ticker {
	// 立即返回一个 Ticker
	// 但其周期将在 [d-jitter, d+jitter] 范围内随机
	ticker := time.NewTicker(d)
	go func() {
		for range ticker.C {
			// 每次触发后，重置 Ticker 的周期
			offset := time.Duration(rand.Int63n(int64(jitter)*2)) - jitter
			nextTick := d + offset
			if nextTick < 1*time.Second {
				nextTick = 1 * time.Second // 避免周期过小
			}
			ticker.Reset(nextTick)
		}
	}()
	return ticker
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
