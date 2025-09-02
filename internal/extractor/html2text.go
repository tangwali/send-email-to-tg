package extractor

import (
	"strings"

	"golang.org/x/net/html"
)

// HTML -> 纯文本：忽略 script/style，压缩空白
func HTMLToText(doc *html.Node) string {
	var sb strings.Builder
	var walk func(*html.Node, bool)
	walk = func(n *html.Node, inPre bool) {
		switch n.Type {
		case html.ElementNode:
			switch n.Data {
			case "script", "style":
				return
			case "p", "div", "br", "li", "tr", "h1", "h2", "h3":
				sb.WriteString("\n")
			}
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				walk(c, inPre || n.Data == "pre")
			}
			switch n.Data {
			case "p", "div", "li", "tr":
				sb.WriteString("\n")
			}
		case html.TextNode:
			t := n.Data
			if !inPre {
				t = NormalizeSpaces(t)
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
	return strings.TrimSpace(NormalizeSpaces(sb.String()))
}
