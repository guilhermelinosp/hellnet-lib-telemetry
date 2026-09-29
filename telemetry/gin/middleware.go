// Package gintelemetry integrates hellnet-lib-telemetry with Gin.
package gintelemetry

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/guilhermelinosp/hellnet-lib-telemetry/telemetry"
)

type contextKey struct{}

// Middleware instruments a Gin engine with the library's HTTP middleware.
//
// It preserves the request context created by otelhttp and exposes Gin's
// route pattern through http.Request.Pattern, so request logs and metrics use
// /orders/:id instead of the high-cardinality /orders/123 value.
func Middleware(tel *telemetry.Telemetry) gin.HandlerFunc {
	if tel == nil {
		return func(c *gin.Context) { c.Next() }
	}

	handler := telemetry.Middleware(tel, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, ok := r.Context().Value(contextKey{}).(*gin.Context)
		if !ok || c == nil {
			return
		}
		c.Request = r
		c.Next()
	}))

	return func(c *gin.Context) {
		r := c.Request
		if pattern := c.FullPath(); pattern != "" {
			r = r.Clone(r.Context())
			r.Pattern = pattern
		}
		r = r.WithContext(context.WithValue(r.Context(), contextKey{}, c))
		handler.ServeHTTP(c.Writer, r)
	}
}
