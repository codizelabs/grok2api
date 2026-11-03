package observe

import (
	"crypto/subtle"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

// Methods P6 refuses with 405: P6 has no write route, now or later.
var writeMethods = []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions, http.MethodConnect, http.MethodTrace}

// Mount registers P6 under group (/cz/v1/observe). Every write method answers
// 405 before anything else is checked. Without a service or with no token (or
// one shorter than MinTokenLength) every read answers 503
// observe_not_configured; otherwise a read needs `Authorization: Bearer
// <token>` (constant-time compare) and answers 401 observe_unauthorized
// without it. The admin login is never accepted, and the observe token opens
// nothing else.
func Mount(group *gin.RouterGroup, service *Service, token string) {
	for _, method := range writeMethods {
		group.Handle(method, "/*path", methodNotAllowed)
	}
	configured := service != nil && len(token) >= MinTokenLength
	group.Use(func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		c.Header("X-Content-Type-Options", "nosniff")
		if !configured {
			fail(c, http.StatusServiceUnavailable, "observe_not_configured", "the observe token is not configured")
			return
		}
		raw, ok := strings.CutPrefix(c.GetHeader("Authorization"), "Bearer ")
		if !ok || len(raw) != len(token) || subtle.ConstantTimeCompare([]byte(raw), []byte(token)) != 1 {
			fail(c, http.StatusUnauthorized, "observe_unauthorized", "a valid observe token is required")
			return
		}
		c.Next()
	})
	h := handler{service: service}
	group.GET("/overview", h.overview)
	group.GET("/accounts", h.accounts)
	group.GET("/summary", h.summary)
	group.GET("/usage", h.usage)
	group.GET("/requests", h.requests)
	group.GET("/proxies", h.proxies)
	group.GET("/", h.index)
}

type handler struct{ service *Service }

func methodNotAllowed(c *gin.Context) {
	c.Header("Allow", "GET")
	fail(c, http.StatusMethodNotAllowed, "method_not_allowed", "the observe routes are read-only")
}

func fail(c *gin.Context, status int, code, message string) {
	c.AbortWithStatusJSON(status, gin.H{"error": gin.H{"code": code, "message": message}})
}

func readFailed(c *gin.Context, err error) {
	if errors.Is(err, ErrUnavailable) {
		fail(c, http.StatusServiceUnavailable, "source_unavailable", "this view is not available")
		return
	}
	fail(c, http.StatusInternalServerError, "read_failed", "the view could not be read")
}

func requestLimit(c *gin.Context) (int, bool) {
	raw := c.Query("limit")
	if raw == "" {
		return 50, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > MaxRequests {
		return 0, false
	}
	return n, true
}

func (h handler) index(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"patch": "P6", "read_only": true, "routes": []string{"overview", "accounts", "summary", "usage", "requests", "proxies"}})
}

func (h handler) overview(c *gin.Context) {
	n, ok := requestLimit(c)
	if !ok {
		fail(c, http.StatusBadRequest, "invalid_limit", "limit must be 1 to 100")
		return
	}
	c.JSON(http.StatusOK, h.service.Overview(c.Request.Context(), n))
}

func (h handler) accounts(c *gin.Context) {
	v, err := h.service.Accounts(c.Request.Context())
	if err != nil {
		readFailed(c, err)
		return
	}
	c.JSON(http.StatusOK, v)
}

func (h handler) summary(c *gin.Context) {
	v, err := h.service.Summary(c.Request.Context())
	if err != nil {
		readFailed(c, err)
		return
	}
	c.JSON(http.StatusOK, v)
}

func (h handler) usage(c *gin.Context) {
	v, err := h.service.Usage(c.Request.Context(), c.Query("period"))
	if errors.Is(err, ErrInvalidPeriod) {
		fail(c, http.StatusBadRequest, "invalid_period", "period must be 24h or 7d")
		return
	}
	if err != nil {
		readFailed(c, err)
		return
	}
	c.JSON(http.StatusOK, v)
}

func (h handler) requests(c *gin.Context) {
	n, ok := requestLimit(c)
	if !ok {
		fail(c, http.StatusBadRequest, "invalid_limit", "limit must be 1 to 100")
		return
	}
	v, err := h.service.RecentRequests(c.Request.Context(), n)
	if err != nil {
		readFailed(c, err)
		return
	}
	c.JSON(http.StatusOK, v)
}

func (h handler) proxies(c *gin.Context) {
	v, err := h.service.Proxies(c.Request.Context())
	if err != nil {
		readFailed(c, err)
		return
	}
	c.JSON(http.StatusOK, v)
}
