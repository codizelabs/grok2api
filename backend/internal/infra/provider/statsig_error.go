package provider

import (
	"errors"
	"strings"
)

// Readable reasons for ErrStatsigUnavailable. They are fixed strings, safe to
// show in an API error or the admin console: none carries a token, cookie,
// request path or upstream body.
const (
	StatsigReasonManualMissing   = "manual mode with no value"
	StatsigReasonManualInvalid   = "manual mode value is not a valid x-statsig-id"
	StatsigReasonSignerURLEmpty  = "url mode with no signer URL"
	StatsigReasonSignerURLBad    = "url mode signer URL is invalid"
	StatsigReasonSignerFailed    = "signer service returned no valid x-statsig-id"
	StatsigReasonNotInitialised  = "Statsig signer not initialised"
	StatsigReasonUnknownMode     = "unknown Statsig mode"
	StatsigReasonTargetInvalid   = "request URL could not be parsed for signing"
	StatsigReasonMetaMissing     = "grok.com page has no grok-site-verification value"
	StatsigReasonMetaInvalid     = "grok.com grok-site-verification value is not a valid seed"
	StatsigReasonCurvesMissing   = "grok.com page has no animation curves"
	StatsigReasonCurvesInvalid   = "grok.com animation curves have an unexpected shape"
	StatsigReasonPageUnreachable = "grok.com page could not be loaded"
	StatsigReasonPageStatus      = "grok.com page returned an error status"
	StatsigReasonPageTooLarge    = "grok.com page is larger than the safety limit"
	// StatsigReasonBrowserRequired: the endpoint is marked as needing a
	// signature, and statsigMode "browser" sends none.
	StatsigReasonBrowserRequired = "endpoint needs x-statsig-id and browser mode sends none; choose manual or url mode"
)

// StatsigUnavailableError is ErrStatsigUnavailable with the reason no
// x-statsig-id could be made. errors.Is(err, ErrStatsigUnavailable) holds.
type StatsigUnavailableError struct {
	Reason string
}

func (e *StatsigUnavailableError) Error() string {
	return "statsig_unavailable: " + e.reason() + "; request not sent"
}

func (e *StatsigUnavailableError) Unwrap() error { return ErrStatsigUnavailable }

func (e *StatsigUnavailableError) reason() string {
	if e == nil || strings.TrimSpace(e.Reason) == "" {
		return "no valid x-statsig-id"
	}
	return e.Reason
}

// NewStatsigUnavailable returns ErrStatsigUnavailable carrying reason.
func NewStatsigUnavailable(reason string) error {
	return &StatsigUnavailableError{Reason: reason}
}

// StatsigUnavailableReason returns the readable reason carried by err, or a
// generic one when err is ErrStatsigUnavailable without a reason. It returns
// "" when err is not a Statsig failure.
func StatsigUnavailableReason(err error) string {
	if !errors.Is(err, ErrStatsigUnavailable) {
		return ""
	}
	var typed *StatsigUnavailableError
	if errors.As(err, &typed) {
		return typed.reason()
	}
	return "no valid x-statsig-id"
}
