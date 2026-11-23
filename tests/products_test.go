package tests

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"autivashop/backend/internal/admin"
	"autivashop/backend/internal/product"
	"autivashop/backend/internal/server"
)

func TestProductRouterRegistration(t *testing.T) {
	cfg := testConfig(t, nil)
	srv := server.New(cfg, stubPinger{}, testLogger())
	srv.SetAdminService(admin.NewService(nil, cfg, testLogger()))
	srv.SetProductService(product.NewService(nil, nil))
	for _, tc := range []struct {
		method, path string
		want         int
	}{{"POST", "/api/v1/products", 405}, {"PATCH", "/api/v1/products/00000000-0000-0000-0000-000000000000", 405}, {"PATCH", "/api/v1/admin/products/00000000-0000-0000-0000-000000000000/images/order", 403}} {
		rec := doReq(srv.Handler(), httptest.NewRequest(tc.method, tc.path, nil))
		if rec.Code != tc.want {
			t.Fatalf("%s %s -> %d", tc.method, tc.path, rec.Code)
		}
	}
}

func productInput() product.Input {
	return product.Input{Name: product.Text{EN: "Isolated test phone"}, Description: product.Description{EN: "English test description", RW: "Ibisobanuro"}, Category: "phones-and-tablets", PriceRWF: 125000, StockQuantity: 3, Availability: "available", Location: product.Location{Country: "RW"}, LowStockThreshold: 2, Condition: "USED_GRADE_A", Specifications: map[string]string{"ram": "8 GB"}}
}
func imageBytes(seed uint8) []byte {
	im := image.NewNRGBA(image.Rect(0, 0, 8, 8))
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			im.Set(x, y, color.NRGBA{R: seed, G: uint8(x), B: uint8(y), A: 255})
		}
	}
	var b bytes.Buffer
	_ = png.Encode(&b, im)
	return b.Bytes()
}

func TestProductValidation(t *testing.T) {
	if fields := product.Validate(productInput()); len(fields) != 0 {
		t.Fatal(fields)
	}
	for _, tc := range []struct {
		name   string
		change func(*product.Input)
	}{
		{"missing condition", func(p *product.Input) { p.Condition = "" }}, {"unapproved category", func(p *product.Input) { p.Category = "cars" }},
		{"negative stock", func(p *product.Input) { p.StockQuantity = -1 }}, {"negative threshold", func(p *product.Input) { p.LowStockThreshold = -1 }},
		{"zero price", func(p *product.Input) { p.PriceRWF = 0 }}, {"unsafe price", func(p *product.Input) { p.PriceRWF = product.MaxInteger + 1 }},
		{"comparison below price", func(p *product.Input) { v := int64(1); p.ComparisonPriceRWF = &v }}, {"foreign location", func(p *product.Input) { p.Location.Country = "US" }},
		{"available without stock", func(p *product.Input) { p.StockQuantity = 0 }}, {"out of stock with stock", func(p *product.Input) { p.Availability = "out-of-stock" }},
		{"blank description", func(p *product.Input) { p.Description.RW = " " }}, {"long name", func(p *product.Input) { p.Name.EN = strings.Repeat("x", 241) }},
		{"duplicate specification", func(p *product.Input) { p.Specifications["RAM"] = "16 GB" }}, {"dangerous key", func(p *product.Input) { p.Specifications["__proto__"] = "bad" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := productInput()
			tc.change(&p)
			if len(product.Validate(p)) == 0 {
				t.Fatal("accepted invalid product")
			}
		})
	}
}
func TestProductStrictJSONAndPatch(t *testing.T) {
	valid, _ := json.Marshal(productInput())
	if _, err := product.DecodeInput(valid); err != nil {
		t.Fatal(err)
	}
	for _, data := range []string{`null`, `[]`, `{"name":1,"name":2}`, string(valid) + ` {}`, strings.Replace(string(valid), `"stockQuantity":3`, `"stockQuantity":null`, 1), strings.Replace(string(valid), `"priceRwf":125000`, `"priceRwf":1.2`, 1), strings.Replace(string(valid), `"priceRwf":125000`, `"priceRwf":1e3`, 1)} {
		if _, err := product.DecodeInput([]byte(data)); err == nil {
			t.Fatal("accepted malformed input")
		}
	}
	for _, patch := range []map[string]json.RawMessage{{"sku": json.RawMessage(`"OTHER"`)}, {"stockQuantity": json.RawMessage(`null`)}, {"published": json.RawMessage(`true`)}, {"priceRwf": json.RawMessage(`0`)}, {"name": json.RawMessage(`{"en":"New","unknown":1}`)}} {
		if _, err := product.Merge(productInput(), patch); err == nil {
			t.Fatal("accepted invalid patch")
		}
	}
	p, err := product.Merge(productInput(), map[string]json.RawMessage{"stockQuantity": json.RawMessage(`4`)})
	if err != nil || p.StockQuantity != 4 {
		t.Fatal(err)
	}
	for _, q := range []product.Query{{Page: 0, Limit: 20, Sort: "newest"}, {Page: 1, Limit: 101, Sort: "newest"}, {Page: 1001, Limit: 20, Sort: "newest"}, {Page: 1, Limit: 20, Sort: "DROP TABLE products"}} {
		if q.Validate() {
			t.Fatal("invalid query accepted")
		}
	}
}
func TestProductLocalStorageBoundaries(t *testing.T) {
	storage, err := product.NewLocalStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	ctx := context.Background()
	key := strings.Repeat("a", 64) + ".png"
	data := imageBytes(1)
	if err = storage.Put(ctx, key, data); err != nil {
		t.Fatal(err)
	}
	if err = storage.Put(ctx, key, data); err == nil {
		t.Fatal("overwrote existing file")
	}
	for _, key := range []string{"../outside.png", "C:\\outside.png", "/absolute.png", "proof.png", ""} {
		if err = storage.Put(ctx, key, data); err == nil {
			t.Fatal("unsafe key accepted")
		}
		if _, err = storage.Open(ctx, key); err == nil {
			t.Fatal("unsafe key opened")
		}
		if err = storage.Delete(ctx, key); err == nil {
			t.Fatal("unsafe key deleted")
		}
	}
	f, err := storage.Open(ctx, strings.Repeat("a", 64)+".png")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(f)
	_ = f.Close()
	if !bytes.Equal(b, data) {
		t.Fatal("storage changed bytes")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := storage.Put(cancelled, strings.Repeat("b", 64)+".png", data); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
