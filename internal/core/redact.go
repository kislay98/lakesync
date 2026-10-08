package core

import (
	"net/url"
	"regexp"
	"strings"
)

var (
	uriCreds = regexp.MustCompile(`(?i)^([a-z][a-z0-9+.-]*://)([^/@\s]+)@`)
	kvSecret = regexp.MustCompile(`(?i)(password|passwd|pwd|secret|token|authSource|accessKey|secretKey|api[_-]?key)=([^&\s;,]+)`)
)

// RedactURI hides credentials in a connection string or URL:
// scheme://user:pass@host -> scheme://user:***@host, and password-like query values.
func RedactURI(s string) string {
	s = uriCreds.ReplaceAllStringFunc(s, func(m string) string {
		sub := uriCreds.FindStringSubmatch(m)
		userinfo := sub[2]
		if i := strings.IndexByte(userinfo, ':'); i >= 0 {
			userinfo = userinfo[:i] + ":***"
		} else {
			userinfo = "***"
		}
		return sub[1] + userinfo + "@"
	})
	return kvSecret.ReplaceAllString(s, "$1=***")
}

// RedactText replaces every occurrence of the given secrets in text with ***.
func RedactText(text string, secrets ...string) string {
	for _, sec := range secrets {
		if len(sec) >= 4 {
			text = strings.ReplaceAll(text, sec, "***")
			if esc := url.QueryEscape(sec); esc != sec {
				text = strings.ReplaceAll(text, esc, "***")
			}
		}
	}
	return text
}
