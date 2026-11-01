package httpserver

import (
	"net/http"

	imaginehttp "github.com/chenyme/grok2api/backend/internal/cz/imaginepost/http"
	"github.com/chenyme/grok2api/backend/internal/transport/http/middleware"
	"github.com/gin-gonic/gin"
)

// registerCZImaginePost mounts fork patch P3 under /cz/v1/imagine, behind the
// same concurrency gate, readiness gate and client-key auth as /v1. Without
// a P3 service every route answers 503.
func registerCZImaginePost(router *gin.Engine, deps Dependencies) {
	group := router.Group("/cz/v1/imagine")
	group.Use(deps.ConcurrencyGate.Middleware())
	if deps.TrafficReady != nil {
		group.Use(func(c *gin.Context) {
			if deps.TrafficReady() {
				c.Next()
				return
			}
			c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": gin.H{"code": "service_reconciling", "message": "service is finishing startup recovery", "send_state": "not_sent"}})
		})
	}
	group.Use(middleware.ClientAuth(deps.ClientKeys))
	if deps.CZImaginePost == nil {
		group.Any("/*path", func(c *gin.Context) {
			c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": gin.H{"code": "feature_unavailable", "message": "imagine posts are unavailable", "send_state": "not_sent"}})
		})
		return
	}
	imaginehttp.NewHandler(deps.CZImaginePost).Register(group)
}
