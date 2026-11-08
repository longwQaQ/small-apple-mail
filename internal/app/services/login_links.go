package services

import (
	"html"
	"net/url"
	"regexp"
	"strings"
)

// URL 使用 ASCII 字符，避免吞掉紧邻链接的中文说明或标点。
var messageURL = regexp.MustCompile(`(?i)https?://[a-z0-9._~:/?#\[\]@!$&()*+,;=%-]+`)
var messageHTMLEntity = regexp.MustCompile(`&(?:#[0-9]+|#[xX][0-9a-fA-F]+|[a-zA-Z][a-zA-Z0-9]+);`)

// ExtractClaudeLoginURL 只解析邮件文本，不访问链接或解析远端跳转。
func ExtractClaudeLoginURL(body string) string {
	// 必须有分号，防止把 URL 参数中的 &notifier= 等内容误解为 HTML 实体。
	body = messageHTMLEntity.ReplaceAllStringFunc(body, html.UnescapeString)
	var result string
	for _, loc := range messageURL.FindAllStringIndex(body, -1) {
		candidate := body[loc[0]:loc[1]]
		quoted := loc[0] > 0 && loc[1] < len(body) && (body[loc[0]-1] == '"' || body[loc[0]-1] == '\'') && body[loc[0]-1] == body[loc[1]]
		if !quoted {
			candidate = strings.TrimRight(candidate, ".,);]")
		}
		u, err := url.Parse(candidate)
		if err != nil || u.Scheme != "https" || !strings.EqualFold(u.Host, "claude.ai") || u.User != nil || u.Path != "/magic-link" || (u.RawQuery == "" && u.Fragment == "") {
			continue
		}
		if result != "" && result != candidate {
			return ""
		}
		result = candidate
	}
	return result
}
