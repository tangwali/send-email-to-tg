package summarizer

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/google/generative-ai-go/genai"
	"google.golang.org/api/option"
)

type Config struct {
	APIKey string
	Model  string
	Enable bool
}

func Summarize(ctx context.Context, cfg Config, text string) string {
	if !cfg.Enable || strings.TrimSpace(cfg.APIKey) == "" {
		return text
	}

	client, err := genai.NewClient(ctx, option.WithAPIKey(cfg.APIKey))
	if err != nil {
		log.Printf("gemini client error: %v", err)
		return text
	}
	defer client.Close()

	model := client.GenerativeModel(cfg.Model)
	model.GenerationConfig = genai.GenerationConfig{
		Temperature: genai.Ptr(float32(0.2)),
	}

	prompt := fmt.Sprintf(
		"你是一位可爱风格的资深运营助理，请把下面的完整邮件信息整理成中文正文，要求："+
			"1) 采用可爱轻松的语气和合理使用emoji；"+
			"2) 使用清晰标签逐行列出关键信息，只保留主要信息，不要“帮助/设置/温馨提示”等杂讯；"+
			"3) 用换行分隔条目，不要空白段落；"+
			"4) 不要使用 Markdown/HTML，不要粘贴跟踪链接；"+
			"5) 总长度不超过 800 字；"+
			"6) 输出只包含摘要正文，不要重复发件人或日期抬头。\n\n邮件内容：\n%s", text)

	const maxRetries = 5
	backoff := 5 * time.Second
	var last string

	for i := 0; i < maxRetries; i++ {
		resp, err := model.GenerateContent(ctx, genai.Text(prompt))
		if err == nil && len(resp.Candidates) > 0 {
			var sb strings.Builder
			for _, part := range resp.Candidates[0].Content.Parts {
				if txt, ok := part.(genai.Text); ok {
					sb.WriteString(string(txt))
				}
			}
			out := strings.TrimSpace(sb.String())
			if out != "" {
				return out
			}
		} else if err != nil {
			last = err.Error()
			log.Printf("gemini generate error (try %d/%d): %v", i+1, maxRetries, err)
		}
		select {
		case <-ctx.Done():
			return text
		case <-time.After(backoff):
			backoff *= 2
		}
	}
	log.Printf("gemini final error: %s", last)
	return text
}
