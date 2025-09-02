package extractor

import (
	"bytes"
	"io"
	"strings"

	"github.com/emersion/go-message/mail"
	"golang.org/x/net/html"
)

func NormalizeSpaces(s string) string {
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

func RemoveEmptyLines(s string) string {
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

func ExtractTextPreferPlain(r io.Reader) (string, error) {
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
							return NormalizeSpaces(string(b)), nil
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
							doc, perr := html.Parse(bytes.NewReader(b))
							if perr == nil {
								return HTMLToText(doc), nil
							}
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
				return NormalizeSpaces(string(b)), nil
			}
		}
	}
	return "", nil
}
