package util

import "strings"

func EscHTML(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	return r.Replace(s)
}

func DomainAllowed(host string, allow []string) bool {
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

func GuessIMAPHost(email string) string {
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
		// 通用猜测
		return "imap." + domain + ":993"
	}
}
