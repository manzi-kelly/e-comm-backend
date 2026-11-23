package customer

import (
	"regexp"
	"strings"
	"unicode/utf8"

	"autivashop/backend/internal/auth"
	"autivashop/backend/internal/product"
)

var (
	emailShape        = regexp.MustCompile(`^[^\s@]+@[^\s@]+\.[A-Za-z]{2,}$`)
	rwandaMobileLocal = regexp.MustCompile(`^7[0-9]{8}$`)
	rwandaPhoneE164   = regexp.MustCompile(`^\+2507[0-9]{8}$`)
)

// NormalizeEmail matches the storefront's case-insensitive account identity.
func NormalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

func collapseWhitespace(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

// ValidEmail mirrors the storefront account/checkout validators. It rejects
// malformed or oversized addresses without applying any transformation.
func ValidEmail(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || utf8.RuneCountInString(value) > 254 || !emailShape.MatchString(value) ||
		strings.Contains(value, "..") || strings.HasPrefix(value, ".") || strings.HasSuffix(value, ".") {
		return false
	}
	parts := strings.Split(value, "@")
	if len(parts) != 2 || len(parts[0]) < 1 || len(parts[0]) > 64 {
		return false
	}
	domain := parts[1]
	return domain != "" && !strings.HasPrefix(domain, "-") && !strings.HasSuffix(domain, "-")
}

// NormalizeRwandaPhone accepts 07XXXXXXXX, +250 7XXXXXXXX, 00250 7XXXXXXXX,
// 250 7XXXXXXXX and 7XXXXXXXX. Missing digits are never invented.
func NormalizeRwandaPhone(raw string) (e164, display string, ok bool) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || len(trimmed) > 32 {
		return "", "", false
	}
	compact := strings.Map(func(r rune) rune {
		switch r {
		case ' ', '\t', '\n', '\r', '(', ')', '.', '-':
			return -1
		default:
			return r
		}
	}, trimmed)
	if compact == "" || strings.Trim(compact, "+0123456789") != "" {
		return "", "", false
	}
	var national string
	switch {
	case strings.HasPrefix(compact, "+250"):
		national = strings.TrimPrefix(compact, "+250")
	case strings.HasPrefix(compact, "00250"):
		national = strings.TrimPrefix(compact, "00250")
	case strings.HasPrefix(compact, "250") && len(compact) == 12:
		national = strings.TrimPrefix(compact, "250")
	case strings.HasPrefix(compact, "0"):
		national = strings.TrimPrefix(compact, "0")
	default:
		national = compact
	}
	if !rwandaMobileLocal.MatchString(national) {
		return "", "", false
	}
	return "+250" + national,
		"+250 " + national[:3] + " " + national[3:6] + " " + national[6:9], true
}

func ValidPhoneE164(value string) bool { return rwandaPhoneE164.MatchString(value) }

// NormalizedRegistration is the validated, server-normalized input passed to
// the repository. No caller-supplied identity is trusted verbatim.
type NormalizedRegistration struct {
	Name         string
	Email        string
	PhoneE164    string
	PhoneDisplay string
	Password     string
}

func ValidateRegistration(in RegistrationInput) (NormalizedRegistration, map[string][]string) {
	fields := map[string][]string{}
	add := func(key string) { fields[key] = []string{"invalid"} }
	name := collapseWhitespace(in.FullName)
	email := NormalizeEmail(in.Email)
	e164, display, phoneOK := NormalizeRwandaPhone(in.Phone)
	if !product.SafeText(name, 120, true) || utf8.RuneCountInString(name) < 2 {
		add("fullName")
	}
	if !ValidEmail(email) {
		add("email")
	}
	if !phoneOK {
		add("phone")
	}
	if err := auth.ValidatePasswordStrength(in.Password); err != nil {
		add("password")
	}
	if in.ConfirmPassword != "" && in.ConfirmPassword != in.Password {
		add("confirmPassword")
	}
	if len(fields) != 0 {
		return NormalizedRegistration{}, fields
	}
	return NormalizedRegistration{
		Name:         name,
		Email:        email,
		PhoneE164:    e164,
		PhoneDisplay: display,
		Password:     in.Password,
	}, nil
}
