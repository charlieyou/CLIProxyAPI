package handlers

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/latencydiag"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

func TestDiagnosticTimelineSurvivesHandlerContext(t *testing.T) {
	t.Setenv(latencydiag.EnvDirectory, t.TempDir())
	r := gin.New()
	r.Use(latencydiag.MiddlewareFromEnv())
	r.GET("/", func(c *gin.Context) {
		h := &BaseAPIHandler{Cfg: &config.SDKConfig{}}
		ctx, cancel := h.GetContextWithCancel(nil, c, context.Background())
		defer cancel()
		if latencydiag.FromContext(ctx) == nil || latencydiag.FromContext(ctx) != latencydiag.FromContext(c.Request.Context()) {
			t.Error("upstream context lost diagnostic timeline")
		}
	})
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
}
