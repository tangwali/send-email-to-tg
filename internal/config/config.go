package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Mailbox struct {
	Name        string // 你的“品牌名”，如 TwaliRay / Qiaxiluxe
	Email       string
	Password    string // QQ: 授权码；Gmail: 应用专用密码
	IMAP        string // 可留空，自动由 Email 推导。非标准时可显式填写 host:port
	UnreadOnly  bool   // 每个邮箱可单独控制是否仅未读
	WindowHours int    // 初始启动时回溯窗口（小时）
}

type Config struct {
	Mailboxes []Mailbox

	TGBotToken string
	TGChatID   string

	SenderDomains []string // 域名白名单（含子域）
	SummaryAPIKey string   // Google AI Studio Key
	SummaryModel  string   // e.g. gemini-2.0-flash
	EnableSummary bool

	// 实时控制
	IdleFallbackPollSec int           // IDLE 不可用时轮询间隔秒（默认 45~90 之间随机抖动）
	IdleKeepaliveSec    int           // 发送 NOOP 维持 IDLE 的间隔（默认 14 分钟内）
	ReconnectBackoff    time.Duration // 断线指数退避基准
	MaxFetchPerBatch    int           // 每次批量抓取上限
}

func (c *Config) Validate() error {
	if len(c.Mailboxes) == 0 {
		return fmt.Errorf("no mailboxes configured")
	}
	if c.TGBotToken == "" || c.TGChatID == "" {
		return fmt.Errorf("TELEGRAM_BOT_TOKEN / TELEGRAM_CHAT_ID required")
	}
	return nil
}

// Load 从环境变量加载（兼容你原逻辑）。
// 也可扩展为 YAML/JSON 配置，这里先保持环境变量方案简单可控。
func Load() (*Config, error) {
	// 支持两个邮箱（可自行扩到多邮箱，通过前缀数组化，这里按你的原始需求保留两个）
	var mbs []Mailbox

	// QQ
	if qqEmail := os.Getenv("QQ_EMAIL"); qqEmail != "" && os.Getenv("QQ_EMAIL_AUTH_CODE") != "" {
		mbs = append(mbs, Mailbox{
			Name:        envOr("QQ_BOX_NAME", "TwaliRay"),
			Email:       qqEmail,
			Password:    os.Getenv("QQ_EMAIL_AUTH_CODE"),
			IMAP:        os.Getenv("QQ_IMAP_HOST"), // 可为空 -> 推导
			UnreadOnly:  parseBoolEnv("UNREAD_ONLY_QQ", parseBoolEnv("UNREAD_ONLY", true)),
			WindowHours: parseIntEnv("WINDOW_HOURS_QQ", parseIntEnv("WINDOW_HOURS", 1)),
		})
	}
	// Gmail
	if gmEmail := os.Getenv("GMAIL_EMAIL"); gmEmail != "" && os.Getenv("GMAIL_APP_PASSWORD") != "" {
		mbs = append(mbs, Mailbox{
			Name:        envOr("GMAIL_BOX_NAME", "Qiaxiluxe"),
			Email:       gmEmail,
			Password:    os.Getenv("GMAIL_APP_PASSWORD"),
			IMAP:        os.Getenv("GMAIL_IMAP_HOST"), // 可为空 -> 推导
			UnreadOnly:  parseBoolEnv("UNREAD_ONLY_GMAIL", parseBoolEnv("UNREAD_ONLY", true)),
			WindowHours: parseIntEnv("WINDOW_HOURS_GMAIL", parseIntEnv("WINDOW_HOURS", 1)),
		})
	}

	// 发件域白名单
	domains := splitCSVDefault(os.Getenv("SENDER_DOMAINS"), []string{"amazon.com"})

	cfg := &Config{
		Mailboxes:           mbs,
		TGBotToken:          os.Getenv("TELEGRAM_BOT_TOKEN"),
		TGChatID:            os.Getenv("TELEGRAM_CHAT_ID"),
		SenderDomains:       domains,
		SummaryAPIKey:       os.Getenv("OPENAI_API_KEY"), // 兼容你现有命名；建议改为 GOOGLE_API_KEY
		SummaryModel:        envOr("SUMMARY_MODEL", "gemini-2.0-flash"),
		EnableSummary:       parseBoolEnv("ENABLE_SUMMARY", true),
		IdleFallbackPollSec: parseIntEnv("IDLE_FALLBACK_POLL_SEC", 60),
		IdleKeepaliveSec:    parseIntEnv("IDLE_KEEPALIVE_SEC", 13*60),
		ReconnectBackoff:    time.Duration(parseIntEnv("RECONNECT_BACKOFF_SEC", 10)) * time.Second,
		MaxFetchPerBatch:    parseIntEnv("MAX_FETCH_PER_BATCH", 80),
	}
	return cfg, nil
}

func envOr(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

func splitCSVDefault(s string, def []string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return def
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.ToLower(strings.TrimSpace(p))
		if p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return def
	}
	return out
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
