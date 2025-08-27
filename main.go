package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
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
行为概览
- 本地：POLL_SECONDS>0 时轮询；=0 时一次性运行；可写 state/state.json 做 UID 去重
- GitHub Actions：默认一次性执行 (POLL_SECONDS=0) + 禁用 state（DISABLE_STATE=1）
- 搜索：最近 WINDOW_HOURS（默认2小时） + （可选）仅未读 UNREAD_ONLY=1
- 过滤：发件域名在 SENDER_DOMAINS 白名单（含子域）
- 正文：优先 text/plain；否则解析 text/html -> 纯文本（忽略 script/style，压缩空白）
- 超长：>1200 字尝试调用 OpenAI 摘要（OPENAI_API_KEY），最终按 4096 字分块发送
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

	PollSeconds  int
	DisableState bool

	SenderDomains []string
	WindowHours   int
	UnreadOnly    bool
	ApiKey        string
}

type State struct {
	LastUID map[string]uint32 `json:"last_uid"`
}

func loadState(path string) (*State, error) {
	st := &State{LastUID: map[string]uint32{}}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return st, nil
		}
		return nil, err
	}
	defer f.Close()
	_ = json.NewDecoder(f).Decode(st)
	return st, nil
}

func saveState(path string, st *State) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := json.NewEncoder(f).Encode(st); err != nil {
		return err
	}
	return os.Rename(tmp, path)
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

type MailSummary struct {
	UID     uint32
	From    string
	Subject string
	Date    time.Time
	Preview string
}

func escHTML(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	return r.Replace(s)
}

