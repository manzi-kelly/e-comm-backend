package payment

import (
	"regexp"
	"strings"
)

var (
	mtnMSISDN    = regexp.MustCompile(`^2507[89][0-9]{7}$`)
	airtelMSISDN = regexp.MustCompile(`^2507[23][0-9]{7}$`)
)

// NormalizeRwandaMSISDN accepts local, country-coded and spaced Rwanda mobile
// money numbers and returns the provider form (250781234567), E.164 form and
// the routable network. It never accepts a PIN, short code or foreign number.
func NormalizeRwandaMSISDN(raw string) (string, string, string, error) {
	value := strings.TrimSpace(raw)
	value = strings.NewReplacer(" ", "", "-", "", "\t", "", "\n", "", "\r", "").Replace(value)
	if value == "" || len(value) > 20 {
		return "", "", "", ErrInvalidPhone
	}
	var digits string
	switch {
	case strings.HasPrefix(value, "+250"):
		digits = value[1:]
	case strings.HasPrefix(value, "250"):
		digits = value
	case strings.HasPrefix(value, "0"):
		digits = "250" + value[1:]
	default:
		return "", "", "", ErrInvalidPhone
	}
	for _, r := range digits {
		if r < '0' || r > '9' {
			return "", "", "", ErrInvalidPhone
		}
	}
	network := ""
	switch {
	case mtnMSISDN.MatchString(digits):
		network = "mtn"
	case airtelMSISDN.MatchString(digits):
		network = "airtel"
	default:
		return "", "", "", ErrInvalidPhone
	}
	return digits, "+" + digits, network, nil
}
