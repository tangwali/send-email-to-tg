package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
	"github.com/emersion/go-message/mail"
	"github.com/google/generative-ai-go/genai"
	"github.com/joho/godotenv"
	"golang.org/x/net/html"
	"google.golang.org/api/option"
)

/*
- GitHub Actions：一次性执行；不使用本地状态（不做 UID 去重）
- 搜索：最近 WINDOW_HOURS（默认1小时） + （可选）仅未读 UNREAD_ONLY=1
- 过滤：发件域名在 SENDER_DOMAINS 白名单（含子域）
- 正文：优先 text/plain；否则解析 text/html -> 纯文本（忽略 script/style，压缩空白）
- 摘要：配置 OPENAI_API_KEY 时使用 Gemini 做摘要；最终按 4096 字分块发送
*/

type Env struct {
	Mail1Email string
	Mail1Pass  string
	Mail1IMAP  string

	Mail2Email string
	Mail2Pass  string
	Mail2IMAP  string

	TGBotToken string
	TGChatID   string

	SenderDomains []string
	WindowHours   int
	UnreadOnly    bool
	ApiKey        string
	Model         string
}

type Mailbox struct {
	Email string
	Pass  string
	IMAP  string
	Name  string
}

func guessIMAPHost(email string) string {
	parts := strings.SplitN(email, "@", 2)
	if len(parts) != 2 {
		return "imap.gmail.com:993"
	}
	domain := strings.ToLower(parts[1])
	switch domain {
	case "gmail.com", "googlemail.com":
		return "imap.gmail.com:993"
	case "qq.com":
		return "imap.qq.com:993"
	case "163.com":
		return "imap.163.com:993"
	default:
		return "imap." + domain + ":993"
	}
}

func escHTML(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	return r.Replace(s)
}

func formatForTG(ms MailSummary, boxName string) string {
	ts := ms.Date.Local().Format("2006-01-02 15:04:05")
	// 可爱版本：品牌 + 主题；发件人 + 时间；空一行；正文
	return fmt.Sprintf(
		"<b>%s【%s】</b>\n\n%s\n%s\n\n%s",
		escHTML(boxName),
		escHTML(ms.Subject),
		escHTML(ts), // 这里使用 ms.Date
		escHTML(ms.From),
		escHTML(ms.Preview),
	)
}

func tgSend(token, chatID, html string) error {
	api := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", token)
	form := url.Values{}
	form.Set("chat_id", chatID)
	form.Set("text", html)
	form.Set("parse_mode", "HTML")
	form.Set("disable_web_page_preview", "true")
	resp, err := http.PostForm(api, form)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("telegram %s: %s", resp.Status, string(b))
	}
	return nil
}

func normalizeSpaces(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = strings.Join(strings.Fields(lines[i]), " ")
	}
	// 压缩多空行
	out := []string{}
	prevBlank := false
	for _, ln := range lines {
		blank := strings.TrimSpace(ln) == ""
		if blank && prevBlank {
			continue
		}
		out = append(out, ln)
		prevBlank = blank
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

// HTML -> 纯文本：忽略 script/style，压缩空白
func htmlToText(r io.Reader) string {
	doc, err := html.Parse(r)
	if err != nil {
		return ""
	}
	var sb strings.Builder
	var walk func(*html.Node, bool)
	walk = func(n *html.Node, inPre bool) {
		switch n.Type {
		case html.ElementNode:
			if n.Data == "script" || n.Data == "style" {
				return
			}
			if n.Data == "p" || n.Data == "div" || n.Data == "br" || n.Data == "li" || n.Data == "tr" || n.Data == "h1" || n.Data == "h2" || n.Data == "h3" {
				sb.WriteString("\n")
			}
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				walk(c, inPre || n.Data == "pre")
			}
			if n.Data == "p" || n.Data == "div" || n.Data == "li" || n.Data == "tr" {
				sb.WriteString("\n")
			}
		case html.TextNode:
			t := n.Data
			if !inPre {
				t = normalizeSpaces(t)
			}
			if t != "" {
				sb.WriteString(t)
			}
		default:
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				walk(c, inPre)
			}
		}
	}
	walk(doc, false)
	return strings.TrimSpace(normalizeSpaces(sb.String()))
}

