package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"autivashop/backend/internal/auth"
	"autivashop/backend/internal/middleware"
	"autivashop/backend/internal/product"
)

type productHandlers struct{ svc *product.Service }

func (h *productHandlers) fail(w http.ResponseWriter, r *http.Request, err error) {
	status, code, message := 500, middleware.CodeInternalError, "An internal error occurred."
	switch {
	case errors.Is(err, product.ErrNotFound):
		status, code, message = 404, middleware.CodeNotFound, "Product or image not found."
	case errors.Is(err, product.ErrConflict):
		status, code, message = 409, middleware.CodeConflict, "The product changed or a unique value already exists. Reload and retry."
	case errors.Is(err, product.ErrValidation):
		status, code, message = 422, middleware.CodeValidationFailed, "Product input failed validation."
	case errors.Is(err, product.ErrUnsupportedImage):
		status, code, message = 415, middleware.CodeUnsupportedMediaType, "Use a valid JPEG or PNG image."
	case errors.Is(err, product.ErrStorage):
		status, code, message = 503, middleware.CodeStorageUnavailable, "Product image storage is unavailable."
	}
	middleware.WriteError(w, r, status, code, message, nil)
}
func actor(r *http.Request) string { p, _ := auth.PrincipalFromContext(r.Context()); return p.ID }
func version(w http.ResponseWriter, r *http.Request) (int64, bool) {
	raw := r.Header.Get("If-Match")
	if len(raw) < 3 || raw[0] != '"' || raw[len(raw)-1] != '"' {
		middleware.WriteError(w, r, 400, middleware.CodeBadRequest, "If-Match must contain the quoted product version.", nil)
		return 0, false
	}
	n, err := strconv.ParseInt(raw[1:len(raw)-1], 10, 64)
	if err != nil || n < 1 {
		middleware.WriteError(w, r, 400, middleware.CodeBadRequest, "Invalid product version.", nil)
		return 0, false
	}
	return n, true
}
func sendProduct(w http.ResponseWriter, status int, p product.Product) {
	w.Header().Set("ETag", strconv.Quote(strconv.FormatInt(p.Version, 10)))
	middleware.WriteJSON(w, status, p)
}
func jsonBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		middleware.WriteError(w, r, 415, middleware.CodeUnsupportedMediaType, "Use application/json.", nil)
		return nil, false
	}
	b, err := io.ReadAll(r.Body)
	if err != nil {
		middleware.WriteError(w, r, 400, middleware.CodeBadRequest, "Invalid request body.", nil)
		return nil, false
	}
	return b, true
}
func emptyBody(w http.ResponseWriter, r *http.Request) bool {
	b, err := io.ReadAll(r.Body)
	if err != nil || len(bytes.TrimSpace(b)) != 0 {
		middleware.WriteError(w, r, 400, middleware.CodeBadRequest, "This endpoint expects an empty body.", nil)
		return false
	}
	return true
}
func (h *productHandlers) create(w http.ResponseWriter, r *http.Request) {
	b, ok := jsonBody(w, r)
	if !ok {
		return
	}
	in, err := product.DecodeInput(b)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if fields := product.Validate(in); len(fields) != 0 {
		middleware.WriteError(w, r, 422, middleware.CodeValidationFailed, "Product input failed validation.", fields)
		return
	}
	p, err := h.svc.Create(r.Context(), in, actor(r))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	w.Header().Set("Location", "/api/v1/admin/products/"+p.ID)
	sendProduct(w, 201, p)
}
func (h *productHandlers) patch(w http.ResponseWriter, r *http.Request) {
	h.patchFields(w, r, false)
}
func (h *productHandlers) patchInventory(w http.ResponseWriter, r *http.Request) {
	h.patchFields(w, r, true)
}
func (h *productHandlers) patchFields(w http.ResponseWriter, r *http.Request, inventoryOnly bool) {
	v, ok := version(w, r)
	if !ok {
		return
	}
	b, ok := jsonBody(w, r)
	if !ok {
		return
	}
	var patch map[string]json.RawMessage
	if err := product.StrictJSON(b, &patch); err != nil {
		h.fail(w, r, err)
		return
	}
	if inventoryOnly {
		for key := range patch {
			if !product.Contains([]string{"stockQuantity", "availability", "lowStockThreshold"}, key) {
				h.fail(w, r, product.ErrValidation)
				return
			}
		}
	}
	p, err := h.svc.Patch(r.Context(), r.PathValue("id"), v, patch, actor(r))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	sendProduct(w, 200, p)
}
func (h *productHandlers) state(action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		v, ok := version(w, r)
		if !ok || !emptyBody(w, r) {
			return
		}
		p, err := h.svc.State(r.Context(), r.PathValue("id"), v, action, actor(r))
		if err != nil {
			h.fail(w, r, err)
			return
		}
		if action == "delete" {
			w.WriteHeader(204)
			return
		}
		sendProduct(w, 200, p)
	}
}
func (h *productHandlers) get(public bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, err := h.svc.Get(r.Context(), r.PathValue("id"), public)
		if err != nil {
			h.fail(w, r, err)
			return
		}
		if public {
			middleware.WriteJSON(w, 200, p.Public())
		} else {
			sendProduct(w, 200, p)
		}
	}
}
func parseQuery(r *http.Request) (product.Query, error) {
	q := product.Query{Page: 1, Limit: 20, Sort: "newest"}
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return q, product.ErrValidation
	}
	for k, v := range values {
		if len(v) != 1 || !product.Contains([]string{"page", "limit", "search", "category", "availability", "minPriceRwf", "maxPriceRwf", "sort"}, k) {
			return q, product.ErrValidation
		}
	}
	for k, dst := range map[string]*int{"page": &q.Page, "limit": &q.Limit} {
		if s := values.Get(k); s != "" {
			n, err := strconv.Atoi(s)
			if err != nil {
				return q, product.ErrValidation
			}
			*dst = n
		}
	}
	q.Search = values.Get("search")
	q.Category = values.Get("category")
	q.Availability = values.Get("availability")
	if s := values.Get("sort"); s != "" {
		q.Sort = s
	}
	for k, dst := range map[string]**int64{"minPriceRwf": &q.MinPrice, "maxPriceRwf": &q.MaxPrice} {
		if s := values.Get(k); s != "" {
			n, err := strconv.ParseInt(s, 10, 64)
			if err != nil {
				return q, product.ErrValidation
			}
			*dst = &n
		}
	}
	if !q.Validate() {
		return q, product.ErrValidation
	}
	return q, nil
}
func (h *productHandlers) list(public bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q, err := parseQuery(r)
		if err != nil {
			h.fail(w, r, err)
			return
		}
		items, more, err := h.svc.List(r.Context(), q, public)
		if err != nil {
			h.fail(w, r, err)
			return
		}
		var result any = items
		if public {
			safe := make([]map[string]any, 0, len(items))
			for _, p := range items {
				safe = append(safe, p.Public())
			}
			result = safe
		}
		middleware.WriteJSON(w, 200, map[string]any{"items": result, "page": q.Page, "limit": q.Limit, "hasNext": more})
	}
}
func (h *productHandlers) upload(w http.ResponseWriter, r *http.Request) {
	v, ok := version(w, r)
	if !ok {
		return
	}
	reader, err := r.MultipartReader()
	if err != nil {
		middleware.WriteError(w, r, 415, middleware.CodeUnsupportedMediaType, "Use multipart/form-data with files.", nil)
		return
	}
	uploads := []product.Upload{}
	var alts []product.Description
	hasAlts := false
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				middleware.WriteError(w, r, 413, middleware.CodePayloadTooLarge, "Upload is too large.", nil)
			} else {
				h.fail(w, r, product.ErrValidation)
			}
			return
		}
		if part.FormName() == "alts" && part.FileName() == "" && !hasAlts {
			hasAlts = true
			b, err := io.ReadAll(io.LimitReader(part, 4097))
			_ = part.Close()
			if err != nil || len(b) > 4096 || product.StrictJSON(b, &alts) != nil {
				h.fail(w, r, product.ErrValidation)
				return
			}
			continue
		}
		if part.FormName() != "files" || part.FileName() == "" || len(uploads) >= product.MaxImages {
			_ = part.Close()
			h.fail(w, r, product.ErrValidation)
			return
		}
		data, err := io.ReadAll(io.LimitReader(part, product.MaxImageBytes+1))
		_ = part.Close()
		if err != nil || int64(len(data)) > product.MaxImageBytes {
			middleware.WriteError(w, r, 413, middleware.CodePayloadTooLarge, "Image exceeds 5 MiB.", nil)
			return
		}
		uploads = append(uploads, product.Upload{Filename: part.FileName(), MIME: part.Header.Get("Content-Type"), Data: data})
	}
	if hasAlts && len(alts) != len(uploads) {
		h.fail(w, r, product.ErrValidation)
		return
	}
	for i := range uploads {
		if hasAlts {
			uploads[i].Alt = alts[i]
		}
	}
	p, err := h.svc.Upload(r.Context(), r.PathValue("id"), v, uploads, actor(r))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	sendProduct(w, 201, p)
}
func (h *productHandlers) order(w http.ResponseWriter, r *http.Request) {
	v, ok := version(w, r)
	if !ok {
		return
	}
	b, ok := jsonBody(w, r)
	if !ok {
		return
	}
	var input struct {
		Items     []string `json:"items"`
		PrimaryID string   `json:"primaryId"`
	}
	if product.StrictJSON(b, &input) != nil {
		h.fail(w, r, product.ErrValidation)
		return
	}
	p, err := h.svc.OrderImages(r.Context(), r.PathValue("id"), v, input.Items, input.PrimaryID, actor(r))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	sendProduct(w, 200, p)
}
func (h *productHandlers) removeImage(w http.ResponseWriter, r *http.Request) {
	v, ok := version(w, r)
	if !ok || !emptyBody(w, r) {
		return
	}
	p, err := h.svc.RemoveImage(r.Context(), r.PathValue("id"), r.PathValue("imageId"), v, actor(r))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	sendProduct(w, 200, p)
}
func (h *productHandlers) image(admin bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, err := h.svc.Image(r.Context(), r.PathValue("id"), r.PathValue("imageId"), admin)
		if err != nil {
			h.fail(w, r, err)
			return
		}
		file, err := h.svc.OpenImage(r.Context(), p)
		if err != nil {
			h.fail(w, r, err)
			return
		}
		defer file.Close()
		w.Header().Set("Content-Type", p.MIME)
		w.Header().Set("Content-Length", strconv.FormatInt(p.Size, 10))
		w.Header().Set("Content-Disposition", "inline")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		_, _ = io.CopyN(w, file, p.Size)
	}
}

