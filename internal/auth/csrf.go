package auth

import (
	"crypto/subtle"
	"net/http"
	"net/url"
	"strings"
)

// RequiresCSRF reports whether the request method requires CSRF protection.
func RequiresCSRF(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace:
		return false
	default:
		return true
	}
}

// ValidateOrigin checks that the Origin header, when present, is allowed for state-changing requests.
// If Origin is absent, Referer is checked as a fallback.
func ValidateOrigin(r *http.Request, allowedOrigins []string) bool {
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin != "" {
		for _, allowed := range allowedOrigins {
			if origin == allowed {
				return true
			}
		}
		return false
	}
	referer := strings.TrimSpace(r.Header.Get("Referer"))
	if referer == "" {
		// No origin context available; require CSRF token validation to proceed.
		return true
	}
	u, err := url.Parse(referer)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return false
	}
	refOrigin := u.Scheme + "://" + u.Host
	for _, allowed := range allowedOrigins {
		if refOrigin == allowed {
			return true
		}
	}
	return false
}

// ValidCSRFToken reports whether the double-submit CSRF cookie matches the request header.
func ValidCSRFToken(r *http.Request, cookieName, headerName string) bool {
	cookie, err := r.Cookie(cookieName)
	if err != nil || cookie == nil {
		return false
	}
	cookieValue := strings.TrimSpace(cookie.Value)
	headerValue := strings.TrimSpace(r.Header.Get(headerName))
	if cookieValue == "" || headerValue == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(cookieValue), []byte(headerValue)) == 1
}
