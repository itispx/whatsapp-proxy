package handler

import (
	"context"
	"log/slog"
	"net/http"
	"strings"

	"github.com/itispx/whatsapp-proxy/config"
	"github.com/itispx/whatsapp-proxy/metrics"
)

type contextKey string

const appContextKey contextKey = "app"

// Auth validates the Bearer API key and injects the matched App into the request context.
func Auth(cfg *config.Config, log *slog.Logger, m *metrics.Metrics, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Debug("auth: request received", "method", r.Method, "path", r.URL.Path)

		header := r.Header.Get("Authorization")
		raw, ok := strings.CutPrefix(header, "Bearer ")
		if !ok || raw == "" {
			log.Debug("auth: missing or invalid authorization header")
			m.AuthFailures.WithLabelValues("missing").Inc()
			writeError(w, http.StatusUnauthorized, "missing or invalid Authorization header")
			return
		}

		log.Debug("auth: bearer token found, looking up app")

		app, ok := cfg.AppByKeyHash(raw)
		if !ok {
			log.Debug("auth: no app matched the provided key")
			m.AuthFailures.WithLabelValues("invalid").Inc()
			writeError(w, http.StatusUnauthorized, "invalid API key")
			return
		}

		log.Debug("auth: app matched", "app_id", app.ID, "app_name", app.Name)

		ctx := context.WithValue(r.Context(), appContextKey, app)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func appFromContext(r *http.Request) *config.App {
	app, _ := r.Context().Value(appContextKey).(*config.App)
	return app
}