func extractTextPlain(r io.Reader) (string, error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return "", err
	}
	// 1) text/plain
	{
		mr, err := mail.CreateReader(bytes.NewReader(raw))
		if err == nil && mr != nil {
			for {
				p, err := mr.NextPart()
				if err != nil {
					if err == io.EOF {
						break
					}
					return "", err
				}
				if h, ok := p.Header.(*mail.InlineHeader); ok {
					if mt, _, _ := h.ContentType(); strings.HasPrefix(strings.ToLower(mt), "text/plain") {
						if b, _ := io.ReadAll(p.Body); len(b) > 0 {
							return normalizeSpaces(string(b)), nil
						}
					}
				}
			}
		}
	}
	// 2) text/html
	{
		mr, err := mail.CreateReader(bytes.NewReader(raw))
		if err == nil && mr != nil {
			for {
				p, err := mr.NextPart()
				if err != nil {
					if err == io.EOF {
						break
					}
					return "", err
				}
				if h, ok := p.Header.(*mail.InlineHeader); ok {
					if mt, _, _ := h.ContentType(); strings.HasPrefix(strings.ToLower(mt), "text/html") {
						if b, _ := io.ReadAll(p.Body); len(b) > 0 {
							return htmlToText(bytes.NewReader(b)), nil
						}
					}
				}
			}
		}
	}
	// 3) 兜底：首部件
	if mr, err := mail.CreateReader(bytes.NewReader(raw)); err == nil && mr != nil {
		if p, err := mr.NextPart(); err == nil && p != nil {
			if b, _ := io.ReadAll(p.Body); len(b) > 0 {
				return normalizeSpaces(string(b)), nil
			}
		}
	}
	return "", nil
}

// summarizeWithAI 使用 Google Gemini 对邮件文本进行摘要。
// 当 API 调用失败时，它会自动进行重试。
// apiKey: 从环境变量等配置中获取（使用 OPENAI_API_KEY 存放 Google AI Key）。
func summarizeWithAI(ctx context.Context, text string, apiKey string) string {
	const maxRetries = 5                   // 尝试次数更充足
	const initialBackoff = 5 * time.Second // 初始退避时间更长

	// 1. 预检查：API Key 是否存在
	if apiKey == "" {
		// 这是一个配置问题，不需要重试，直接返回
		log.Println("警告: OPENAI_API_KEY 未设置，跳过AI摘要。")
		return text
	}

	// 2. 创建 Gemini 客户端 (只需创建一次)
	client, err := genai.NewClient(ctx, option.WithAPIKey(apiKey))
	if err != nil {
		log.Printf("错误: 无法创建 Gemini 客户端: %v", err)
		return text
	}
	defer client.Close()

	// 3. 配置模型和提示 (只需配置一次)

	model := client.GenerativeModel("gemini-2.0-flash")
	model.GenerationConfig = genai.GenerationConfig{
		Temperature: genai.Ptr(float32(0.2)),
	}

	prompt := fmt.Sprintf(
		"你是一位可爱风格的资深运营助理，请把下面的完整邮件信息整理成中文正文，要求："+
			"1) 采用可爱轻松的语气和合理使用颜文字emoji"+
			"2) 使用清晰标签逐行列出关键信息需要简洁精炼只需要关键信息注意！只要关注主要信息！！！不需要帮助信息 设置信息 以及温馨提示！！！ "+
			"3) 仅使用换行分隔条目，不要出现空白段落；"+
			"4) 不要使用 Markdown/HTML，不要粘贴跟踪链接；"+
			"5) 总长度不超过 800 字；"+
			"6) 注意：输出只包含摘要正文，不要重复发件人或日期抬头。\n\n邮件内容：\n%s",
		text,
	)

	// --- 4. [新增] 带重试机制的循环 ---
	var lastErr error
	for attempt := 1; attempt <= maxRetries; attempt++ {
		// 调用 API 生成内容
		resp, err := model.GenerateContent(ctx, genai.Text(prompt))
		if err == nil {
			// --- 成功路径 ---
			var summary strings.Builder
			if len(resp.Candidates) > 0 {
				for _, part := range resp.Candidates[0].Content.Parts {
					if txt, ok := part.(genai.Text); ok {
						summary.WriteString(string(txt))
					}
				}
			}

			if summary.Len() > 0 {
				// 成功获取到内容，直接返回
				return strings.TrimSpace(summary.String())
			}

			// API调用成功，但未返回内容，这种情况通常不需要重试
			log.Println("警告: AI 调用成功，但未能返回有效内容。")
			return text // 直接返回原文
		}

		// --- 失败路径 ---
		lastErr = err
		log.Printf("错误: 内容生成失败 (尝试 %d/%d): %v", attempt, maxRetries, err)

		// 如果不是最后一次尝试，则等待后重试
		if attempt < maxRetries {
			// 指数退避：5s,10s,20s,40s ...
			backoffDuration := time.Duration(1<<uint(attempt-1)) * initialBackoff
			log.Printf("将在 %v 后重试...", backoffDuration)

			// 使用 select 实现上下文感知的等待
			select {
			case <-time.After(backoffDuration):
				// 等待结束，继续下一次循环
				continue
			case <-ctx.Done():
				// 在等待期间，上层 context 被取消
				log.Printf("错误: Context 在重试等待期间被取消: %v", ctx.Err())
				return text // 立即中止并返回原文
			}
		}
	}

	// --- 所有重试都失败后 ---
	log.Printf("错误: 所有 %d 次尝试均失败，最终错误: %v", maxRetries, lastErr)
	return text // 返回原文作为兜底
}

