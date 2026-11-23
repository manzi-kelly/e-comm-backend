package server

import (
	"context"
	"math"
	"net/http"
	"time"

	"autivashop/backend/internal/middleware"
)

// GET /api/v1/health — process health only; it never touches the database and
// never reports credentials, DSNs, host details or dependency versions.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	uptime := math.Round(time.Since(s.startedAt).Seconds()*100) / 100
	middleware.WriteJSON(w, http.StatusOK, map[string]any{
		"status":        "ok",
		"service":       "autivashop-api",
		"environment":   s.cfg.Environment,
		"uptimeSeconds": uptime,
		"timestamp":     time.Now().UTC().Format(time.RFC3339),
	})
}

// GET /api/v1/ready — PostgreSQL readiness with a bounded context and a safe
// client message. Connection details are never returned or logged here.
func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		s.logger.Warn("readiness check failed", "request_id", middleware.RequestIDFromContext(r.Context()))
		middleware.WriteError(w, r, http.StatusServiceUnavailable,
			middleware.CodeStorageUnavailable, "Database is not ready.", nil)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.Timeouts.DBQuery)
	defer cancel()
	if err := s.db.Ping(ctx); err != nil {
		s.logger.Warn("readiness check failed", "request_id", middleware.RequestIDFromContext(r.Context()))
		middleware.WriteError(w, r, http.StatusServiceUnavailable,
			middleware.CodeStorageUnavailable, "Database is not ready.", nil)
		return
	}

	middleware.WriteJSON(w, http.StatusOK, map[string]any{
		"status":   "ready",
		"database": "connected",
	})
}
