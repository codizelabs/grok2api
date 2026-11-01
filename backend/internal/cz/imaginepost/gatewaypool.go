package imaginepost

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/chenyme/grok2api/backend/internal/application/gateway"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	clientkeydomain "github.com/chenyme/grok2api/backend/internal/domain/clientkey"
)

// Upstream models and quota modes P3 leases accounts for (web catalog).
const (
	imageEditModel = "imagine-image-edit"
	videoModel     = "grok-imagine-video"
)

// GatewayPool adapts grok2api's selector (gateway.Service) to Pool.
type GatewayPool struct {
	Service *gateway.Service
}

type gatewayLease struct{ lease *gateway.CZImagineLease }

func (l gatewayLease) Credential() account.Credential { return l.lease.Credential }
func (l gatewayLease) Release()                       { l.lease.Release() }

func purposeRoute(purpose Purpose) (string, string) {
	switch purpose {
	case PurposeVideo:
		return videoModel, account.QuotaModeWebVideo720p
	case PurposeImage:
		return imageEditModel, account.QuotaModeWebImageEdit
	default:
		return imageEditModel, ""
	}
}

// poolRefusal carries a selector refusal's status and retry delay, nothing else.
type poolRefusal struct {
	status     int
	retryAfter time.Duration
}

func (r *poolRefusal) Error() string                     { return "account pool refused" }
func (r *poolRefusal) HTTPStatus() int                   { return r.status }
func (r *poolRefusal) RetryAfterDuration() time.Duration { return r.retryAfter }

func refusal(err error) error {
	var unavailable *gateway.SelectionUnavailableError
	if errors.As(err, &unavailable) {
		return &poolRefusal{status: unavailable.HTTPStatus(), retryAfter: unavailable.RetryAfter}
	}
	return &poolRefusal{status: http.StatusServiceUnavailable}
}

// AcquirePinned leases exactly accountID through AcquirePinnedForKey.
func (p GatewayPool) AcquirePinned(ctx context.Context, accountID uint64, purpose Purpose, scope clientkeydomain.AccountScope) (Lease, error) {
	model, quotaMode := purposeRoute(purpose)
	lease, err := p.Service.CZAcquireImaginePinned(ctx, accountID, model, quotaMode, scope)
	if err != nil {
		return nil, refusal(err)
	}
	return gatewayLease{lease: lease}, nil
}

// AcquireNew picks an account for a new post.
func (p GatewayPool) AcquireNew(ctx context.Context, purpose Purpose, scope clientkeydomain.AccountScope) (Lease, error) {
	model, quotaMode := purposeRoute(purpose)
	lease, err := p.Service.CZAcquireImagineNew(ctx, model, quotaMode, scope)
	if err != nil {
		return nil, refusal(err)
	}
	return gatewayLease{lease: lease}, nil
}

// Report feeds a 401 or 429 into the account's health.
func (p GatewayPool) Report(ctx context.Context, credential account.Credential, status int) {
	p.Service.CZReportImagineStatus(ctx, credential, status)
}
