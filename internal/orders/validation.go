package orders

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"autivashop/backend/internal/customer"
	"autivashop/backend/internal/product"
)

var idempotencyKeyPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{8,128}$`)

func ValidIdempotencyKey(value string) bool {
	return idempotencyKeyPattern.MatchString(value)
}

func optional(value string, max int) bool {
	return value == "" || product.SafeText(value, max, false)
}

func required(value string, min, max int) bool {
	return product.SafeText(value, max, true) && utf8.RuneCountInString(strings.TrimSpace(value)) >= min
}

// Decode validates and normalizes a raw checkout request. It rejects any
// client-supplied price, total, status or reference through StrictJSON's
// unknown-field rejection.
func Decode(data []byte) (NormalizedInput, map[string][]string) {
	var in Input
	if err := product.StrictJSON(data, &in); err != nil {
		return NormalizedInput{}, map[string][]string{"body": {"invalid"}}
	}
	var fields map[string]json.RawMessage
	if err := product.StrictJSON(data, &fields); err != nil {
		return NormalizedInput{}, map[string][]string{"body": {"invalid"}}
	}
	for _, key := range []string{"fullName", "email", "phone", "province", "district", "sector", "addressLine", "items"} {
		raw, ok := fields[key]
		if !ok || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return NormalizedInput{}, map[string][]string{key: {"required"}}
		}
	}

	invalid := map[string][]string{}
	add := func(key string) { invalid[key] = []string{"invalid"} }
	name := strings.Join(strings.Fields(in.FullName), " ")
	email := customer.NormalizeEmail(in.Email)
	e164, display, phoneOK := customer.NormalizeRwandaPhone(in.Phone)
	province := strings.Join(strings.Fields(in.Province), " ")
	district := strings.Join(strings.Fields(in.District), " ")
	sector := strings.Join(strings.Fields(in.Sector), " ")
	cell := strings.Join(strings.Fields(in.Cell), " ")
	address := strings.Join(strings.Fields(in.AddressLine), " ")
	instructions := strings.TrimSpace(in.Instructions)

	if !required(name, 2, 120) {
		add("fullName")
	}
	if !customer.ValidEmail(email) {
		add("email")
	}
	if !phoneOK {
		add("phone")
	}
	if !required(province, 1, 120) {
		add("province")
	}
	if !required(district, 1, 120) {
		add("district")
	}
	if !required(sector, 1, 120) {
		add("sector")
	}
	if !optional(cell, 120) {
		add("cell")
	}
	if !required(address, 1, 160) {
		add("addressLine")
	}
	if !optional(instructions, 500) {
		add("instructions")
	}
	if len(in.Items) < 1 || len(in.Items) > 20 {
		add("items")
	}
	seen := map[string]bool{}
	for i, item := range in.Items {
		key := "items[" + strconv.Itoa(i) + "]"
		if !product.ValidID(item.ProductID) || seen[item.ProductID] {
			add(key + ".productId")
		}
		seen[item.ProductID] = true
		// Backend 0003 has no variant data; only the frontend's base variant
		// sentinel or an omitted value is accepted.
		if item.VariantID != "" && item.VariantID != "base" {
			add(key + ".variantId")
		}
		if item.Quantity < 1 || item.Quantity > 99 {
			add(key + ".quantity")
		}
	}
	if len(invalid) != 0 {
		return NormalizedInput{}, invalid
	}

	normalized := NormalizedInput{
		FullName:     name,
		Email:        email,
		PhoneE164:    e164,
		PhoneDisplay: display,
		Province:     province,
		District:     district,
		Sector:       sector,
		Cell:         cell,
		Instructions: instructions,
		AddressLine:  address,
		Items:        in.Items,
	}
	fingerprintPayload, err := json.Marshal(struct {
		FullName     string      `json:"fullName"`
		Email        string      `json:"email"`
		PhoneE164    string      `json:"phoneE164"`
		Province     string      `json:"province"`
		District     string      `json:"district"`
		Sector       string      `json:"sector"`
		Cell         string      `json:"cell"`
		Instructions string      `json:"instructions"`
		AddressLine  string      `json:"addressLine"`
		Items        []InputItem `json:"items"`
	}{normalized.FullName, normalized.Email, normalized.PhoneE164, normalized.Province,
		normalized.District, normalized.Sector, normalized.Cell, normalized.Instructions,
		normalized.AddressLine, normalized.Items})
	if err != nil {
		return NormalizedInput{}, map[string][]string{"body": {"invalid"}}
	}
	sum := sha256.Sum256(fingerprintPayload)
	normalized.Fingerprint = sum[:]
	return normalized, nil
}
