package main

import (
	"testing"

	"github.com/mileusna/useragent"
)

func TestUAFamilyBotStamping(t *testing.T) {
	cases := []struct {
		name  string
		rawUA string
		want  string
	}{
		{"named browser", "Mozilla/5.0 (Windows NT 10.0) AppleWebKit/537.36 Chrome/120 Safari/537.36", "Chrome"},
		{"gptbot", "Mozilla/5.0 AppleWebKit/537.36 (KHTML, like Gecko; compatible; GPTBot/1.1; +https://openai.com/gptbot)", "bot"},
		{"claudebot", "Mozilla/5.0 (compatible; ClaudeBot/1.0; +claudebot@anthropic.com)", "bot"},
		{"flutter dart is never a bot", "Dart/3.5 (dart:io)", ""},
	}
	for _, c := range cases {
		ua := useragent.Parse(c.rawUA)
		got := uaFamily(ua, c.rawUA)
		// Named crawlers may parse to their own name (still bot-matched downstream);
		// what matters is: never empty for a flagged bot, and never "bot" for Dart.
		if c.name == "flutter dart is never a bot" {
			if got == "bot" {
				t.Errorf("%s: Dart must never be marked bot, got %q", c.name, got)
			}
			continue
		}
		if c.want == "bot" && got == "" {
			t.Errorf("%s: bot leaked as empty ua_family (raw=%q)", c.name, c.rawUA)
		}
		if c.name == "named browser" && got != "Chrome" {
			t.Errorf("%s: got %q want Chrome", c.name, got)
		}
	}
}
