package gintelemetry

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/guilhermelinosp/hellnet-lib-telemetry/telemetry"
)

func TestMiddlewarePreservesContextAndRoutePattern(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tel, err := telemetry.NewWithOptions(context.Background(), telemetry.Options{ServiceName: "gin-test"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tel.Close(context.Background()) }()

	router := gin.New()
	router.Use(Middleware(tel))
	router.GET("/orders/:id", func(c *gin.Context) {
		if got := c.Request.Pattern; got != "/orders/:id" {
			t.Fatalf("request pattern = %q", got)
		}
		if c.Request.Context() == nil {
			t.Fatal("request context is nil")
		}
		c.Status(204)
	})

	req := httptest.NewRequest("GET", "/orders/123", nil)
	res := httptest.NewRecorder()
	router.ServeHTTP(res, req)
	if res.Code != 204 {
		t.Fatalf("status = %d, want 204", res.Code)
	}
}
