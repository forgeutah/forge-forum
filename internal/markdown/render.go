// Package markdown renders post bodies. Member-written posts are Markdown;
// text imported from Slack is converted to Markdown first.
package markdown

import (
	"bytes"
	"html/template"
	"regexp"

	"github.com/microcosm-cc/bluemonday"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
)

// renderer leaves goldmark's raw-HTML passthrough off, so any HTML a
// member types is dropped before the sanitizer ever sees it.
var renderer = goldmark.New(
	goldmark.WithExtensions(extension.Table, extension.Strikethrough, extension.Linkify),
)

// policy is the second line of defence: even if the renderer emitted
// something unexpected, only the user-content allowlist survives.
var policy = func() *bluemonday.Policy {
	p := bluemonday.UGCPolicy()
	// goldmark marks a fenced block's language as class="language-go".
	p.AllowAttrs("class").Matching(regexp.MustCompile(`^language-[A-Za-z0-9_+.-]+$`)).OnElements("code")
	p.RequireNoReferrerOnLinks(true)
	return p
}()

// Render converts member-written Markdown to sanitized HTML: goldmark with
// raw HTML disabled, then bluemonday's user-content policy.
func Render(src string) template.HTML {
	var buf bytes.Buffer
	if err := renderer.Convert([]byte(src), &buf); err != nil {
		// goldmark only fails when the writer does; fall back to plain text.
		return template.HTML("<p>" + template.HTMLEscapeString(src) + "</p>")
	}
	return template.HTML(policy.SanitizeBytes(buf.Bytes()))
}