// Telegram 单条消息最大约 4096 字符，按换行优先分块
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
			// 优先在段尾换行处截断，增强可读性
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

func domainAllowed(host string, allow []string) bool {
	h := strings.ToLower(strings.TrimSpace(host))
	for _, d := range allow {
		d = strings.ToLower(strings.TrimSpace(d))
		if d == "" {
			continue
		}
		if h == d || strings.HasSuffix(h, "."+d) {
			return true
		}
	}
	return false
}

// searchParams 结构体保持不变
type searchParams struct {
	windowHours int
	unreadOnly  bool
}

// MailSummary 结构体保持不变（假设存在）
type MailSummary struct {
	UID     uint32
	From    string
	Subject string
	Date    time.Time // 添加 InternalDate 字段
	Preview string
}

// 辅助函数 domainAllowed, guessIMAPHost, extractTextPlain, normalizeSpaces, summarizeWithAI 保持不变（假设存在）

// --- 优化后的 fetchLatest 函数 ---

// fetchLatest 从指定的邮箱中获取邮件。
// 它首先在服务器上搜索最近 windowHours 内的邮件，然后获取其中最新的 limit 封，
// 并对这些邮件进行处理（域名过滤、AI摘要等）。
func fetchLatest(ctx context.Context, mb Mailbox, sinceUID uint32, limit int, allowDomains []string, params searchParams, apikey string) ([]MailSummary, uint32, error) {
	// --- 1. 连接和登录IMAP服务器 (逻辑不变) ---
	host := mb.IMAP
	if host == "" {
		host = guessIMAPHost(mb.Email)
	}
	c, err := client.DialTLS(host, &tls.Config{ServerName: strings.Split(host, ":")[0]})
	if err != nil {
		return nil, sinceUID, fmt.Errorf("imap dial %s: %w", host, err)
	}
	defer c.Logout()

	if err := c.Login(mb.Email, mb.Pass); err != nil {
		return nil, sinceUID, fmt.Errorf("imap login: %w", err)
	}
	if _, err = c.Select(imap.InboxName, false); err != nil {
		return nil, sinceUID, fmt.Errorf("select INBOX: %w", err)
	}

	// --- 2. [优化] 高效搜索符合条件的邮件UID ---
	crit := imap.NewSearchCriteria()
	if params.windowHours <= 0 {
		params.windowHours = 1 // 默认至少1小时
	}
	// 在服务器端按时间窗口进行搜索，这是最高效的方式
	searchCutoff := time.Now().Add(-time.Duration(params.windowHours) * time.Hour)
	crit.Since = searchCutoff // 在服务器端进行初步筛选

	if params.unreadOnly {
		crit.WithoutFlags = []string{imap.SeenFlag}
	}

	uids, err := c.UidSearch(crit)
	if err != nil {
		return nil, sinceUID, fmt.Errorf("UidSearch: %w", err)
	}
	if len(uids) == 0 {
		return nil, sinceUID, nil // 没有在时间窗口内的新邮件
	}

	// --- 3. [优化] 过滤、排序并限制UID数量 ---
	// a. 过滤掉已经处理过的邮件 (UID <= sinceUID)
	uidsToFetch := make([]uint32, 0, len(uids))
	for _, id := range uids {
		if id > sinceUID {
			uidsToFetch = append(uidsToFetch, id)
		}
	}

	if len(uidsToFetch) == 0 {
		return nil, sinceUID, nil // 没有比上次更新的邮件
	}

	// b. 对UID进行排序（UID越大越新），确保我们能取到最新的
	sort.Slice(uidsToFetch, func(i, j int) bool { return uidsToFetch[i] < uidsToFetch[j] })

	// c. 如果结果多于limit，只保留最新的 limit 条
	if len(uidsToFetch) > limit {
		uidsToFetch = uidsToFetch[len(uidsToFetch)-limit:]
	}

	// --- 4. 批量抓取已筛选出的邮件内容 (逻辑不变) ---
	seq := new(imap.SeqSet)
	seq.AddNum(uidsToFetch...)

	section := &imap.BodySectionName{}
	// 需要 INTERNALDATE 用于按服务器接收时间过滤
	items := []imap.FetchItem{imap.FetchEnvelope, imap.FetchUid, imap.FetchInternalDate, section.FetchItem()}

	messages := make(chan *imap.Message, len(uidsToFetch))
	done := make(chan error, 1)
	go func() {
		done <- c.UidFetch(seq, items, messages)
	}()

	var res []MailSummary
	maxUID := sinceUID

	// --- 5. 处理每封邮件 (逻辑不变) ---
	for msg := range messages {
		if msg == nil || msg.Envelope == nil || msg.Uid == 0 {
			continue
		}

		// 再次检查服务器接收时间，确保严格在 windowHours 内
		if msg.InternalDate.Before(searchCutoff) {
			continue // 跳过早于窗口时间的邮件
		}

		// 域名白名单过滤
		isAllowed := false
		for _, fromAddr := range msg.Envelope.From {
			if domainAllowed(fromAddr.HostName, allowDomains) {
				isAllowed = true
				break
			}
		}
		if !isAllowed {
			continue
		}

		if msg.Uid > maxUID {
			maxUID = msg.Uid
		}

		from := formatFromAddress(msg.Envelope.From)

		preview := ""
		if body := msg.GetBody(section); body != nil {
			if text, err := extractTextPlain(body); err == nil {
				preview = normalizeSpaces(text)
				preview = removeEmptyLines(preview)
			}
		}

		// AI摘要 (可选) —— 将主题/发件人/时间与正文一起交给 AI，产出“可爱、带标签”的摘要正文
		if apikey != "" {
			mailCtx := fmt.Sprintf("主题: %s\n发件人: %s\n时间: %s\n\n正文:\n%s",
				strings.TrimSpace(msg.Envelope.Subject),
				formatFromAddress(msg.Envelope.From),
				msg.InternalDate.Local().Format("2006-01-02 15:04:05"), // 使用 InternalDate
				preview,
			)
			preview = summarizeWithAI(ctx, mailCtx, apikey)
			// 统一压缩空白并移除空行，避免两行之间出现额外空白
			preview = normalizeSpaces(preview)
			preview = removeEmptyLines(preview)
		}

		// 兜底截断
		if len([]rune(preview)) > 3800 {
			preview = string([]rune(preview)[:3800]) + "…"
		}

		res = append(res, MailSummary{
			UID:     msg.Uid,
			From:    from,
			Subject: msg.Envelope.Subject,
			Date:    msg.InternalDate, // 使用 InternalDate
			Preview: preview,
		})
	}

	if err := <-done; err != nil {
		return nil, sinceUID, fmt.Errorf("UidFetch failed: %w", err)
	}

	// --- 6. [优化] 按邮件头中的Date字段进行最终排序 ---
	// UID代表到达顺序，但邮件本身的Date头可能不同。按Date排序对用户更友好。
	sort.Slice(res, func(i, j int) bool { return res[i].Date.Before(res[j].Date) })

	return res, maxUID, nil
}

