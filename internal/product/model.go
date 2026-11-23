// Package product implements the canonical storefront product contract.
package product

import (
	"errors"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const MaxInteger int64 = 9007199254740991
const MaxImages = 6
const MaxImageBytes int64 = 5 << 20
const MaxUploadBytes int64 = 31 << 20

var ErrNotFound = errors.New("product not found")
var ErrConflict = errors.New("product version or uniqueness conflict")
var ErrValidation = errors.New("invalid product input")
var ErrUnsupportedImage = errors.New("unsupported image")
var ErrStorage = errors.New("image storage unavailable")

var Categories = []string{"phones-and-tablets", "computers", "gaming", "cameras", "audio", "wearables", "accessories"}
var Conditions = []string{"BRAND_NEW_SEALED", "BRAND_NEW_OPEN_BOX", "REFURBISHED", "USED_GRADE_A", "USED_GRADE_B", "USED_GRADE_C"}
var skuPattern = regexp.MustCompile(`^[A-Z0-9][A-Z0-9_-]{2,63}$`)
var uuidPattern = regexp.MustCompile(`^[a-fA-F0-9]{8}-[a-fA-F0-9]{4}-[a-fA-F0-9]{4}-[a-fA-F0-9]{4}-[a-fA-F0-9]{12}$`)

func ValidID(s string) bool { return uuidPattern.MatchString(s) }
func Contains(values []string, v string) bool {
	for _, s := range values {
		if s == v {
			return true
		}
	}
	return false
}

type Text struct {
	EN string  `json:"en"`
	RW *string `json:"rw"`
}
type Description struct {
	EN string `json:"en"`
	RW string `json:"rw"`
}
type Location struct {
	Country  string  `json:"country"`
	District *string `json:"district"`
	Sector   *string `json:"sector"`
}

// Input matches frontend ProductApiInput; SKU is an optional create-only addition.
type Input struct {
	SKU                string            `json:"sku,omitempty"`
	Name               Text              `json:"name"`
	Brand              *string           `json:"brand"`
	Model              *string           `json:"model"`
	Category           string            `json:"category"`
	Description        Description       `json:"description"`
	PriceRWF           int64             `json:"priceRwf"`
	ComparisonPriceRWF *int64            `json:"comparisonPriceRwf"`
	StockQuantity      int64             `json:"stockQuantity"`
	Availability       string            `json:"availability"`
	Location           Location          `json:"location"`
	LowStockThreshold  int64             `json:"lowStockThreshold"`
	Condition          string            `json:"condition"`
	Specifications     map[string]string `json:"specifications"`
}
type Photo struct {
	ID      string      `json:"id"`
	URL     string      `json:"url"`
	Alt     Description `json:"alt"`
	Primary bool        `json:"isPrimary"`
	Order   int         `json:"displayOrder"`
	// Storage identifiers and original filenames are never serialized.
	Key  string `json:"-"`
	MIME string `json:"-"`
	Size int64  `json:"-"`
}
type Product struct {
	Input
	ID          string     `json:"id"`
	Photos      []Photo    `json:"photos"`
	Published   bool       `json:"published"`
	PublishedAt *time.Time `json:"publishedAt"`
	CreatedAt   time.Time  `json:"createdAt"`
	UpdatedAt   time.Time  `json:"updatedAt"`
	Version     int64      `json:"version"`
}

// Public projects only storefront fields; publication/version/threshold are private.
func (p Product) Public() map[string]any {
	return map[string]any{"id": p.ID, "sku": p.SKU, "name": p.Name, "brand": p.Brand, "model": p.Model, "category": p.Category, "description": p.Description, "priceRwf": p.PriceRWF, "comparisonPriceRwf": p.ComparisonPriceRWF, "condition": p.Condition, "location": p.Location, "availability": p.Availability, "stockQuantity": p.StockQuantity, "specifications": p.Specifications, "photos": p.Photos, "createdAt": p.CreatedAt, "updatedAt": p.UpdatedAt}
}
func SafeText(s string, max int, required bool) bool {
	if !utf8.ValidString(s) || utf8.RuneCountInString(s) > max || (required && strings.TrimSpace(s) == "") {
		return false
	}
	for _, r := range s {
		if (unicode.IsControl(r) && r != '\n' && r != '\r' && r != '\t') || r == '\u202e' || r == '\u202d' {
			return false
		}
	}
	return true
}
func Validate(in Input) map[string][]string {
	fields := map[string][]string{}
	check := func(ok bool, key string) {
		if !ok {
			fields[key] = []string{"invalid"}
		}
	}
	check(in.SKU == "" || skuPattern.MatchString(in.SKU), "sku")
	check(SafeText(in.Name.EN, 240, true), "name.en")
	opt := func(p *string, n int) bool { return p == nil || SafeText(*p, n, false) }
	check(opt(in.Name.RW, 240), "name.rw")
	check(opt(in.Brand, 240), "brand")
	check(opt(in.Model, 240), "model")
	check(SafeText(in.Description.EN, 5000, true), "description.en")
	check(SafeText(in.Description.RW, 5000, true), "description.rw")
	check(Contains(Categories, in.Category), "category")
	check(Contains(Conditions, in.Condition), "condition")
	check(in.PriceRWF > 0 && in.PriceRWF <= MaxInteger, "priceRwf")
	check(in.ComparisonPriceRWF == nil || (*in.ComparisonPriceRWF >= in.PriceRWF && *in.ComparisonPriceRWF > 0 && *in.ComparisonPriceRWF <= MaxInteger), "comparisonPriceRwf")
	check(in.StockQuantity >= 0 && in.StockQuantity <= MaxInteger, "stockQuantity")
	check(in.LowStockThreshold >= 0 && in.LowStockThreshold <= MaxInteger, "lowStockThreshold")
	check(Contains([]string{"available", "out-of-stock", "unavailable"}, in.Availability) && !(in.Availability == "available" && in.StockQuantity == 0) && !(in.Availability == "out-of-stock" && in.StockQuantity != 0), "availability")
	check(in.Location.Country == "RW" && opt(in.Location.District, 240) && opt(in.Location.Sector, 240), "location")
	seen := map[string]bool{}
	check(len(in.Specifications) <= 19, "specifications")
	for k, v := range in.Specifications {
		normalized := strings.ToLower(strings.TrimSpace(k))
		check(SafeText(k, 80, true) && k == strings.TrimSpace(k) && SafeText(v, 240, true) && !seen[normalized] && !Contains([]string{"__proto__", "constructor", "prototype"}, normalized), "specifications")
		seen[normalized] = true
	}
	return fields
}
func SpecKeys(in Input) []string {
	keys := make([]string, 0, len(in.Specifications))
	for k := range in.Specifications {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
