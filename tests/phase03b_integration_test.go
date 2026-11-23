//go:build !js

package tests

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/jpeg"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/textproto"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"autivashop/backend/internal/admin"
	"autivashop/backend/internal/auth"
	"autivashop/backend/internal/product"
	"autivashop/backend/internal/server"
	"github.com/jackc/pgx/v5/pgconn"
)

// Use production Server.Serve over a real loopback listener. No CLI execution
// is assumed, and the test does not replace the router with a test-only router.
func TestPhase03BProductionTCPWorkflow(t *testing.T) {
	e := setupProducts(t)
	password, err := auth.GenerateToken(32)
	if err != nil {
		t.Fatal(err)
	}
	user := e.createTestAdmin(t, "tcp03b+"+time.Now().Format("150405.000000000")+"@example.test", password, "Phase03B TCP", admin.RoleSuperAdmin, true)
	cfg := testConfig(t, nil)
	srv := server.New(cfg, e.db, testLogger())
	srv.SetAdminService(e.cfgSvc)
	srv.SetProductService(e.svc)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, listener) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("production server did not stop")
		}
	})
	base := "http://" + listener.Addr().String() + "/api/v1"
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Jar: jar, Timeout: 10 * time.Second}
	anonymous := &http.Client{Timeout: 10 * time.Second}
	csrf := ""
	call := func(method, path string, payload any, version int64, authenticated bool, want int) ([]byte, http.Header) {
		t.Helper()
		var body io.Reader
		contentType := "application/json"
		switch value := payload.(type) {
		case *tcpMultipart:
			body = bytes.NewReader(value.data)
			contentType = value.contentType
		case nil:
		default:
			b, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			body = bytes.NewReader(b)
		}
		req, err := http.NewRequest(method, base+path, body)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Origin", "http://localhost:3000")
		req.Header.Set("Content-Type", contentType)
		if csrf != "" {
			req.Header.Set("X-CSRF-Token", csrf)
		}
		if version > 0 {
			req.Header.Set("If-Match", strconv.Quote(strconv.FormatInt(version, 10)))
		}
		chosen := client
		if !authenticated {
			chosen = anonymous
		}
		resp, err := chosen.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != want {
			t.Fatalf("%s %s: %d want %d body=%s", method, path, resp.StatusCode, want, b)
		}
		t.Logf("%s %s -> %d", method, path, resp.StatusCode)
		return b, resp.Header
	}
	parse := func(b []byte) product.Product {
		t.Helper()
		var p product.Product
		if err := json.Unmarshal(b, &p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	call("GET", "/health", nil, 0, false, 200)
	call("GET", "/ready", nil, 0, false, 200)
	b, _ := call("GET", "/admin/auth/me", nil, 0, true, 401)
	var bootstrap struct {
		Token string `json:"csrfToken"`
	}
	if err = json.Unmarshal(b, &bootstrap); err != nil || bootstrap.Token == "" {
		t.Fatal("missing CSRF bootstrap")
	}
	csrf = bootstrap.Token
	creds := map[string]string{"email": user.Email, "password": password}
	call("POST", "/admin/auth/login", creds, 0, false, 403)
	b, _ = call("POST", "/admin/auth/login", creds, 0, true, 200)
	if err = json.Unmarshal(b, &bootstrap); err != nil {
		t.Fatal(err)
	}
	csrf = bootstrap.Token
	call("GET", "/admin/auth/me", nil, 0, true, 200)
	call("GET", "/admin/products", nil, 0, false, 401)
	b, headers := call("POST", "/admin/products", productInput(), 0, true, 201)
	p := parse(b)
	e.cleanupProduct(t, p.ID)
	path := "/admin/products/" + p.ID
	public := "/products/" + p.ID
	if headers.Get("ETag") != `"1"` {
		t.Fatal("missing version ETag")
	}
	duplicate := productInput()
	duplicate.SKU = p.SKU
	call("POST", "/admin/products", duplicate, 0, true, 409)
	call("PATCH", path, map[string]int{"priceRwf": 0}, p.Version, true, 422)
	call("GET", public, nil, 0, false, 404)
	// Negative image cases exercise production middleware and multipart parsing.
	bad := multipartImages(t, []product.Upload{{Filename: "fake.png", MIME: "image/png", Data: []byte("not a PNG")}})
	call("POST", path+"/images", bad, p.Version, true, 415)
	big := multipartImages(t, []product.Upload{{Filename: "big.png", MIME: "image/png", Data: make([]byte, product.MaxImageBytes+1)}})
	call("POST", path+"/images", big, p.Version, true, 413)
	var jpegData bytes.Buffer
	if err = jpeg.Encode(&jpegData, image.NewNRGBA(image.Rect(0, 0, 8, 8)), nil); err != nil {
		t.Fatal(err)
	}
	uploads := multipartImages(t, []product.Upload{{Filename: "../../test.png", MIME: "image/png", Data: append(imageBytes(42), []byte("strip-this-metadata")...)}, {Filename: "test.jpg", MIME: "image/jpeg", Data: jpegData.Bytes()}})
	call("POST", path+"/images", uploads, p.Version, false, 403)
	b, _ = call("POST", path+"/images", uploads, p.Version, true, 201)
	p = parse(b)
	if len(p.Photos) != 2 {
		t.Fatal("mixed image upload failed")
	}
	pngID, jpegID := p.Photos[0].ID, p.Photos[1].ID
	call("GET", public+"/images/"+pngID, nil, 0, false, 404)
	b, headers = call("GET", path+"/images/"+pngID, nil, 0, true, 200)
	if bytes.Contains(b, []byte("strip-this-metadata")) || headers.Get("Content-Type") != "image/png" || headers.Get("Cache-Control") != "no-store" || headers.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("unsafe PNG response")
	}
	_, headers = call("GET", path+"/images/"+jpegID, nil, 0, true, 200)
	if headers.Get("Content-Type") != "image/jpeg" {
		t.Fatal("incorrect JPEG response")
	}
	b, _ = call("PATCH", path+"/images/order", map[string]any{"items": []string{jpegID, pngID}, "primaryId": pngID}, p.Version, true, 200)
	p = parse(b)
	if p.Photos[0].ID != jpegID || !p.Photos[1].Primary {
		t.Fatal("ordering/primary mismatch")
	}
	b, _ = call("PATCH", "/admin/inventory/"+p.ID, map[string]any{"stockQuantity": 8}, p.Version, true, 200)
	p = parse(b)
	call("PATCH", "/admin/inventory/"+p.ID, map[string]any{"stockQuantity": 9}, 1, true, 409)
	b, _ = call("POST", path+"/publish", nil, p.Version, true, 200)
	p = parse(b)
	b, _ = call("GET", public, nil, 0, false, 200)
	var visible map[string]any
	if err = json.Unmarshal(b, &visible); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"version", "published", "publishedAt", "lowStockThreshold", "storage_key", "original_filename"} {
		if _, ok := visible[key]; ok {
			t.Fatalf("private field %s leaked", key)
		}
	}
	if visible["stockQuantity"] != float64(8) {
		t.Fatal("inventory update not persisted")
	}
	call("GET", public+"/images/"+pngID, nil, 0, false, 200)
	b, _ = call("PATCH", path, map[string]any{"priceRwf": 100001}, p.Version, true, 200)
	p = parse(b)
	b, _ = call("GET", public, nil, 0, false, 200)
	if parse(b).PriceRWF != 100001 {
		t.Fatal("price update not persisted")
	}
	b, _ = call("POST", path+"/unpublish", nil, p.Version, true, 200)
	p = parse(b)
	call("GET", public, nil, 0, false, 404)
	call("GET", public+"/images/"+pngID, nil, 0, false, 404)
	call("DELETE", path, nil, p.Version, true, 204)
	call("GET", path, nil, 0, true, 404)
	call("GET", public, nil, 0, false, 404)
	call("GET", path+"/images/"+pngID, nil, 0, true, 404)
	// Archived images remain referenced and must never be swept as orphans.
	orphan := strings.Repeat("f", 64) + ".png"
	if err = e.storage.Put(e.ctx, orphan, imageBytes(90)); err != nil {
		t.Fatal(err)
	}
	n, err := e.svc.CleanupOrphans(e.ctx)
	if err != nil || n != 1 {
		t.Fatalf("orphan cleanup: %d %v", n, err)
	}
	keys, err := e.storage.Keys(e.ctx)
	if err != nil || len(keys) != 2 {
		t.Fatal("cleanup removed archived references")
	}
	call("POST", "/admin/auth/logout", nil, 0, true, 204)
	call("GET", "/admin/products", nil, 0, true, 401)
}

