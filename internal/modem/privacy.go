package modem

import "strings"

func redactIdentifier(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	runes := []rune(value)
	visible := 4
	if len(runes) <= visible {
		return "****"
	}
	return "****" + string(runes[len(runes)-visible:])
}
