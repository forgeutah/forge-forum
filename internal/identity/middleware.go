package identity

import (
	"crypto/subtle"
	"database/sql"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/forgeutah/forge-forum/internal/config"
	"github.com/forgeutah/forge-forum/internal/db"
)

// Header names of forge-proxy's upstream contract, version 1.
const (
	headerSecret      = "X-Forge-Proxy-Secret"
	headerVersion     = "X-Forge-Contract-Version"
	headerUserID      = "X-Forge-User-Id"
	headerName        = "X-Forge-Name"
	headerAvatar      = "X-Forge-Avatar"
	headerRoles       = "X-Forge-Roles"
	headerSlackUserID = "X-Forge-Slack-User-Id"
	headerSlackTeamID = "X-Forge-Slack-Team-Id"

	contractVersion = "1"
	maxNameRunes    = 120
)

// Middleware enforces the forge-proxy upstream contract (plan KTD4) and
// upserts the member. A request that fails any check gets a bare 401; the
// wrapped handler never runs, so no forum content can leak.
//
// X-Forge-Email is deliberately ignored: the forum neither stores nor logs
// email addresses.
func Middleware(cfg config.Config, database *db.DB, logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if reason := checkContract(cfg, r); reason != "" {
				logger.Warn("proxy contract rejected", "reason", reason, "path", r.URL.Path)
				unauthorized(w)
				return
			}

			forgeUserID, err := strconv.ParseInt(r.Header.Get(headerUserID), 10, 64)
			if err != nil || forgeUserID <= 0 {
				logger.Warn("proxy contract rejected", "reason", "bad user id", "path", r.URL.Path)
				unauthorized(w)
				return
			}

			m := Member{
				SlackUserID: strings.TrimSpace(r.Header.Get(headerSlackUserID)),
				Name:        decodeName(r.Header.Get(headerName)),
				AvatarURL:   safeAvatar(r.Header.Get(headerAvatar)),
			}
			if m.Name == "" {
				m.Name = m.SlackUserID
			}

			err = database.WithWriteTx(r.Context(), func(tx *sql.Tx) error {
				var err error
				m, err = upsertSignedIn(r.Context(), tx, m, forgeUserID, time.Now())
				return err
			})
			if err != nil {
				logger.Error("member upsert failed", "error", err)
				http.Error(w, "Something went wrong on our side. Try again in a moment.", http.StatusInternalServerError)
				return
			}
			m.IsAdmin = hasRole(r.Header.Get(headerRoles), cfg.AdminRole)

			logger.Debug("request", "member_id", m.ID, "method", r.Method, "path", r.URL.Path, "admin", m.IsAdmin)
			next.ServeHTTP(w, r.WithContext(WithMember(r.Context(), m)))
		})
	}
}

// checkContract returns why the request fails the contract, or "" when it
// passes. The reason is for the log only; the client always sees a bare 401.
func checkContract(cfg config.Config, r *http.Request) string {
	if !secretAccepted(cfg.ProxySecrets, r.Header.Get(headerSecret)) {
		return "bad secret"
	}
	if v := r.Header.Get(headerVersion); v != "" && v != contractVersion {
		return "unsupported contract version"
	}
	if cfg.SlackTeamID == "" || r.Header.Get(headerSlackTeamID) != cfg.SlackTeamID {
		return "wrong team"
	}
	if strings.TrimSpace(r.Header.Get(headerSlackUserID)) == "" {
		return "no slack user"
	}
	return ""
}

// secretAccepted compares got against every accepted secret in constant
// time, without stopping at the first match.
func secretAccepted(accepted []string, got string) bool {
	ok := 0
	for _, want := range accepted {
		if want == "" {
			continue
		}
		ok |= subtle.ConstantTimeCompare([]byte(want), []byte(got))
	}
	return ok == 1
}

func unauthorized(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusUnauthorized)
	w.Write([]byte("unauthorized\n"))
}

// decodeName reads X-Forge-Name. forge-proxy sends ASCII names verbatim
// and anything else in the RFC 8187 form UTF-8'<lang>'<percent-encoded>.
// A value that claims the extended form but does not decode is kept as is.
func decodeName(raw string) string {
	name := strings.TrimSpace(raw)
	if len(name) > 6 && strings.EqualFold(name[:6], "utf-8'") {
		if _, encoded, ok := strings.Cut(name[6:], "'"); ok {
			if decoded, err := url.PathUnescape(encoded); err == nil && utf8.ValidString(decoded) {
				name = strings.TrimSpace(decoded)
			}
		}
	}
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, name)
	if utf8.RuneCountInString(name) > maxNameRunes {
		name = string([]rune(name)[:maxNameRunes])
	}
	return name
}

// safeAvatar keeps an avatar only when it is an absolute https URL, since
// it is rendered as an image source.
func safeAvatar(raw string) string {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return ""
	}
	return raw
}

// hasRole reports whether the comma-separated roles header contains role.
func hasRole(header, role string) bool {
	if role == "" {
		return false
	}
	for r := range strings.SplitSeq(header, ",") {
		if strings.TrimSpace(r) == role {
			return true
		}
	}
	return false
}
