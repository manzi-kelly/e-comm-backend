//go:build !js

package tests

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"autivashop/backend/internal/admin"
	"autivashop/backend/internal/product"
	"autivashop/backend/internal/server"
	"github.com/jackc/pgx/v5/pgconn"
)

type productEnv struct {
	*adminAuthEnv
	svc           *product.Service
	storage       *product.LocalStorage
	handler       http.Handler
	user          admin.User
	session, csrf *http.Cookie
}

func setupProducts(t *testing.T) *productEnv {
	e := setupAdminAuthEnv(t)
	storage, err := product.NewLocalStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	svc := product.NewService(e.db.Pool(), storage)
	cfg := testConfig(t, nil)
	srv := server.New(cfg, e.db, testLogger())
	srv.SetAdminService(e.cfgSvc)
	srv.SetProductService(svc)
	pass := "Isolated-Product-Test-Password!"
	user := e.createTestAdmin(t, "products+"+time.Now().Format("150405.000000000")+"@example.test", pass, "Product tester", admin.RoleAdmin, true)
	h := srv.Handler()
	csrf, cookies := getCSRFToken(t, h)
	rec := loginReq(t, h, user.Email, pass, csrf, cookies)
	if rec.Code != 200 {
		t.Fatalf("login %d", rec.Code)
	}
	pe := &productEnv{adminAuthEnv: e, svc: svc, storage: storage, handler: h, user: user}
	for _, c := range rec.Result().Cookies() {
		if c.Name == e.cookies.SessionName {
			pe.session = c
		}
		if c.Name == e.cookies.CSRFName {
			pe.csrf = c
		}
	}
	return pe
}
func (e *productEnv) request(t *testing.T, method, path string, body any, v int64, auth bool, want int) *httptest.ResponseRecorder {
	t.Helper()
	var b []byte
	switch value := body.(type) {
	case string:
		b = []byte(value)
	case nil:
	default:
		var err error
		b, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://localhost:3000")
	if v > 0 {
		req.Header.Set("If-Match", strconv.Quote(strconv.FormatInt(v, 10)))
	}
	if auth {
		req.AddCookie(e.session)
		req.AddCookie(e.csrf)
		req.Header.Set("X-CSRF-Token", e.csrf.Value)
	}
	rec := doReq(e.handler, req)
	if rec.Code != want {
		t.Fatalf("%s %s: got %d want %d body=%s", method, path, rec.Code, want, rec.Body.String())
	}
	return rec
}
func decodeProduct(t *testing.T, rec *httptest.ResponseRecorder) product.Product {
	t.Helper()
	var p product.Product
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	return p
}
func (e *productEnv) cleanupProduct(t *testing.T, id string) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for _, table := range []string{"product_images", "product_specifications", "inventory"} {
			_, _ = e.db.Exec(ctx, "DELETE FROM "+table+" WHERE product_id=$1", id)
		}
		_, _ = e.db.Exec(ctx, `DELETE FROM products WHERE id=$1`, id)
	})
}
func (e *productEnv) create(t *testing.T, in product.Input) product.Product {
	t.Helper()
	p := decodeProduct(t, e.request(t, "POST", "/api/v1/admin/products", in, 0, true, 201))
	e.cleanupProduct(t, p.ID)
	return p
}