func formatForTG(ms MailSummary, boxName string) string {
	ts := ms.Date.Local().Format("2006-01-02 15:04:05")
	// 简洁可读：顶部关键信息 + 正文
	return fmt.Sprintf(
		"<b>[%s]</b>\n<b>From:</b> %s\n<b>Subject:</b> %s\n<b>Date:</b> %s\n\n%s",
		escHTML(boxName),
		escHTML(ms.From),
		escHTML(ms.Subject),
		escHTML(ts),
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

// AI 摘要（可选）：设置 OPENAI_API_KEY 时启用；失败则返回原文
func summarizeWithAI(ctx context.Context, text string, apiKey string) string {
	// 1. 从环境变量获取 Google AI API Key
	if apiKey == "" {
		fmt.Println("警告: GOOGLE_API_KEY 环境变量未设置。")
		return text
	}

	// 2. 使用 API Key 初始化 Gemini 客户端
	// SDK 会处理所有底层的 HTTP 连接和认证
	client, err := genai.NewClient(ctx, option.WithAPIKey(apiKey))
	if err != nil {
		log.Printf("错误: 无法创建 Gemini 客户端: %v", err)
		return text
	}
	defer client.Close()

	// 3.选择要使用的模型，并设置生成配置
	model := client.GenerativeModel("gemini-1.5-flash-latest")
	model.GenerationConfig = genai.GenerationConfig{
		Temperature: genai.Ptr(float32(0.2)),
	}

	// 4. 构建提示 (Prompt)
	// 这里的 prompt 和之前完全一样，只是传递方式更简单
	prompt := fmt.Sprintf(
		"你是一位可爱的资深运营助理，非常擅长将冗长复杂的邮件内容提炼为清晰、可执行的操作可爱清淡清单（emjoil和颜文字）。请你用简洁可爱的中文来回答"+
			"请把下面的邮件正文压缩为一份不超过800字的纯文本摘（这篇摘要需要发送到tg），摘要中必须保留以下关键信息："+
			"主题要点、任何相关的订单/货件/工单编号、具体的时间和地点、已知的异常原因、需要团队处理的具体动作、以及明确的截止时间。"+
			"请确保去掉所有不必要的客套话、装饰性语句和跟踪链接，只保留核心事实和行动项。使用纯文本 不使用md格式 \n\n邮件正文如下：\n\n%s",
		text,
	)

	// 5. 调用 API 生成内容
	// SDK 会自动将字符串包装成 `genai.Text` 类型
	resp, err := model.GenerateContent(ctx, genai.Text(prompt))
	if err != nil {
		log.Printf("错误: 内容生成失败: %v", err)
		return text
	}

	// 6. 从响应中提取文本内容
	// SDK 提供了更简单、更安全的方式来访问结果
	var summary strings.Builder
	if len(resp.Candidates) > 0 {
		for _, part := range resp.Candidates[0].Content.Parts {
			if txt, ok := part.(genai.Text); ok {
				summary.WriteString(string(txt))
			}
		}
	}

	if summary.Len() > 0 {
		return strings.TrimSpace(summary.String())
	}

	fmt.Println("警告: AI 未能返回有效内容。")
	return text
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

type searchParams struct {
	windowHours int
	unreadOnly  bool
}

func fetchLatest(ctx context.Context, mb Mailbox, sinceUID uint32, limit int, allowDomains []string, params searchParams, apikey string) ([]MailSummary, uint32, error) {
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

	crit := imap.NewSearchCriteria()
	if params.windowHours <= 0 {
		params.windowHours = 2
	}
	crit.Since = time.Now().Add(-time.Duration(params.windowHours) * time.Hour)
	if params.unreadOnly {
		// 仅未读（UNSEEN）
		crit.WithoutFlags = []string{imap.SeenFlag}
	}
	uids, err := c.UidSearch(crit)
	if err != nil {
		return nil, sinceUID, fmt.Errorf("UidSearch: %w", err)
	}
	if len(uids) == 0 {
		return nil, sinceUID, nil
	}

	// > sinceUID（若使用 state）
	filtered := make([]uint32, 0, len(uids))
	for _, id := range uids {
		if sinceUID == 0 || id > sinceUID {
			filtered = append(filtered, id)
		}
	}
	if len(filtered) == 0 {
		return nil, sinceUID, nil
	}
	sort.Slice(filtered, func(i, j int) bool { return filtered[i] < filtered[j] })
	if len(filtered) > limit {
		filtered = filtered[len(filtered)-limit:]
	}

	seq := new(imap.SeqSet)
	seq.AddNum(filtered...)

	section := &imap.BodySectionName{} // RFC822
	items := []imap.FetchItem{imap.FetchEnvelope, imap.FetchUid, section.FetchItem()}
	ch := make(chan *imap.Message, len(filtered))
	go func() { _ = c.UidFetch(seq, items, ch) }()

	var res []MailSummary
	maxUID := sinceUID

	for msg := range ch {
		if msg == nil || msg.Envelope == nil || msg.Uid == 0 {
			continue
		}
		// 域名白名单
		ok := false
		for _, a := range msg.Envelope.From {
			if domainAllowed(a.HostName, allowDomains) {
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

		from := "(unknown)"
		if len(msg.Envelope.From) > 0 {
			a := msg.Envelope.From[0]
			if a.PersonalName != "" {
				from = fmt.Sprintf("%s <%s@%s>", a.PersonalName, a.MailboxName, a.HostName)
			} else {
				from = fmt.Sprintf("%s@%s", a.MailboxName, a.HostName)
			}
		}

		preview := ""
		if body := msg.GetBody(section); body != nil {
			if text, err := extractTextPlain(body); err == nil {
				preview = normalizeSpaces(text)
			}
		}
		// 超长先AI摘要（可选）
		preview = summarizeWithAI(ctx, preview, apikey)

		// 进一步兜底截断，避免进入 TG 才超限
		if len([]rune(preview)) > 3800 {
			rs := []rune(preview)
			preview = string(rs[:3800]) + "…"
		}

		res = append(res, MailSummary{
			UID:     msg.Uid,
			From:    from,
			Subject: msg.Envelope.Subject,
			Date:    msg.Envelope.Date,
			Preview: preview,
		})
	}

	sort.Slice(res, func(i, j int) bool { return res[i].Date.Before(res[j].Date) })
	return res, maxUID, nil
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
		PollSeconds: 0,
		// Actions 环境：一次性 + 禁用 state
		DisableState: os.Getenv("GITHUB_ACTIONS") == "true" || os.Getenv("DISABLE_STATE") == "1",
		WindowHours:  parseIntEnv("WINDOW_HOURS", 2),
		UnreadOnly:   parseBoolEnv("UNREAD_ONLY", true),
		ApiKey:       os.Getenv("OPENAI_API_KEY"),
	}
	if v := strings.TrimSpace(os.Getenv("POLL_SECONDS")); v != "" {
		if d, err := time.ParseDuration(v + "s"); err == nil {
			env.PollSeconds = int(d.Seconds())
		}
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

	statePath := "state/state.json"
	var st *State
	var err error
	if env.DisableState {
		st = &State{LastUID: map[string]uint32{}}
	} else {
		st, err = loadState(statePath)
		if err != nil {
			log.Fatalf("加载状态失败: %v", err)
		}
	}

	runOnce := func() {
		ctx := context.Background()
		for _, mb := range mbs {
			func() {
				var since uint32
				if !env.DisableState {
					since = st.LastUID[mb.Email]
				}
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
				if !env.DisableState && maxUID > since {
					st.LastUID[mb.Email] = maxUID
					if err := saveState(statePath, st); err != nil {
						log.Printf("保存状态失败: %v", err)
					}
				}
			}()
		}
	}

	if env.PollSeconds <= 0 {
		runOnce()
		return
	}
	ticker := time.NewTicker(time.Duration(env.PollSeconds) * time.Second)
	defer ticker.Stop()
	log.Printf("mail2tg 轮询中，间隔 %ds", env.PollSeconds)
	for {
		runOnce()
		<-ticker.C
	}
}