// 辅助函数：格式化发件人地址 (从原逻辑中提取)
func formatFromAddress(addrs []*imap.Address) string {
	// 仅返回邮箱地址，不包含显示名
	if len(addrs) == 0 {
		return "(unknown)"
	}
	a := addrs[0]
	return fmt.Sprintf("%s@%s", a.MailboxName, a.HostName)
}

// removeEmptyLines 去掉所有空白行，确保行与行之间没有多余的空行
func removeEmptyLines(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	for _, ln := range lines {
		if strings.TrimSpace(ln) == "" {
			continue
		}
		out = append(out, ln)
	}
	return strings.Join(out, "\n")
}

func parseBoolEnv(key string, def bool) bool {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	switch strings.ToLower(v) {
	case "1", "true", "yes", "y", "on":
		return true
	case "0", "false", "no", "n", "off":
		return false
	default:
		return def
	}
}

func parseIntEnv(key string, def int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	if x, err := strconv.Atoi(v); err == nil {
		return x
	}
	return def
}

func main() {
	_ = godotenv.Load(".env")

	env := Env{
		Mail1Email:  os.Getenv("QQ_EMAIL"),
		Mail1Pass:   os.Getenv("QQ_EMAIL_AUTH_CODE"),
		Mail1IMAP:   os.Getenv("QQ_IMAP_HOST"),
		Mail2Email:  os.Getenv("GMAIL_EMAIL"),
		Mail2Pass:   os.Getenv("GMAIL_APP_PASSWORD"),
		Mail2IMAP:   os.Getenv("GMAIL_IMAP_HOST"),
		TGBotToken:  os.Getenv("TELEGRAM_BOT_TOKEN"),
		TGChatID:    os.Getenv("TELEGRAM_CHAT_ID"),
		WindowHours: parseIntEnv("WINDOW_HOURS", 1),
		UnreadOnly:  parseBoolEnv("UNREAD_ONLY", true),
		ApiKey:      os.Getenv("OPENAI_API_KEY"),
	}
	// 发件域白名单
	domains := strings.Split(os.Getenv("SENDER_DOMAINS"), ",")
	if len(domains) == 0 || (len(domains) == 1 && strings.TrimSpace(domains[0]) == "") {
		// 默认仅 amazon.com
		domains = []string{"amazon.com"}
	}
	for i := range domains {
		domains[i] = strings.ToLower(strings.TrimSpace(domains[i]))
	}
	env.SenderDomains = domains

	if env.TGBotToken == "" || env.TGChatID == "" {
		log.Fatal("缺少 TELEGRAM_BOT_TOKEN / TELEGRAM_CHAT_ID")
	}

	var mbs []Mailbox
	if env.Mail1Email != "" && env.Mail1Pass != "" {
		mbs = append(mbs, Mailbox{Email: env.Mail1Email, Pass: env.Mail1Pass, IMAP: env.Mail1IMAP, Name: "TwaliRay"})
	}
	if env.Mail2Email != "" && env.Mail2Pass != "" {
		mbs = append(mbs, Mailbox{Email: env.Mail2Email, Pass: env.Mail2Pass, IMAP: env.Mail2IMAP, Name: "Qiaxiluxe"})
	}
	if len(mbs) == 0 {
		log.Fatal("未配置任何邮箱凭据（QQ 或 Gmail 至少一个）")
	}

	runOnce := func() {
		ctx := context.Background()
		for _, mb := range mbs {
			func() {
				var since uint32 = 0 // 一次性运行：不做 UID 去重
				msgs, maxUID, err := fetchLatest(
					ctx,
					mb,
					since,
					80, // 每次最多取 80
					env.SenderDomains,
					searchParams{windowHours: env.WindowHours, unreadOnly: env.UnreadOnly},
					env.ApiKey,
				)
				if err != nil {
					log.Printf("[%s] 拉取失败: %v", mb.Email, err)
					return
				}
				if len(msgs) == 0 {
					log.Printf("[%s] 无匹配新邮件", mb.Email)
					return
				}
				for _, ms := range msgs {
					html := formatForTG(ms, mb.Name)
					for _, chunk := range splitForTelegram(html) {
						if err := tgSend(env.TGBotToken, env.TGChatID, chunk); err != nil {
							log.Printf("[%s] 推送失败 UID=%d: %v", mb.Email, ms.UID, err)
							break
						}
					}
					log.Printf("[%s] 已推送 UID=%d %s", mb.Email, ms.UID, ms.Subject)
				}
				_ = maxUID // 单次运行无需保存状态
			}()
		}
	}
	// GitHub Action: 一次性执行并退出
	runOnce()
}
