package ai

import "regexp"

// Redaction happens before ticket text leaves the process. Credentials and
// secrets are always removed; requester-facing preflight additionally removes
// email addresses and IP addresses, because those are the fields most likely to
// identify a person and the model does not need them to pick a category.
var (
	secretPattern = regexp.MustCompile(`(?i)\b(password|passwd|pwd|secret|token|api[_-]?key|access[_-]?key)\b\s*[:=]\s*\S+`)
	bearerPattern = regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._\-]{8,}`)
	emailPattern  = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)
	ipv4Pattern   = regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}\b`)
)

// Redact removes secrets from text. When identity is true it also removes
// email addresses and IPv4 addresses.
func Redact(text string, identity bool) string {
	text = secretPattern.ReplaceAllString(text, "${1}: [redacted]")
	text = bearerPattern.ReplaceAllString(text, "Bearer [redacted]")
	if identity {
		text = emailPattern.ReplaceAllString(text, "[email]")
		text = ipv4Pattern.ReplaceAllString(text, "[ip]")
	}
	return text
}
