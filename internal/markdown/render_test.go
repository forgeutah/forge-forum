package markdown

import (
	"strings"
	"testing"
)

// TestRender_ScriptAndFencedCode_KeepsCodeDropsScript checks that a fenced
// code block survives and a script tag never reaches the page as markup.
func TestRender_ScriptAndFencedCode_KeepsCodeDropsScript(t *testing.T) {
	src := "Hello\n\n<script>alert('x')</script>\n\n```go\nfmt.Println(\"<b>hi</b>\")\n```\n"
	out := string(Render(src))

	if strings.Contains(strings.ToLower(out), "<script") {
		t.Errorf("output contains a script tag: %s", out)
	}
	if !strings.Contains(out, "<pre><code") || !strings.Contains(out, "fmt.Println") {
		t.Errorf("fenced code block missing: %s", out)
	}
	if !strings.Contains(out, `class="language-go"`) {
		t.Errorf("code language class missing: %s", out)
	}
	if strings.Contains(out, "<b>hi</b>") {
		t.Errorf("HTML inside the code block was not escaped: %s", out)
	}
}

// TestRender_RawHTML_NeverPassesThrough checks inline and block raw HTML,
// event handlers, and script-scheme links.
func TestRender_RawHTML_NeverPassesThrough(t *testing.T) {
	cases := map[string]string{
		"inline handler":  `text <img src=x onerror="alert(1)"> more`,
		"iframe":          "<iframe src=\"https://evil.example\"></iframe>",
		"style block":     "<style>body{display:none}</style>",
		"js link":         "[click](javascript:alert(1))",
		"data link":       "[click](data:text/html;base64,PHNjcmlwdD4=)",
		"form":            `<form action="https://evil.example"><input name="q"></form>`,
		"class injection": "```go\" onmouseover=\"alert(1)\nx\n```",
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			out := strings.ToLower(string(Render(src)))
			// A dangerous construct may only survive as escaped text.
			for _, bad := range []string{"<img", "<iframe", "<style", "<form", "<input", `href="javascript:`, `href="data:`, ` onerror="`, ` onmouseover="`} {
				if strings.Contains(out, bad) {
					t.Errorf("output contains %q: %s", bad, out)
				}
			}
		})
	}
}

// TestRender_CommonMarkdown_Renders checks the formatting members rely on.
func TestRender_CommonMarkdown_Renders(t *testing.T) {
	cases := map[string]string{
		"**bold** and *em*":                 "<strong>bold</strong> and <em>em</em>",
		"use `go vet`":                      "<code>go vet</code>",
		"- one\n- two":                      "<li>one</li>",
		"> quoted":                          "<blockquote>",
		"[forge](https://forgeutah.tech/x)": `href="https://forgeutah.tech/x"`,
		"see https://forgeutah.tech/y ok":   `href="https://forgeutah.tech/y"`,
		"~~gone~~":                          "<del>gone</del>",
		"| a | b |\n|---|---|\n| 1 | 2 |":   "<table>",
		"## Heading":                        "<h2",
	}
	for src, want := range cases {
		t.Run(src, func(t *testing.T) {
			if out := string(Render(src)); !strings.Contains(out, want) {
				t.Errorf("Render(%q) = %s, want it to contain %s", src, out, want)
			}
		})
	}
}

// TestRender_Links_GetNoFollow checks that member-written links cannot
// pass ranking or the opener to the target.
func TestRender_Links_GetNoFollow(t *testing.T) {
	out := string(Render("[x](https://example.com)"))
	if !strings.Contains(out, "nofollow") {
		t.Errorf("link has no rel=nofollow: %s", out)
	}
}

// TestRender_Empty_ReturnsEmpty checks the zero case.
func TestRender_Empty_ReturnsEmpty(t *testing.T) {
	if out := Render(""); out != "" {
		t.Errorf("Render(\"\") = %q", out)
	}
}
