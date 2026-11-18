package markdown

import (
	"strings"
	"testing"
)

// names is a SlackResolver backed by a map of user ID to display name.
type names map[string]string

func (n names) UserName(userID string) string { return n[userID] }

var testNames = names{"U123": "Ada Lovelace", "U456": "Grace_Hopper"}

func assertFromSlack(t *testing.T, in, want string) {
	t.Helper()
	if got := FromSlack(in, testNames); got != want {
		t.Errorf("FromSlack(%q)\n got: %q\nwant: %q", in, got, want)
	}
}

// TestFromSlack_Mentions_ResolveToDisplayNames checks user, group, and
// channel references, including the fallbacks for an unknown user.
func TestFromSlack_Mentions_ResolveToDisplayNames(t *testing.T) {
	assertFromSlack(t, "hi <@U123>!", "hi @Ada Lovelace!")
	assertFromSlack(t, "hi <@U123|ada>", "hi @Ada Lovelace")
	assertFromSlack(t, "ask <@U999|bob>", "ask @bob")
	assertFromSlack(t, "ask <@U999>", "ask @unknown")
	assertFromSlack(t, "<!here> <!channel> <!everyone>", "@here @channel @everyone")
	assertFromSlack(t, "<!here|here> look", "@here look")
	assertFromSlack(t, "cc <!subteam^S123|@devs>", "cc @devs")
	assertFromSlack(t, "in <#C123|general> and <#C456>", "in #general and #unknown")
}

// TestFromSlack_MentionName_MarkdownEscaped checks that a display name
// cannot introduce emphasis of its own.
func TestFromSlack_MentionName_MarkdownEscaped(t *testing.T) {
	assertFromSlack(t, "<@U456> said", `@Grace\_Hopper said`)
}

// TestFromSlack_NilResolver_FallsBack checks that a nil resolver is safe.
func TestFromSlack_NilResolver_FallsBack(t *testing.T) {
	if got := FromSlack("<@U123|ada> and <@U123>", nil); got != "@ada and @unknown" {
		t.Errorf("got %q", got)
	}
}

// TestFromSlack_Links_BecomeMarkdownLinks checks labelled, bare, and
// mailto links, and several links on one line.
func TestFromSlack_Links_BecomeMarkdownLinks(t *testing.T) {
	assertFromSlack(t, "see <https://a.test/x?a=1&amp;b=2|the docs>", "see [the docs](https://a.test/x?a=1&b=2)")
	assertFromSlack(t, "see <https://a.test/x_y_z>.", "see https://a.test/x_y_z.")
	assertFromSlack(t, "<mailto:a@b.test|mail me>", "[mail me](mailto:a@b.test)")
	assertFromSlack(t, "<mailto:a@b.test>", "[a@b.test](mailto:a@b.test)")
	assertFromSlack(t,
		"<https://a.test|one>, <https://b.test> and <https://c.test/p_(q)|th[r]ee>",
		`[one](https://a.test), https://b.test and [th\[r\]ee](https://c.test/p_%28q%29)`)
}

// TestFromSlack_UnsupportedScheme_NotLinked checks that only web and mail
// links become links.
func TestFromSlack_UnsupportedScheme_NotLinked(t *testing.T) {
	assertFromSlack(t, "<javascript:alert(1)|click>", "click")
}

// TestFromSlack_Emphasis_Converts checks bold, italic, and strike.
func TestFromSlack_Emphasis_Converts(t *testing.T) {
	assertFromSlack(t, "*bold* and ~gone~ and _it_", "**bold** and ~~gone~~ and *it*")
	assertFromSlack(t, "*two words* here", "**two words** here")
}

// TestFromSlack_NestedEmphasisNextToPunctuation_Converts checks nesting
// and markers that touch punctuation or a link.
func TestFromSlack_NestedEmphasisNextToPunctuation_Converts(t *testing.T) {
	assertFromSlack(t, "(*bold _both_*), ok.", "(**bold *both***), ok.")
	assertFromSlack(t, "_*both*_!", "***both***!")
	assertFromSlack(t, "*<https://a.test|x>*", "**[x](https://a.test)**")
	assertFromSlack(t, "*see `code`*", "**see `code`**")
}

