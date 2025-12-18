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
		"You are an experienced operations assistant with a cute and friendly writing style. "+
			"Please summarize the full email content below into a Chinese正文摘要. Requirements:\n"+
			"1) Use a cute, light, and friendly tone, with reasonable use of emojis;\n"+
			"2) List key information line by line using clear labels; keep only essential information; "+
			"remove any noise such as help instructions, setup guides, tips, disclaimers, or promotional text; "+
			"remove ALL URLs, including https links and any other links;\n"+
			"3) Separate each item with a single line break; do NOT include empty lines;\n"+
			"4) Output plain text ONLY — do NOT use Markdown or HTML; do NOT include tracking links;\n"+
			"5) Total length must not exceed 800 Chinese characters;\n"+
			"6) Output ONLY the summarized body content; do NOT repeat sender, subject, or date headers;\n"+
			"7) The FINAL OUTPUT MUST BE IN CHINESE.\n\n"+
			"Email content:\n%s",
		text,
	)

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