type tcpMultipart struct {
	data        []byte
	contentType string
}

func multipartImages(t *testing.T, uploads []product.Upload) *tcpMultipart {
	t.Helper()
	var b bytes.Buffer
	w := multipart.NewWriter(&b)
	for _, file := range uploads {
		headers := textproto.MIMEHeader{}
		headers.Set("Content-Disposition", `form-data; name="files"; filename="`+file.Filename+`"`)
		headers.Set("Content-Type", file.MIME)
		part, err := w.CreatePart(headers)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = part.Write(file.Data); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return &tcpMultipart{b.Bytes(), w.FormDataContentType()}
}

func TestPhase03BMissingIDsAndSortTies(t *testing.T) {
	e := setupProducts(t)
	missing := "00000000-0000-0000-0000-000000000000"
	path := "/api/v1/admin/products/" + missing
	for _, target := range []string{path, "/api/v1/products/" + missing, "/api/v1/admin/products/not-a-uuid", "/api/v1/products/not-a-uuid"} {
		e.request(t, "GET", target, nil, 0, true, 404)
	}
	e.request(t, "PATCH", path, map[string]any{"stockQuantity": 5}, 1, true, 404)
	e.request(t, "POST", path+"/publish", nil, 1, true, 404)
	e.request(t, "DELETE", path, nil, 1, true, 404)
	e.request(t, "GET", "/api/v1/admin/products/", nil, 0, true, 404)
	prefix := "03B-Tied-" + time.Now().Format("150405000")
	ids := []string{}
	for i := 0; i < 3; i++ {
		in := productInput()
		in.Name.EN = prefix
		in.PriceRWF = 100
		p := e.create(t, in)
		ids = append(ids, p.ID)
		if _, err := e.svc.State(e.ctx, p.ID, p.Version, "publish", e.user.ID); err != nil {
			t.Fatal(err)
		}
	}
	sort.Strings(ids)
	for _, order := range []string{"price-asc", "price-desc", "name"} {
		for page := 1; page <= 3; page++ {
			rec := e.request(t, "GET", "/api/v1/products?search="+prefix+"&sort="+order+"&limit=1&page="+strconv.Itoa(page), nil, 0, false, 200)
			var list struct {
				Items []product.Product `json:"items"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || len(list.Items) != 1 || list.Items[0].ID != ids[page-1] {
				t.Fatalf("unstable %s page %d", order, page)
			}
		}
	}
	// Unsupported roles cannot enter the valid administrator data set. Both
	// existing admin roles are product operators; no new read-only role is invented.
	_, err := e.db.Exec(e.ctx, `UPDATE admin_users SET role='viewer' WHERE id=$1`, e.user.ID)
	var pg *pgconn.PgError
	if !errors.As(err, &pg) || pg.Code != "23514" {
		t.Fatalf("unsupported role admitted: %v", err)
	}
}

func TestPhase03BExplicitInventoryRowLock(t *testing.T) {
	e := setupProducts(t)
	p := e.create(t, productInput())
	tx, err := e.db.Pool().Begin(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err = tx.Exec(e.ctx, `SELECT id FROM products WHERE id=$1 FOR UPDATE`, p.ID); err != nil {
		t.Fatal(err)
	}
	blocked, cancel := context.WithTimeout(e.ctx, 200*time.Millisecond)
	defer cancel()
	_, err = e.svc.Patch(blocked, p.ID, p.Version, map[string]json.RawMessage{"stockQuantity": json.RawMessage(`6`)}, e.user.ID)
	if err == nil || blocked.Err() != context.DeadlineExceeded {
		t.Fatalf("inventory mutation did not wait for row lock: %v", err)
	}
	if err = tx.Rollback(e.ctx); err != nil {
		t.Fatal(err)
	}
	fresh, err := e.svc.Get(e.ctx, p.ID, false)
	if err != nil || fresh.Version != p.Version || fresh.StockQuantity != p.StockQuantity {
		t.Fatal("cancelled blocked edit changed inventory")
	}
	fresh, err = e.svc.Patch(e.ctx, p.ID, p.Version, map[string]json.RawMessage{"stockQuantity": json.RawMessage(`6`)}, e.user.ID)
	if err != nil || fresh.StockQuantity != 6 || fresh.Version != p.Version+1 {
		t.Fatalf("post-lock mutation failed: %v", err)
	}
}
