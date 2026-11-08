package protocol

import (
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type SessionFailure struct {
	RequiresLogin bool
	RateLimited   bool
	Temporary     bool
	RetryAfter    time.Duration
}

func ClassifySessionError(err error) SessionFailure {
	var coded codedError
	if !errors.As(err, &coded) {
		var networkError net.Error
		return SessionFailure{Temporary: errors.As(err, &networkError) || isAppleTransientNetworkError(err)}
	}
	return SessionFailure{
		RequiresLogin: coded.status == 401 || coded.status == 421 || coded.status == 450 || coded.code == "apple_account_auth_failed" || coded.code == "apple_account_session_missing" || coded.code == "icloud_session_missing" || coded.code == "session_login_required",
		RateLimited:   coded.status == http.StatusTooManyRequests,
		Temporary:     coded.status >= 500,
		RetryAfter:    coded.retryAfter,
	}
}

func stopSessionRecovery(err error) bool {
	failure := ClassifySessionError(err)
	return failure.RequiresLogin || failure.RateLimited || failure.Temporary || failure.RetryAfter > 0
}

func withHTTPDetails(err error, response *http.Response) error {
	var coded codedError
	if !errors.As(err, &coded) {
		coded = codedError{code: "remote_http_error", message: err.Error(), retryable: true}
	}
	coded.status = response.StatusCode
	value := strings.TrimSpace(response.Header.Get("Retry-After"))
	if seconds, e := strconv.ParseInt(value, 10, 64); e == nil && seconds > 0 {
		// 限制乘法溢出；有效的服务端等待时间不会被本地退避上限截短。
		if seconds > int64((1<<63-1)/time.Second) {
			seconds = int64((1<<63 - 1) / time.Second)
		}
		coded.retryAfter = time.Duration(seconds) * time.Second
	} else if at, e := http.ParseTime(value); e == nil && at.After(time.Now()) {
		coded.retryAfter = time.Until(at)
	}
	return coded
}
