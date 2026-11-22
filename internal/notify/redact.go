// SPDX-License-Identifier: Apache-2.0

package notify

import (
	"errors"
	"net/url"
	"regexp"
	"strings"
)

// A notification endpoint URL is often the credential itself: a Slack
// incoming-webhook URL carries its secret in the path, generic webhooks put
// tokens in the query, and any of them may carry userinfo. net/http wraps
// every transport failure in a *url.Error whose message repeats the full
// URL, so an unredacted error lands the secret in every retry log line and
// in the dead-letter record. Errors from notifiers therefore pass through
// RedactError, which keeps only the scheme and host.

const redactedPath = "/REDACTED"

// RedactURL returns raw reduced to scheme://host: path, query, fragment
// and userinfo are dropped. A value that does not parse as an absolute URL
// is replaced entirely.
func RedactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "REDACTED"
	}
	return u.Scheme + "://" + u.Host + redactedPath
}

// urlInText matches absolute http(s) URLs in free text: scheme, optional
// userinfo, host, and everything up to the next delimiter.
var urlInText = regexp.MustCompile(`(?i)\b(https?://)(?:[^/\s"'@]*@)?([^/\s"'?#]+)[^\s"']*`)

// RedactText replaces every http(s) URL in s by its scheme and host.
func RedactText(s string) string {
	return urlInText.ReplaceAllString(s, "${1}${2}"+redactedPath)
}

// RedactError returns err with every URL in its message reduced to scheme
// and host, and every occurrence of the given secrets removed. errors.Is
// still sees through it (context deadline, cancellation); errors.As and
// Unwrap do not, so the original message cannot be recovered by unwrapping.
func RedactError(err error, secrets ...string) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	var ue *url.Error
	if errors.As(err, &ue) && ue.URL != "" {
		msg = strings.ReplaceAll(msg, ue.URL, RedactURL(ue.URL))
	}
	for _, secret := range secrets {
		if secret == "" {
			continue
		}
		msg = strings.ReplaceAll(msg, secret, RedactURL(secret))
	}
	return &redactedError{msg: RedactText(msg), inner: err}
}

type redactedError struct {
	msg   string
	inner error
}

func (e *redactedError) Error() string { return e.msg }

func (e *redactedError) Is(target error) bool { return errors.Is(e.inner, target) }
