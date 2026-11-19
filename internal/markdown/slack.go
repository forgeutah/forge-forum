package markdown

import (
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// SlackResolver supplies display names for Slack IDs found in message text.
type SlackResolver interface {
	// UserName returns the display name for a Slack user ID, or "" when
	// the user is unknown.
	UserName(userID string) string
}

// UnescapeSlack turns the three HTML entities Slack escapes in message
// text (&amp;, &lt;, &gt;) back into their characters.
func UnescapeSlack(s string) string { return slackUnescape.Replace(s) }

// Converted pieces (code, mentions, links) are swapped for placeholders
// while emphasis and entities are rewritten, so that neither pass can
// touch them. A placeholder is holdOpen, the piece's index, holdClose.
// Both runes are private-use and are stripped from the input first.
const (
	holdOpen  = ""
	holdClose = ""
)

var (
	holdPattern   = regexp.MustCompile(holdOpen + `(\d+)` + holdClose)
	stripHolds    = strings.NewReplacer(holdOpen, "", holdClose, "")
	slackUnescape = strings.NewReplacer("&amp;", "&", "&lt;", "<", "&gt;", ">")
	// inlineEscape keeps a display name or link label from being read as
	// Markdown.
	inlineEscape = strings.NewReplacer(
		`\`, `\\`, `*`, `\*`, `_`, `\_`, `[`, `\[`, `]`, `\]`, "`", "\\`", `~`, `\~`, `<`, `\<`)
	// linkDestEscape keeps a URL inside the parentheses of a Markdown link.
	linkDestEscape = strings.NewReplacer(" ", "%20", "(", "%28", ")", "%29", "<", "%3C", ">", "%3E")
	emphasisMarks  = map[rune]string{'*': "**", '_': "*", '~': "~~"}
)

// FromSlack converts Slack mrkdwn message text to Markdown suitable for
// Render. It only converts: the text is untrusted and Render sanitizes it.
//
// Slack escapes &, < and > everywhere, including inside code, so the three
// entities are unescaped exactly once in code as well as in prose.
func FromSlack(text string, r SlackResolver) string {
	c := slackConverter{resolver: r}
	s := c.tokenize(stripHolds.Replace(text))
	s = separateQuotes(s)
	s = convertEmphasis(s)
	s = slackUnescape.Replace(s)
	return holdPattern.ReplaceAllStringFunc(s, func(m string) string {
		i, err := strconv.Atoi(m[len(holdOpen) : len(m)-len(holdClose)])
		if err != nil || i >= len(c.held) {
			return ""
		}
		return c.held[i]
	})
}

type slackConverter struct {
	resolver SlackResolver
	held     []string
	out      []byte
}

// hold stores finished Markdown and writes its placeholder.
func (c *slackConverter) hold(md string) {
	c.out = append(c.out, holdOpen+strconv.Itoa(len(c.held))+holdClose...)
	c.held = append(c.held, md)
}

// tokenize replaces code spans, fenced blocks, and <...> references with
// placeholders, code first, and returns the remaining prose.
func (c *slackConverter) tokenize(text string) string {
	for i := 0; i < len(text); {
		rest := text[i:]
		switch {
		case strings.HasPrefix(rest, "```"):
			if end := strings.Index(rest[3:], "```"); end >= 0 {
				i += c.fence(rest[3:3+end], text[i+end+6:]) + end + 6
				continue
			}
		case rest[0] == '`':
			// Inline code stays on one line and is not empty.
			if end := strings.IndexAny(rest[1:], "`\n"); end > 0 && rest[1+end] == '`' {
				c.hold("`" + slackUnescape.Replace(rest[1:1+end]) + "`")
				i += end + 2
				continue
			}
		case rest[0] == '<':
			if end := strings.IndexAny(rest[1:], "<>\n"); end > 0 && rest[1+end] == '>' {
				c.hold(c.reference(rest[1 : 1+end]))
				i += end + 2
				continue
			}
		}
		c.out = append(c.out, text[i])
		i++
	}
	return string(c.out)
}

// fence holds a fenced block on lines of its own. Slack allows a fence in
// the middle of a line; Markdown does not. It returns how many bytes of
// after (the text following the closing fence) it consumed.
func (c *slackConverter) fence(code, after string) int {
	c.out = []byte(strings.TrimRight(string(c.out), " \t"))
	if len(c.out) > 0 && c.out[len(c.out)-1] != '\n' {
		c.out = append(c.out, '\n')
	}
	code = strings.TrimSuffix(strings.TrimPrefix(slackUnescape.Replace(code), "\n"), "\n")
	c.hold("```\n" + code + "\n```")
	skipped := len(after) - len(strings.TrimLeft(after, " \t"))
	if skipped < len(after) && after[skipped] != '\n' {
		c.out = append(c.out, '\n')
	}
	return skipped
}

