package httpserver

import (
	"github.com/chenyme/grok2api/backend/internal/cz/observe"
	"github.com/gin-gonic/gin"
)

// registerCZObserve mounts fork patch P6 under /cz/v1/observe: GET-only views
// behind their own observe token (not the admin login, not a client key).
// Without the token every read answers 503 and grok2api runs as before.
func registerCZObserve(router *gin.Engine, deps Dependencies) {
	observe.Mount(router.Group("/cz/v1/observe"), deps.CZObserve, deps.CZObserveToken)
}
