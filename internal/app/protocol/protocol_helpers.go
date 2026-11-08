package protocol

import (
	"regexp"
	"strings"
	"time"
)

// codedError 为 Apple/iCloud/IMAP 协议层保留稳定的错误分类。
type codedError struct {
	code       string
	message    string
	retryable  bool
	status     int
	retryAfter time.Duration
}

func (e codedError) Error() string { return e.message }

func errCode(code, message string, retryable bool) error {
	return codedError{code: code, message: message, retryable: retryable}
}

func firstNonZeroTime(values ...time.Time) time.Time {
	for _, value := range values {
		if !value.IsZero() {
			return value
		}
	}
	return time.Time{}
}

func normalizeICloudIMAPEmail(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

var (
	contextOTPRegex = regexp.MustCompile(`(?i)(?:openai|chatgpt|otp|code|verification|验证码|验证|代码)[^\d]{0,80}(\d{6})`)
	plainOTPRegex   = regexp.MustCompile(`\b(\d{6})\b`)
)

func extractOTP(text string) string {
	if matches := contextOTPRegex.FindStringSubmatch(text); len(matches) == 2 && validOTP(matches[1]) {
		return matches[1]
	}
	for _, matches := range plainOTPRegex.FindAllStringSubmatch(text, -1) {
		if len(matches) == 2 && validOTP(matches[1]) {
			return matches[1]
		}
	}
	return ""
}

func validOTP(code string) bool {
	return len(code) == 6 && code != "000000"
}
