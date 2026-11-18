package config

import (
	"strings"
	"testing"
)

const goodSecret = "0123456789abcdef0123456789abcdef"

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
	if c.ListenAddr != ":8080" || c.AdminRole != "admin" || c.ClassifierModel != "claude-haiku-5-5" {
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
	m["SLACK_BOT_TOKEN"], m["SLACK_APP_TOKEN"] = "xoxb-1", "xapp-1"
	c, err = Load(env(m))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !c.SlackEnabled() || c.MoveEnabled() {
		t.Errorf("with Slack only: SlackEnabled=%v MoveEnabled=%v", c.SlackEnabled(), c.MoveEnabled())
	}
}

// TestLoad_OneSlackToken_Rejected checks the tokens must come as a pair.
func TestLoad_OneSlackToken_Rejected(t *testing.T) {
	m := base()
	m["SLACK_BOT_TOKEN"] = "xoxb-1"
	if _, err := Load(env(m)); err == nil {
		t.Fatal("Load succeeded with one Slack token")
	}
}