// TestFromSlack_NonEmphasisMarkers_LeftAlone checks markers that Slack
// does not treat as formatting.
func TestFromSlack_NonEmphasisMarkers_LeftAlone(t *testing.T) {
	for _, s := range []string{"2 * 3 * 4", "snake_case_name", "a*b*c", "~ 5 ~", "*open only", "line *one\ntwo* end"} {
		assertFromSlack(t, s, s)
	}
}

// TestFromSlack_InlineCode_Untouched checks that formatting, mentions,
// and links inside inline code stay literal.
func TestFromSlack_InlineCode_Untouched(t *testing.T) {
	assertFromSlack(t, "use `*x* _y_ <@U123> <https://a.test|l>` now", "use `*x* _y_ <@U123> <https://a.test|l>` now")
	assertFromSlack(t, "*b* `*x*` *c*", "**b** `*x*` **c**")
}

// TestFromSlack_FencedCode_UntouchedOnOwnLines checks that a fenced block
// keeps its content and is placed on its own lines.
func TestFromSlack_FencedCode_UntouchedOnOwnLines(t *testing.T) {
	assertFromSlack(t, "run ```go test *all* <@U123>``` ok", "run\n```\ngo test *all* <@U123>\n```\nok")
	assertFromSlack(t, "```\na _b_\n  c\n```", "```\na _b_\n  c\n```")
	assertFromSlack(t, "before\n```x```\nafter", "before\n```\nx\n```\nafter")
}

// TestFromSlack_CodeEntities_UnescapedOnce checks that Slack's escaping is
// undone inside code, where Markdown would otherwise show it literally.
func TestFromSlack_CodeEntities_UnescapedOnce(t *testing.T) {
	assertFromSlack(t, "`a &lt; b &amp;&amp; c`", "`a < b && c`")
	assertFromSlack(t, "```if a &gt; b {}```", "```\nif a > b {}\n```")
}

// TestFromSlack_UnclosedCode_TreatedAsText checks that a stray backtick or
// fence does not swallow the rest of the message.
func TestFromSlack_UnclosedCode_TreatedAsText(t *testing.T) {
	assertFromSlack(t, "a ` b *c*", "a ` b **c**")
	assertFromSlack(t, "a ``` b *c*", "a ``` b **c**")
}

// TestFromSlack_Entities_UnescapedOnce checks the three Slack entities
// and that an escaped entity is not unescaped twice.
func TestFromSlack_Entities_UnescapedOnce(t *testing.T) {
	assertFromSlack(t, "a &lt; b &amp;&amp; c &gt; d", "a < b && c > d")
	assertFromSlack(t, "&amp;lt;", "&lt;")
	assertFromSlack(t, "&amp;amp; &quot;", "&amp; &quot;")
	assertFromSlack(t, "<https://a.test|a &amp;lt; b>", "[a &lt; b](https://a.test)")
}

// TestFromSlack_Quote_BecomesBlockquote checks the quote prefix and that
// the line after a quote does not join it.
func TestFromSlack_Quote_BecomesBlockquote(t *testing.T) {
	assertFromSlack(t, "&gt; quoted *text*\n&gt; more\nreply", "> quoted **text**\n> more\n\nreply")
	assertFromSlack(t, "a &gt; b", "a > b")
}

// TestFromSlack_PlaceholderRunesInInput_Removed checks that input cannot
// forge the converter's internal placeholders.
func TestFromSlack_PlaceholderRunesInInput_Removed(t *testing.T) {
	got := FromSlack("x 0 `code`", testNames)
	if got != "x 0 `code`" {
		t.Errorf("got %q", got)
	}
	if strings.ContainsAny(got, "") {
		t.Errorf("placeholder rune leaked: %q", got)
	}
}

// TestFromSlack_PlainAndEmpty_Unchanged checks text with nothing to convert.
func TestFromSlack_PlainAndEmpty_Unchanged(t *testing.T) {
	assertFromSlack(t, "", "")
	assertFromSlack(t, "just words\nand lines", "just words\nand lines")
}
