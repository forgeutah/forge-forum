package config

import (
	"strings"
	"testing"
)

const (
	goodSecret = "0123456789abcdef0123456789abcdef"
	goodToken  = "forum-service-token-0123456789abcdef"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func base() map[string]string {
	return map[string]string{
		"PROXY_SECRET":    goodSecret,
		"SLACK_TEAM_ID":   "T0R7G",
		"PUBLIC_BASE_URL": "https://forum.example.test/",
	}
}

// TestLoad_RequiredValues_Loads checks the minimal valid environment and
// the defaults it implies.
func TestLoad_RequiredValues_Loads(t *testing.T) {
	c, err := Load(env(base()))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.ListenAddr != ":8080" || c.AdminRole != "admin" || c.ClassifierModel != "claude-sonnet-5-5" {
		t.Errorf("unexpected defaults: %+v", c)
	}
	if c.PublicBaseURL != "https://forum.example.test" {
		t.Errorf("PublicBaseURL = %q, want trailing slash trimmed", c.PublicBaseURL)
	}
}

// TestLoad_MissingAndShortSecret_ReportedTogether checks that every
// problem is reported in one error.
func TestLoad_MissingAndShortSecret_ReportedTogether(t *testing.T) {
	m := base()
	m["PROXY_SECRET"] = "short"
	m["SLACK_TEAM_ID"] = ""
	_, err := Load(env(m))
	if err == nil {
		t.Fatal("Load succeeded, want error")
	}
	for _, want := range []string{"PROXY_SECRET entry 1", "SLACK_TEAM_ID is required"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err, want)
		}
	}

	delete(m, "PROXY_SECRET")
	if _, err := Load(env(m)); err == nil || !strings.Contains(err.Error(), "PROXY_SECRET is required") {
		t.Errorf("missing secret: got %v", err)
	}
}

// TestLoad_TwoSecrets_BothAccepted checks the rotation list.
func TestLoad_TwoSecrets_BothAccepted(t *testing.T) {
	m := base()
	m["PROXY_SECRET"] = goodSecret + " , " + strings.Repeat("z", 32)
	c, err := Load(env(m))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(c.ProxySecrets) != 2 {
		t.Errorf("ProxySecrets = %v, want 2 entries", c.ProxySecrets)
	}
}

// TestLoad_SlackAndAnthropicAbsent_FeaturesOff checks that optional
// integrations being unset is a valid configuration.
func TestLoad_SlackAndAnthropicAbsent_FeaturesOff(t *testing.T) {
	c, err := Load(env(base()))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.SlackEnabled() || c.MoveEnabled() {
		t.Errorf("SlackEnabled=%v MoveEnabled=%v, want both false", c.SlackEnabled(), c.MoveEnabled())
	}

	m := base()
	m["FORGE_BOT_URL"], m["FORGE_BOT_TOKEN"] = "http://forge-bot:8080/", goodToken
	c, err = Load(env(m))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !c.SlackEnabled() || c.MoveEnabled() {
		t.Errorf("with the bot only: SlackEnabled=%v MoveEnabled=%v", c.SlackEnabled(), c.MoveEnabled())
	}
	if c.ForgeBotURL != "http://forge-bot:8080" || c.ForgeBotToken != goodToken {
		t.Errorf("ForgeBotURL = %q, ForgeBotToken = %q; want the trailing slash trimmed and the token kept", c.ForgeBotURL, c.ForgeBotToken)
	}

	m["ANTHROPIC_API_KEY"] = "sk-ant-test"
	c, err = Load(env(m))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !c.SlackEnabled() || !c.MoveEnabled() {
		t.Errorf("with the bot and a classifier key: SlackEnabled=%v MoveEnabled=%v", c.SlackEnabled(), c.MoveEnabled())
	}
}

// TestLoad_AnthropicKeyWithoutBot_MoveOff checks a classifier key alone
// does not turn on Move to forum.
func TestLoad_AnthropicKeyWithoutBot_MoveOff(t *testing.T) {
	m := base()
	m["ANTHROPIC_API_KEY"] = "sk-ant-test"
	c, err := Load(env(m))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.SlackEnabled() || c.MoveEnabled() {
		t.Errorf("SlackEnabled=%v MoveEnabled=%v, want both false", c.SlackEnabled(), c.MoveEnabled())
	}
}

// TestLoad_OneBotSetting_Rejected checks the bot URL and token must come
// as a pair.
func TestLoad_OneBotSetting_Rejected(t *testing.T) {
	for _, key := range []string{"FORGE_BOT_URL", "FORGE_BOT_TOKEN"} {
		m := base()
		m["FORGE_BOT_URL"], m["FORGE_BOT_TOKEN"] = "http://forge-bot:8080", goodToken
		delete(m, key)
		if _, err := Load(env(m)); err == nil || !strings.Contains(err.Error(), "must be set together") {
			t.Errorf("without %s: got %v, want a must-be-set-together error", key, err)
		}
	}
}

// TestLoad_BadBotSettings_Rejected checks the bot URL must be an http(s)
// URL and the token at least 32 characters.
func TestLoad_BadBotSettings_Rejected(t *testing.T) {
	cases := map[string]struct{ url, token, want string }{
		"no scheme":    {"forge-bot:8080", goodToken, "FORGE_BOT_URL"},
		"other scheme": {"ws://forge-bot:8080", goodToken, "FORGE_BOT_URL"},
		"no host":      {"http://", goodToken, "FORGE_BOT_URL"},
		"short token":  {"http://forge-bot:8080", strings.Repeat("t", 31), "FORGE_BOT_TOKEN is shorter than 32"},
	}
	for name, tc := range cases {
		m := base()
		m["FORGE_BOT_URL"], m["FORGE_BOT_TOKEN"] = tc.url, tc.token
		if _, err := Load(env(m)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want an error naming %q", name, err, tc.want)
		}
	}
}

// TestLoad_OldSlackTokens_Ignored checks the settings the forum no longer
// reads do not turn Slack on.
func TestLoad_OldSlackTokens_Ignored(t *testing.T) {
	m := base()
	m["SLACK_BOT_TOKEN"], m["SLACK_APP_TOKEN"] = "xoxb-1", "xapp-1"
	c, err := Load(env(m))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.SlackEnabled() {
		t.Error("SlackEnabled with only the old token settings")
	}
}
