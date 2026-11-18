// Package config loads forge-forum's settings from the environment.
//
// Configuration is env-only, matching forge-proxy. Load collects every
// problem it finds and reports them together so an operator fixes the
// env file in one pass.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// minSecretLen matches forge-proxy's PROXY_SECRET minimum.
const minSecretLen = 32

var teamIDPattern = regexp.MustCompile(`^T[A-Z0-9]+$`)

// Config is the validated runtime configuration.
type Config struct {
	ListenAddr string
	DBPath     string
	LogLevel   string

	// ProxySecrets are the accepted X-Forge-Proxy-Secret values. More than
	// one is allowed so the shared secret can be rotated without downtime.
	ProxySecrets []string
	SlackTeamID  string
	AdminRole    string

	// PublicBaseURL is the forum's address as members see it, with no
	// trailing slash. The forum cannot infer it: forge-proxy rewrites Host.
	PublicBaseURL string
	// AuthPortalURL is where the sign-out link points. Optional.
	AuthPortalURL string

	SlackBotToken        string
	SlackAppToken        string
	SlackAnnounceChannel string

	AnthropicAPIKey string
	ClassifierModel string
}

// SlackEnabled reports whether the forum can talk to Slack at all.
func (c Config) SlackEnabled() bool {
	return c.SlackBotToken != "" && c.SlackAppToken != ""
}

// MoveEnabled reports whether "Move to forum" can run: it needs Slack and
// a classifier.
func (c Config) MoveEnabled() bool {
	return c.SlackEnabled() && c.AnthropicAPIKey != ""
}

// Load reads configuration through getenv (os.Getenv in production).
func Load(getenv func(string) string) (Config, error) {
	get := func(key, def string) string {
		if v := strings.TrimSpace(getenv(key)); v != "" {
			return v
		}
		return def
	}

	c := Config{
		ListenAddr:           get("LISTEN_ADDR", ":8080"),
		DBPath:               get("DB_PATH", "forum.db"),
		LogLevel:             get("LOG_LEVEL", "info"),
		SlackTeamID:          get("SLACK_TEAM_ID", ""),
		AdminRole:            get("ADMIN_ROLE", "admin"),
		PublicBaseURL:        strings.TrimRight(get("PUBLIC_BASE_URL", ""), "/"),
		AuthPortalURL:        get("AUTH_PORTAL_URL", ""),
		SlackBotToken:        get("SLACK_BOT_TOKEN", ""),
		SlackAppToken:        get("SLACK_APP_TOKEN", ""),
		SlackAnnounceChannel: get("SLACK_ANNOUNCE_CHANNEL", ""),
		AnthropicAPIKey:      get("ANTHROPIC_API_KEY", ""),
		ClassifierModel:      get("CLASSIFIER_MODEL", "claude-haiku-5-5"),
	}

	var errs []error

	for s := range strings.SplitSeq(getenv("PROXY_SECRET"), ",") {
		if s = strings.TrimSpace(s); s != "" {
			c.ProxySecrets = append(c.ProxySecrets, s)
		}
	}
	if len(c.ProxySecrets) == 0 {
		errs = append(errs, errors.New("PROXY_SECRET is required"))
	}
	for i, s := range c.ProxySecrets {
		if len(s) < minSecretLen {
			errs = append(errs, fmt.Errorf("PROXY_SECRET entry %d is shorter than %d characters", i+1, minSecretLen))
		}
	}

	if c.SlackTeamID == "" {
		errs = append(errs, errors.New("SLACK_TEAM_ID is required"))
	} else if !teamIDPattern.MatchString(c.SlackTeamID) {
		errs = append(errs, fmt.Errorf("SLACK_TEAM_ID %q does not look like a Slack team ID", c.SlackTeamID))
	}

	if c.PublicBaseURL == "" {
		errs = append(errs, errors.New("PUBLIC_BASE_URL is required"))
	} else if u, err := url.Parse(c.PublicBaseURL); err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		errs = append(errs, fmt.Errorf("PUBLIC_BASE_URL %q must be an http(s) URL", c.PublicBaseURL))
	}

	if (c.SlackBotToken == "") != (c.SlackAppToken == "") {
		errs = append(errs, errors.New("SLACK_BOT_TOKEN and SLACK_APP_TOKEN must be set together"))
	}

	if len(errs) > 0 {
		return Config{}, errors.Join(errs...)
	}
	return c, nil
}
