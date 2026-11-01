package app

import (
	"context"
	"log/slog"
	"strings"

	"github.com/chenyme/grok2api/backend/internal/application/gateway"
	"github.com/chenyme/grok2api/backend/internal/cz/imaginepost"
	"github.com/chenyme/grok2api/backend/internal/infra/config"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	webprovider "github.com/chenyme/grok2api/backend/internal/infra/provider/web"
)

// newCZImaginePost builds fork patch P3. It is a feature-only dependency: if
// its tables can't be migrated, grok2api still starts, logs one warning and
// P3's routes answer 503.
func newCZImaginePost(ctx context.Context, cfg config.Config, database *relational.Database, gatewayService *gateway.Service, webAdapter *webprovider.Adapter, logger *slog.Logger) *imaginepost.Service {
	service, err := imaginepost.New(ctx, imaginepost.Options{
		DB: database.CZGorm(), Pool: imaginepost.GatewayPool{Service: gatewayService}, Transport: webAdapter,
		LabelSecret: cfg.Secrets.CredentialEncryptionKey, Logger: logger, Config: czImaginePostConfig(cfg),
	})
	if err != nil {
		logger.Warn("cz_imaginepost_unavailable", "reason", "startup failed; /cz/v1/imagine answers 503")
		return nil
	}
	if cfg.Audit.RetentionDays != 1 {
		logger.Warn("cz_imaginepost_audit_retention", "want_days", 1, "have_days", cfg.Audit.RetentionDays)
	}
	return service
}

func czImaginePostConfig(cfg config.Config) imaginepost.Config {
	return imaginepost.Config{
		AllowLike:            cfg.CZ.ImaginePost.AllowLike,
		PinnedOnlyAccountIDs: append([]uint64(nil), cfg.CZ.ImaginePost.PinnedOnlyAccountIDs...),
		DebugReplyShapeFile:  strings.TrimSpace(cfg.CZ.ImaginePost.DebugReplyShapeFile),
	}
}

func updateCZImaginePost(service *imaginepost.Service, next config.Config) {
	if service != nil {
		service.UpdateConfig(czImaginePostConfig(next))
	}
}
