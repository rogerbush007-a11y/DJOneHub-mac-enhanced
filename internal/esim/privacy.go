package esim

import "strings"

func redactIdentifier(value string) string {
	value = strings.TrimSpace(value)
	runes := []rune(value)
	if len(runes) <= 4 {
		if value == "" {
			return ""
		}
		return "****"
	}
	return "****" + string(runes[len(runes)-4:])
}
