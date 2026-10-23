package api

import (
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"gitlab-doc-platform/internal/auth"
)

// Per-replica limits are a backstop; the gateway should enforce global limits.
func projectRateLimit(limit int64) gin.HandlerFunc {
	if limit <= 0 {
		limit = 120
	}
	type window struct {
		start time.Time
		count int64
	}
	var mu sync.Mutex
	windows := map[string]window{}
	return func(c *gin.Context) {
		claims := c.MustGet("oidc_claims").(auth.Claims)
		key := claims.NamespaceID + ":" + claims.ProjectID
		now := time.Now()
		mu.Lock()
		for id, w := range windows {
			if now.Sub(w.start) >= time.Minute {
				delete(windows, id)
			}
		}
		w := windows[key]
		blocked := w.count >= limit || w.count == 0 && len(windows) >= 4096
		if !blocked {
			if w.count == 0 {
				w.start = now
			}
			w.count++
			windows[key] = w
		}
		mu.Unlock()
		if blocked {
			c.Header("Retry-After", "60")
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{"error": "project request limit exceeded"})
			return
		}
		c.Next()
	}
}