func (s *Server) productBodyLimit(next http.Handler) http.Handler {
	normal := middleware.LimitBody(s.cfg.MaxRequestBodyBytes)(next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		prefix := "/api/v1/admin/products/"
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, prefix), "/images")
		if r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, prefix) && strings.HasSuffix(r.URL.Path, "/images") && product.ValidID(id) {
			if r.ContentLength > product.MaxUploadBytes {
				middleware.WriteError(w, r, 413, middleware.CodePayloadTooLarge, "Upload is too large.", nil)
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, product.MaxUploadBytes)
			next.ServeHTTP(w, r)
			return
		}
		normal.ServeHTTP(w, r)
	})
}
func (s *Server) registerProducts(mux *http.ServeMux) {
	if s.productService == nil {
		return
	}
	h := &productHandlers{svc: s.productService}
	mux.HandleFunc("GET /api/v1/products", h.list(true))
	mux.HandleFunc("GET /api/v1/products/{id}", h.get(true))
	mux.HandleFunc("GET /api/v1/products/{id}/images/{imageId}", h.image(false))
	methodFallback := func(pattern, allow string) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Allow", allow)
			middleware.WriteError(w, r, 405, middleware.CodeMethodNotAllowed, "Method not allowed for this endpoint.", nil)
		})
	}
	methodFallback("/api/v1/products", "GET, HEAD")
	methodFallback("/api/v1/products/{id}", "GET, HEAD")
	methodFallback("/api/v1/products/{id}/images/{imageId}", "GET, HEAD")
	if s.adminService == nil {
		return
	}
	ah := &adminHandlers{svc: s.adminService, cfg: s.cfg, cookies: auth.NewCookieConfig(s.cfg.Auth), logger: s.logger}
	protect := func(f http.HandlerFunc) http.HandlerFunc {
		return ah.requireAuth(middleware.RequireAuthenticated(middleware.RequireActiveAccount(middleware.RequireRoles("admin", "super_admin")(f))).ServeHTTP)
	}
	routes := map[string]http.HandlerFunc{"GET /api/v1/admin/products": h.list(false), "POST /api/v1/admin/products": h.create, "GET /api/v1/admin/products/{id}": h.get(false), "PATCH /api/v1/admin/products/{id}": h.patch, "DELETE /api/v1/admin/products/{id}": h.state("delete"), "POST /api/v1/admin/products/{id}/publish": h.state("publish"), "POST /api/v1/admin/products/{id}/unpublish": h.state("unpublish"), "POST /api/v1/admin/products/{id}/images": h.upload, "PATCH /api/v1/admin/products/{id}/images/order": h.order, "DELETE /api/v1/admin/products/{id}/images/{imageId}": h.removeImage, "GET /api/v1/admin/products/{id}/images/{imageId}": h.image(true)}
	for pattern, f := range routes {
		mux.HandleFunc(pattern, protect(f))
	}
	mux.HandleFunc("GET /api/v1/admin/inventory", protect(h.list(false)))
	mux.HandleFunc("PATCH /api/v1/admin/inventory/{id}", protect(h.patchInventory))
	methodFallback("/api/v1/admin/products", "GET, HEAD, POST")
	methodFallback("/api/v1/admin/products/{id}", "GET, HEAD, PATCH, DELETE")
	methodFallback("/api/v1/admin/products/{id}/publish", "POST")
	methodFallback("/api/v1/admin/products/{id}/unpublish", "POST")
	methodFallback("/api/v1/admin/products/{id}/images", "POST")
	methodFallback("/api/v1/admin/products/{id}/images/{imageId}", "GET, HEAD, DELETE")
	methodFallback("/api/v1/admin/inventory", "GET, HEAD")
	methodFallback("/api/v1/admin/inventory/{id}", "PATCH")
}