// reference converts the inside of a Slack <...> token to Markdown.
func (c *slackConverter) reference(content string) string {
	target, label, _ := strings.Cut(content, "|")
	label = slackUnescape.Replace(label)
	switch {
	case strings.HasPrefix(target, "@"):
		name := ""
		if c.resolver != nil {
			name = c.resolver.UserName(target[1:])
		}
		if name == "" {
			name = strings.TrimPrefix(label, "@")
		}
		if name == "" {
			name = "unknown"
		}
		return "@" + inlineEscape.Replace(name)
	case strings.HasPrefix(target, "#"):
		name := strings.TrimPrefix(label, "#")
		if name == "" {
			name = "unknown"
		}
		return "#" + inlineEscape.Replace(name)
	case strings.HasPrefix(target, "!"):
		command, _, _ := strings.Cut(target[1:], "^")
		switch {
		case command == "here" || command == "channel" || command == "everyone":
			return "@" + command
		case label != "":
			return inlineEscape.Replace(label)
		case command == "subteam":
			return "@group"
		}
		return ""
	case isSlackLink(target):
		url := slackUnescape.Replace(target)
		if label == "" && strings.HasPrefix(url, "mailto:") {
			label = strings.TrimPrefix(url, "mailto:")
		}
		if label == "" {
			return url
		}
		return "[" + inlineEscape.Replace(label) + "](" + linkDestEscape.Replace(url) + ")"
	case label != "":
		return inlineEscape.Replace(label)
	}
	return inlineEscape.Replace("<" + slackUnescape.Replace(content) + ">")
}

// isSlackLink reports whether target is a web or mail link. Any other
// scheme is shown as text rather than linked.
func isSlackLink(target string) bool {
	t := strings.ToLower(target)
	return strings.HasPrefix(t, "https://") || strings.HasPrefix(t, "http://") || strings.HasPrefix(t, "mailto:")
}

// separateQuotes puts a blank line after a run of quoted lines, so the
// line that follows is not folded into the blockquote. Quote markers are
// still escaped as "&gt;" here and become ">" when entities are unescaped.
func separateQuotes(s string) string {
	if !strings.Contains(s, "&gt;") {
		return s
	}
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines)+1)
	for i, line := range lines {
		out = append(out, line)
		if strings.HasPrefix(line, "&gt;") && i+1 < len(lines) &&
			lines[i+1] != "" && !strings.HasPrefix(lines[i+1], "&gt;") {
			out = append(out, "")
		}
	}
	return strings.Join(out, "\n")
}

// convertEmphasis rewrites Slack's *bold*, _italic_ and ~strike~ as
// Markdown. Emphasis never spans lines.
func convertEmphasis(s string) string {
	if !strings.ContainsAny(s, "*_~") {
		return s
	}
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		lines[i] = emphasizeLine([]rune(line))
	}
	return strings.Join(lines, "\n")
}

func emphasizeLine(rs []rune) string {
	var b strings.Builder
	for i := 0; i < len(rs); i++ {
		if mark, ok := emphasisMarks[rs[i]]; ok && opensEmphasis(rs, i) {
			if end := closingMarker(rs, i); end > 0 {
				b.WriteString(mark)
				b.WriteString(emphasizeLine(rs[i+1 : end]))
				b.WriteString(mark)
				i = end
				continue
			}
		}
		b.WriteRune(rs[i])
	}
	return b.String()
}

// opensEmphasis reports whether the marker at i starts a word: nothing
// word-like before it and a non-space after it.
func opensEmphasis(rs []rune, i int) bool {
	if i > 0 && (isWordRune(rs[i-1]) || rs[i-1] == rs[i]) {
		return false
	}
	return i+1 < len(rs) && !unicode.IsSpace(rs[i+1]) && rs[i+1] != rs[i]
}

// closingMarker returns the index of the marker that closes the one at
// open, or 0 when there is none.
func closingMarker(rs []rune, open int) int {
	for j := open + 2; j < len(rs); j++ {
		if rs[j] != rs[open] || unicode.IsSpace(rs[j-1]) {
			continue
		}
		if j+1 == len(rs) || (!isWordRune(rs[j+1]) && rs[j+1] != rs[open]) {
			return j
		}
	}
	return 0
}

func isWordRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r)
}
