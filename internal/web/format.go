package web

import (
	"fmt"
	"html/template"
	"strings"
	"time"
	"unicode"
)

// funcs are the helpers templates can call.
func (s *Server) funcs() template.FuncMap {
	return template.FuncMap{
		"reltime":  func(t time.Time) string { return relTime(s.store.Now(), t) },
		"isotime":  func(t time.Time) string { return t.UTC().Format(time.RFC3339) },
		"initials": initials,
		"plural":   plural,
	}
}

// relTime words a past time relative to now: "2h ago", "yesterday",
// "3 days ago", then a date once it is a month old.
func relTime(now, t time.Time) string {
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d/time.Minute))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d/time.Hour))
	case d < 48*time.Hour:
		return "yesterday"
	case d < 30*24*time.Hour:
		return fmt.Sprintf("%d days ago", int(d/(24*time.Hour)))
	default:
		return t.UTC().Format("Jan 2, 2006")
	}
}

// initials returns up to two capitals for the avatar fallback.
func initials(name string) string {
	var out []rune
	for _, word := range strings.Fields(name) {
		for _, r := range word {
			out = append(out, unicode.ToUpper(r))
			break
		}
		if len(out) == 2 {
			break
		}
	}
	if len(out) == 0 {
		return "?"
	}
	return string(out)
}

// plural formats a count with the right noun: "1 reply", "3 replies".
func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}