func TestProductsIntegrationLifecycleAndBoundaries(t *testing.T) {
	e := setupProducts(t)
	p := e.create(t, productInput())
	path := "/api/v1/admin/products/" + p.ID
	public := "/api/v1/products/" + p.ID
	if p.Published || p.SKU == "" || p.Version != 1 || p.Specifications["ram"] != "8 GB" {
		t.Fatal("create contract")
	}
	e.request(t, "GET", path, nil, 0, false, 401)
	e.request(t, "GET", public, nil, 0, false, 404)
	e.request(t, "POST", "/api/v1/admin/products", productInput(), 0, false, 403)
	e.request(t, "POST", "/api/v1/admin/products", `{"name":1,"name":2}`, 0, true, 422)
	duplicate := productInput()
	duplicate.SKU = p.SKU
	e.request(t, "POST", "/api/v1/admin/products", duplicate, 0, true, 409)
	e.request(t, "PATCH", path, map[string]any{"priceRwf": 120000}, 0, true, 400)
	p = decodeProduct(t, e.request(t, "PATCH", path, map[string]any{"priceRwf": 120000, "specifications": map[string]string{"ram": "16 GB"}}, p.Version, true, 200))
	if p.PriceRWF != 120000 || p.Specifications["ram"] != "16 GB" {
		t.Fatal("patch not persisted")
	}
	e.request(t, "GET", "/api/v1/admin/inventory", nil, 0, true, 200)
	e.request(t, "PATCH", "/api/v1/admin/inventory/"+p.ID, map[string]any{"priceRwf": 1}, p.Version, true, 422)
	p = decodeProduct(t, e.request(t, "PATCH", "/api/v1/admin/inventory/"+p.ID, map[string]any{"stockQuantity": 4}, p.Version, true, 200))
	e.request(t, "PATCH", path, map[string]any{"priceRwf": 1}, 1, true, 409)
	for _, patch := range []map[string]any{{"priceRwf": 0}, {"stockQuantity": -1}, {"condition": ""}, {"category": "cars"}, {"sku": "NEW-SKU"}, {"published": true}, {"stockQuantity": nil}, {"comparisonPriceRwf": 1}} {
		e.request(t, "PATCH", path, patch, p.Version, true, 422)
	}
	p = decodeProduct(t, e.request(t, "POST", path+"/publish", nil, p.Version, true, 200))
	rec := e.request(t, "GET", public, nil, 0, false, 200)
	for _, private := range []string{"lowStockThreshold", "version", "published", "storage_key", "original_filename", "password"} {
		if strings.Contains(rec.Body.String(), `"`+private+`"`) {
			t.Fatalf("public leak %s", private)
		}
	}
	e.request(t, "PATCH", public, map[string]any{"stockQuantity": 10}, p.Version, false, 405)
	p = decodeProduct(t, e.request(t, "PATCH", path, map[string]any{"stockQuantity": 0, "availability": "out-of-stock"}, p.Version, true, 200))
	e.request(t, "GET", public, nil, 0, false, 200)
	p = decodeProduct(t, e.request(t, "POST", path+"/unpublish", nil, p.Version, true, 200))
	e.request(t, "GET", public, nil, 0, false, 404)
	p = decodeProduct(t, e.request(t, "POST", path+"/publish", nil, p.Version, true, 200))
	e.request(t, "DELETE", path, nil, p.Version, true, 204)
	e.request(t, "GET", public, nil, 0, false, 404)
	e.request(t, "GET", path, nil, 0, true, 404)
	var archived bool
	if err := e.db.Pool().QueryRow(e.ctx, `SELECT deleted_at IS NOT NULL AND NOT published FROM products WHERE id=$1`, p.ID).Scan(&archived); err != nil || !archived {
		t.Fatal("product identity not preserved")
	}
	var count int
	if err := e.db.Pool().QueryRow(e.ctx, `SELECT count(*) FROM admin_audit_events WHERE metadata->>'product_id'=$1`, p.ID).Scan(&count); err != nil || count < 6 {
		t.Fatal("missing product audit trail")
	}
}

