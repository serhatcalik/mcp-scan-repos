package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"gitlab-doc-platform/internal/auth"
)

func TestProjectRateLimitIsolated(t *testing.T) {
	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set("oidc_claims", auth.Claims{ProjectID: c.GetHeader("Project"), NamespaceID: "101"})
	})
	router.Use(projectRateLimit(2))
	router.GET("/", func(c *gin.Context) { c.Status(http.StatusOK) })
	for _, test := range []struct {
		project string
		status  int
	}{{"42", 200}, {"42", 200}, {"42", 429}, {"43", 200}} {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("Project", test.project)
		resp := httptest.NewRecorder()
		router.ServeHTTP(resp, req)
		if resp.Code != test.status {
			t.Fatalf("status=%d want=%d", resp.Code, test.status)
		}
		if test.status == 429 && resp.Header().Get("Retry-After") == "" {
			t.Error("missing retry hint")
		}
	}
}
