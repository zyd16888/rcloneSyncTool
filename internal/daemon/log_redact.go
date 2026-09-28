package daemon

import (
	"regexp"
	"strings"
)

var secretArgument = regexp.MustCompile(`(?i)((?:authorization|password|passwd|api[_-]?key|secret|token|cookie)[^\s=:]*\s*[=:]\s*)("[^"]*"|'[^']*'|[^\s]+)`)
var credentialURL = regexp.MustCompile(`(?i)(https?://)[^/@\s]+:[^/@\s]+@`)

func redactMessage(message string) string {
	message = secretArgument.ReplaceAllString(message, "${1}***")
	message = credentialURL.ReplaceAllString(message, "${1}***@")
	return strings.ReplaceAll(message, "\x00", "")
}
func RedactLog(message string) string { return redactMessage(message) }
