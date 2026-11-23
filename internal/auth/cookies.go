package auth

import (
	"net/http"
	"strings"
	"time"

	"autivashop/backend/internal/config"
)

// CookieConfig describes session and CSRF cookie attributes.
type CookieConfig struct {
	SessionName string
	CSRFName    string
	Domain      string
	Secure      bool
	SameSite    http.SameSite
	Path        string
}

func NewCookieConfig(cfg config.AuthConfig) CookieConfig {
	return CookieConfig{
		SessionName: cfg.SessionCookieName,
		CSRFName:    cfg.CSRFCookieName,
		Domain:      cfg.CookieDomain,
		Secure:      cfg.CookieSecure,
		SameSite:    parseSameSite(cfg.CookieSameSite),
		Path:        "/",
	}
}

// NewCustomerCookieConfig reuses deployment cookie security attributes but
// enforces the separate customer cookie names. It cannot produce an admin
// cookie configuration.
func NewCustomerCookieConfig(cfg config.Config) CookieConfig {
	return CookieConfig{
		SessionName: cfg.CustomerAuth.SessionCookieName,
		CSRFName:    cfg.CustomerAuth.CSRFCookieName,
		Domain:      cfg.Auth.CookieDomain,
		Secure:      cfg.Auth.CookieSecure,
		SameSite:    parseSameSite(cfg.Auth.CookieSameSite),
		Path:        "/",
	}
}

func parseSameSite(v string) http.SameSite {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "strict":
		return http.SameSiteStrictMode
	case "none":
		return http.SameSiteNoneMode
	default:
		return http.SameSiteLaxMode
	}
}

// SetSessionCookie sets the HttpOnly session cookie.
func (c CookieConfig) SetSessionCookie(w http.ResponseWriter, token string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name:     c.SessionName,
		Value:    token,
		Path:     c.Path,
		Domain:   c.Domain,
		Expires:  expires,
		MaxAge:   int(time.Until(expires).Seconds()),
		HttpOnly: true,
		Secure:   c.Secure,
		SameSite: c.SameSite,
	})
}

// ClearSessionCookie expires the session cookie.
func (c CookieConfig) ClearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     c.SessionName,
		Value:    "",
		Path:     c.Path,
		Domain:   c.Domain,
		MaxAge:   -1,
		Expires:  time.Unix(0, 0),
		HttpOnly: true,
		Secure:   c.Secure,
		SameSite: c.SameSite,
	})
}

// SetCSRFCookie sets a non-HttpOnly CSRF cookie for double-submit protection.
func (c CookieConfig) SetCSRFCookie(w http.ResponseWriter, token string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name:     c.CSRFName,
		Value:    token,
		Path:     c.Path,
		Domain:   c.Domain,
		Expires:  expires,
		MaxAge:   int(time.Until(expires).Seconds()),
		HttpOnly: false,
		Secure:   c.Secure,
		SameSite: c.SameSite,
	})
}

// ClearCSRFCookie expires the CSRF cookie.
func (c CookieConfig) ClearCSRFCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     c.CSRFName,
		Value:    "",
		Path:     c.Path,
		Domain:   c.Domain,
		MaxAge:   -1,
		Expires:  time.Unix(0, 0),
		HttpOnly: false,
		Secure:   c.Secure,
		SameSite: c.SameSite,
	})
}