func TestProductsIntegrationSearchPaginationAndRollback(t *testing.T) {
	e := setupProducts(t)
	prefix := "Phase03Search" + time.Now().Format("150405000")
	for i := 0; i < 3; i++ {
		in := productInput()
		in.Name.EN = fmt.Sprintf("%s %d", prefix, i)
		in.PriceRWF = int64(100 + i)
		p := e.create(t, in)
		if _, err := e.svc.State(e.ctx, p.ID, p.Version, "publish", e.user.ID); err != nil {
			t.Fatal(err)
		}
	}
	rec := e.request(t, "GET", "/api/v1/products?search="+prefix+"&category=phones-and-tablets&availability=available&minPriceRwf=100&maxPriceRwf=102&sort=price-asc&limit=2", nil, 0, false, 200)
	var list struct {
		Items []product.Product `json:"items"`
		More  bool              `json:"hasNext"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || len(list.Items) != 2 || !list.More || list.Items[0].PriceRWF != 100 {
		t.Fatal("search/filter/sort/page")
	}
	rec = e.request(t, "GET", "/api/v1/products?search="+prefix+"&sort=price-asc&limit=2&page=2", nil, 0, false, 200)
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	if len(list.Items) != 1 || list.More || list.Items[0].PriceRWF != 102 {
		t.Fatal("second page")
	}
	for _, query := range []string{"limit=101", "page=0", "page=1001", "sort=bad", "category=bad", "search=x&search=y", "minPriceRwf=3&maxPriceRwf=2", "unknown=1"} {
		e.request(t, "GET", "/api/v1/products?"+query, nil, 0, false, 422)
	}
	e.request(t, "GET", "/api/v1/products?search=%27%20OR%201%3D1--", nil, 0, false, 200)
	in := productInput()
	in.SKU = "ROLLBACK-" + time.Now().Format("150405000")
	if _, err := e.svc.Create(e.ctx, in, "00000000-0000-0000-0000-000000000000"); err == nil {
		t.Fatal("audit FK should abort create")
	}
	var count int
	if err := e.db.Pool().QueryRow(e.ctx, `SELECT count(*) FROM products WHERE sku=$1`, in.SKU).Scan(&count); err != nil || count != 0 {
		t.Fatal("partial product after rollback")
	}
}

func TestProductsIntegrationConcurrentStockAndAuth(t *testing.T) {
	e := setupProducts(t)
	p := e.create(t, productInput())
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, stock := range []string{"8", "9"} {
		wg.Add(1)
		go func(value string) {
			defer wg.Done()
			_, err := e.svc.Patch(context.Background(), p.ID, p.Version, map[string]json.RawMessage{"stockQuantity": json.RawMessage(value)}, e.user.ID)
			results <- err
		}(stock)
	}
	wg.Wait()
	close(results)
	success, conflicts := 0, 0
	for err := range results {
		if err == nil {
			success++
		} else if errors.Is(err, product.ErrConflict) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || conflicts != 1 {
		t.Fatal("lost-update protection failed")
	}
	path := "/api/v1/admin/products/" + p.ID
	_, err := e.db.Exec(e.ctx, `UPDATE admin_users SET is_active=false WHERE id=$1`, e.user.ID)
	if err != nil {
		t.Fatal(err)
	}
	e.request(t, "PATCH", path, map[string]any{"priceRwf": 10}, 2, true, 401)
	_, _ = e.db.Exec(e.ctx, `UPDATE admin_users SET is_active=true WHERE id=$1`, e.user.ID)
	_, err = e.db.Exec(e.ctx, `UPDATE admin_sessions SET created_at=now()-interval '2 hours',expires_at=now()-interval '1 hour' WHERE admin_user_id=$1`, e.user.ID)
	if err != nil {
		t.Fatal(err)
	}
	e.request(t, "GET", path, nil, 0, true, 401)
}

func (e *productEnv) upload(t *testing.T, p product.Product, files []product.Upload, auth bool, want int) *httptest.ResponseRecorder {
	t.Helper()
	var b bytes.Buffer
	writer := multipart.NewWriter(&b)
	for _, file := range files {
		hdr := textproto.MIMEHeader{}
		hdr.Set("Content-Disposition", fmt.Sprintf(`form-data; name="files"; filename="%s"`, file.Filename))
		hdr.Set("Content-Type", file.MIME)
		part, err := writer.CreatePart(hdr)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = part.Write(file.Data)
	}
	_ = writer.Close()
	r := httptest.NewRequest("POST", "/api/v1/admin/products/"+p.ID+"/images", &b)
	r.Header.Set("Content-Type", writer.FormDataContentType())
	r.Header.Set("Origin", "http://localhost:3000")
	r.Header.Set("If-Match", strconv.Quote(strconv.FormatInt(p.Version, 10)))
	if auth {
		r.AddCookie(e.session)
		r.AddCookie(e.csrf)
		r.Header.Set("X-CSRF-Token", e.csrf.Value)
	}
	rec := doReq(e.handler, r)
	if rec.Code != want {
		t.Fatalf("upload %d want %d: %s", rec.Code, want, rec.Body.String())
	}
	return rec
}
func TestProductsIntegrationImages(t *testing.T) {
	e := setupProducts(t)
	p := e.create(t, productInput())
	image1 := product.Upload{Filename: "../../test.png", MIME: "image/png", Data: append(imageBytes(1), []byte("private metadata")...)}
	image2 := product.Upload{Filename: "second.png", MIME: "image/png", Data: imageBytes(2)}
	e.upload(t, p, []product.Upload{image1}, false, 403)
	for _, bad := range []product.Upload{{Filename: "bad.svg", MIME: "image/svg+xml", Data: []byte(`<svg onload="alert(1)"/>`)}, {Filename: "bad.png", MIME: "image/png", Data: []byte("not an image")}, {Filename: "bad.jpg", MIME: "image/jpeg", Data: imageBytes(1)}, {Filename: "empty.png", MIME: "image/png"}, {Filename: "large.png", MIME: "image/png", Data: make([]byte, product.MaxImageBytes+1)}} {
		want := 415
		if len(bad.Data) == 0 {
			want = 422
		}
		if int64(len(bad.Data)) > product.MaxImageBytes {
			want = 413
		}
		e.upload(t, p, []product.Upload{bad}, true, want)
	}
	p = decodeProduct(t, e.upload(t, p, []product.Upload{image1, image2}, true, 201))
	if len(p.Photos) != 2 || !p.Photos[0].Primary {
		t.Fatal("initial primary")
	}
	first, second := p.Photos[0].ID, p.Photos[1].ID
	public := "/api/v1/products/" + p.ID + "/images/" + first
	adminPath := "/api/v1/admin/products/" + p.ID + "/images/" + first
	e.request(t, "GET", public, nil, 0, false, 404)
	rec := e.request(t, "GET", adminPath, nil, 0, true, 200)
	if strings.Contains(rec.Body.String(), "private metadata") {
		t.Fatal("image metadata retained")
	}
	if rec.Header().Get("Content-Type") != "image/png" || rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("unsafe image response")
	}
	before, _ := e.storage.Keys(e.ctx)
	e.upload(t, p, []product.Upload{image2}, true, 409)
	after, _ := e.storage.Keys(e.ctx)
	if len(before) != len(after) {
		t.Fatal("orphan after duplicate rollback")
	}
	orderPath := "/api/v1/admin/products/" + p.ID + "/images/order"
	e.request(t, "PATCH", orderPath, map[string]any{"items": []string{first, first}, "primaryId": first}, p.Version, true, 422)
	p = decodeProduct(t, e.request(t, "PATCH", orderPath, map[string]any{"items": []string{second, first}, "primaryId": first}, p.Version, true, 200))
	if p.Photos[0].ID != second || !p.Photos[1].Primary {
		t.Fatal("image order and primary")
	}
	p = decodeProduct(t, e.request(t, "POST", "/api/v1/admin/products/"+p.ID+"/publish", nil, p.Version, true, 200))
	e.request(t, "GET", public, nil, 0, false, 200)
	p = decodeProduct(t, e.request(t, "POST", "/api/v1/admin/products/"+p.ID+"/unpublish", nil, p.Version, true, 200))
	e.request(t, "GET", public, nil, 0, false, 404)
	p = decodeProduct(t, e.request(t, "DELETE", adminPath, nil, p.Version, true, 200))
	if len(p.Photos) != 1 || !p.Photos[0].Primary {
		t.Fatal("primary fallback")
	}
	keys, _ := e.storage.Keys(e.ctx)
	if len(keys) != 1 {
		t.Fatal("image removal did not delete file")
	}
	// Fill to the limit, then reject the seventh image without changing metadata.
	files := []product.Upload{}
	for seed := uint8(3); seed <= 7; seed++ {
		files = append(files, product.Upload{Filename: "another.png", MIME: "image/png", Data: imageBytes(seed)})
	}
	p = decodeProduct(t, e.upload(t, p, files, true, 201))
	if len(p.Photos) != 6 {
		t.Fatal("six image support")
	}
	e.upload(t, p, []product.Upload{{Filename: "extra.png", MIME: "image/png", Data: imageBytes(8)}}, true, 422)
	// Choosing a primary is independent of display order. Removing a different
	// image must not move that primary to the front.
	ids := []string{}
	for _, photo := range p.Photos {
		ids = append(ids, photo.ID)
	}
	p = decodeProduct(t, e.request(t, "PATCH", orderPath, map[string]any{"items": ids, "primaryId": ids[len(ids)-1]}, p.Version, true, 200))
	p = decodeProduct(t, e.request(t, "DELETE", "/api/v1/admin/products/"+p.ID+"/images/"+ids[1], nil, p.Version, true, 200))
	if p.Photos[0].ID != ids[0] || !p.Photos[len(p.Photos)-1].Primary {
		t.Fatal("non-primary removal changed display order")
	}
	other := e.create(t, productInput())
	e.request(t, "GET", "/api/v1/admin/products/"+other.ID+"/images/"+p.Photos[0].ID, nil, 0, true, 404)
	orphan := strings.Repeat("e", 64) + ".png"
	if err := e.storage.Put(e.ctx, orphan, imageBytes(9)); err != nil {
		t.Fatal(err)
	}
	n, err := e.svc.CleanupOrphans(e.ctx)
	if err != nil || n != 1 {
		t.Fatalf("cleanup %d %v", n, err)
	}
	keys, _ = e.storage.Keys(e.ctx)
	if len(keys) != 5 {
		t.Fatal("cleanup removed referenced image")
	}
	// DB must enforce one primary even if application validation is bypassed.
	_, err = e.db.Exec(e.ctx, `UPDATE product_images SET is_primary=true WHERE product_id=$1`, p.ID)
	var pg *pgconn.PgError
	if !errors.As(err, &pg) || pg.Code != "23505" {
		t.Fatalf("primary uniqueness %v", err)
	}
}

func TestProductsIntegrationRealHTTP(t *testing.T) {
	e := setupProducts(t)
	tcp := httptest.NewServer(e.handler)
	defer tcp.Close()
	in := productInput()
	b, _ := json.Marshal(in)
	r, _ := http.NewRequest("POST", tcp.URL+"/api/v1/admin/products", bytes.NewReader(b))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", "http://localhost:3000")
	r.Header.Set("X-CSRF-Token", e.csrf.Value)
	r.AddCookie(e.session)
	r.AddCookie(e.csrf)
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 201 {
		data, _ := io.ReadAll(resp.Body)
		t.Fatalf("real HTTP create: %d %s", resp.StatusCode, data)
	}
	var p product.Product
	if err = json.NewDecoder(resp.Body).Decode(&p); err != nil {
		t.Fatal(err)
	}
	e.cleanupProduct(t, p.ID)
}

func TestProductsIntegrationCriticalConstraintsAndImageRollback(t *testing.T) {
	e := setupProducts(t)
	p := e.create(t, productInput())
	for _, sql := range []string{
		`UPDATE products SET price_rwf=0 WHERE id=$1`,
		`UPDATE products SET comparison_price_rwf=1 WHERE id=$1`,
		`UPDATE products SET condition='' WHERE id=$1`,
		`UPDATE products SET category='cars' WHERE id=$1`,
		`UPDATE products SET country='US' WHERE id=$1`,
		`UPDATE inventory SET stock_quantity=-1 WHERE product_id=$1`,
		`UPDATE inventory SET low_stock_threshold=-1 WHERE product_id=$1`,
		`UPDATE inventory SET stock_quantity=0 WHERE product_id=$1`,
		`DELETE FROM products WHERE id=$1`,
	} {
		_, err := e.db.Exec(e.ctx, sql, p.ID)
		var pg *pgconn.PgError
		if !errors.As(err, &pg) || (pg.Code != "23514" && pg.Code != "23503" && pg.Code != "23001") {
			t.Fatalf("constraint not enforced: %v", err)
		}
	}
	// Invalid actor causes audit failure after files and metadata were written.
	// Entire DB transaction and newly written files must be rolled back.
	_, err := e.svc.Upload(e.ctx, p.ID, p.Version, []product.Upload{{Filename: "rollback.png", MIME: "image/png", Data: imageBytes(4)}}, "00000000-0000-0000-0000-000000000000")
	if err == nil {
		t.Fatal("expected audit FK failure")
	}
	keys, err := e.storage.Keys(e.ctx)
	if err != nil || len(keys) != 0 {
		t.Fatal("files remain after audit rollback")
	}
	fresh, err := e.svc.Get(e.ctx, p.ID, false)
	if err != nil || len(fresh.Photos) != 0 || fresh.Version != p.Version {
		t.Fatal("partial image transaction")
	}
	// Missing authentication and CSRF remain independent boundaries.
	req := httptest.NewRequest("POST", "/api/v1/admin/products/"+p.ID+"/publish", nil)
	req.AddCookie(e.session)
	req.AddCookie(e.csrf)
	req.Header.Set("Origin", "http://localhost:3000")
	req.Header.Set("If-Match", `"1"`)
	if rec := doReq(e.handler, req); rec.Code != 403 {
		t.Fatalf("mutation without CSRF: %d", rec.Code)
	}
	req.Header.Set("X-CSRF-Token", e.csrf.Value)
	req.Header.Set("Origin", "https://evil.example")
	if rec := doReq(e.handler, req); rec.Code != 403 {
		t.Fatalf("hostile mutation origin: %d", rec.Code)
	}
	req.Header.Set("Origin", "http://localhost:3000")
	req.Header.Set("Cookie", e.csrf.String()+"; "+e.cookies.SessionName+"=invalid")
	if rec := doReq(e.handler, req); rec.Code != 401 {
		t.Fatalf("invalid session: %d", rec.Code)
	}
}
