package middleware

import (
	"bytes"
	"io"
	"net/http"
)

// LimitBody enforces a strict request body limit. Body-bearing methods are
// buffered up to the limit so oversized chunked bodies are rejected before any
// handler runs. Other methods still get a MaxBytesReader guard.
func LimitBody(maxBytes int64) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.ContentLength > maxBytes {
				WriteError(w, r, http.StatusRequestEntityTooLarge, CodePayloadTooLarge,
					"Request body is too large.", nil)
				return
			}
			if r.Body == nil {
				next.ServeHTTP(w, r)
				return
			}

			switch r.Method {
			case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
				body, err := io.ReadAll(io.LimitReader(r.Body, maxBytes+1))
				_ = r.Body.Close()
				if err != nil {
					WriteError(w, r, http.StatusBadRequest, CodeBadRequest,
						"Request body could not be read.", nil)
					return
				}
				if int64(len(body)) > maxBytes {
					WriteError(w, r, http.StatusRequestEntityTooLarge, CodePayloadTooLarge,
						"Request body is too large.", nil)
					return
				}
				r.Body = io.NopCloser(bytes.NewReader(body))
			default:
				r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
			}
			next.ServeHTTP(w, r)
		})
	}
}
