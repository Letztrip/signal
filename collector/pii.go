package main

import (
	"encoding/json"
	"fmt"
	"regexp"
)

var piiPatterns = []struct {
	name string
	re   *regexp.Regexp
}{
	{"email", regexp.MustCompile(`^[\w.\-]+@[\w.\-]+\.\w+$`)},
	{"phone", regexp.MustCompile(`^\+?\d{10,15}$`)},
	{"card", regexp.MustCompile(`\b(?:\d[ -]*?){13,16}\b`)},
	{"ssn", regexp.MustCompile(`\b\d{3}-\d{2}-\d{4}\b`)},
}

// scanPII walks `properties` recursively and returns the JSON path of the first
// match (e.g. "properties.user.email"). Empty string means clean.
func scanPII(propertiesRaw json.RawMessage) string {
	if len(propertiesRaw) == 0 {
		return ""
	}
	var v any
	if err := json.Unmarshal(propertiesRaw, &v); err != nil {
		return ""
	}
	return walkPII("properties", v)
}

func walkPII(path string, v any) string {
	switch val := v.(type) {
	case string:
		for _, p := range piiPatterns {
			if p.re.MatchString(val) {
				return path + " (" + p.name + ")"
			}
		}
	case map[string]any:
		for k, child := range val {
			if hit := walkPII(path+"."+k, child); hit != "" {
				return hit
			}
		}
	case []any:
		for i, child := range val {
			if hit := walkPII(fmt.Sprintf("%s[%d]", path, i), child); hit != "" {
				return hit
			}
		}
	}
	return ""
}
