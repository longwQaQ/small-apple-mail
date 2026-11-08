package services

import (
	"regexp"
	"strings"
)

type CodeCandidate struct {
	Code       string
	Rule       string
	Confidence float64
}

var contextualCode = regexp.MustCompile(`(?i)(?:verification|security|temporary|one[- ]time|验证码|校验码|动态码|code)[^0-9A-Z]{0,28}([0-9]{4,8})`)

// 独立数字不能是颜色值、CSS 类名或字母数字标识符的一部分。
var isolatedCode = regexp.MustCompile(`(?:^|[^A-Za-z0-9#_-])([0-9]{6})(?:[^A-Za-z0-9_-]|$)`)

func ExtractCode(subject, body string) CodeCandidate {
	text := strings.TrimSpace(subject + "\n" + body)
	text = messageURL.ReplaceAllString(text, "")
	if match := contextualCode.FindStringSubmatch(text); len(match) > 1 {
		return CodeCandidate{Code: match[1], Rule: "contextual_numeric", Confidence: .95}
	}
	if match := isolatedCode.FindStringSubmatch(text); len(match) > 1 {
		return CodeCandidate{Code: match[1], Rule: "isolated_six_digit", Confidence: .72}
	}
	return CodeCandidate{}
}
