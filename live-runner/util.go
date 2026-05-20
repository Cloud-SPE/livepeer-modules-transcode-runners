package main

import (
	"strconv"
	"strings"
)

func itoa(v int) string { return strconv.Itoa(v) }

func itoa64(v uint64) string { return strconv.FormatUint(v, 10) }

func redactSecrets(text string, secrets ...string) string {
	out := text
	for _, secret := range secrets {
		if secret == "" {
			continue
		}
		out = strings.ReplaceAll(out, secret, redactToken(secret))
	}
	return out
}

func redactToken(s string) string {
	if len(s) <= 4 {
		return "[redacted]"
	}
	return "[redacted:" + s[len(s)-4:] + "]"
}
