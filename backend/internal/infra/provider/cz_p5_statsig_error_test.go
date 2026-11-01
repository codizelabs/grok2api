package provider

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestCZP5StatsigUnavailableCarriesReason(t *testing.T) {
	for _, reason := range []string{
		StatsigReasonManualMissing, StatsigReasonManualInvalid, StatsigReasonSignerURLEmpty,
		StatsigReasonMetaMissing, StatsigReasonMetaInvalid, StatsigReasonCurvesMissing, StatsigReasonCurvesInvalid,
	} {
		err := NewStatsigUnavailable(reason)
		if !errors.Is(err, ErrStatsigUnavailable) {
			t.Fatalf("%q: errors.Is(ErrStatsigUnavailable) = false", reason)
		}
		if got := StatsigUnavailableReason(err); got != reason {
			t.Fatalf("reason = %q, want %q", got, reason)
		}
		if !strings.HasPrefix(err.Error(), "statsig_unavailable: ") || !strings.Contains(err.Error(), reason) {
			t.Fatalf("Error() = %q, want prefix %q and the reason", err.Error(), "statsig_unavailable: ")
		}
		wrapped := fmt.Errorf("sync quota: %w", err)
		if StatsigUnavailableReason(wrapped) != reason || !errors.Is(wrapped, ErrStatsigUnavailable) {
			t.Fatalf("wrapped %q lost the reason or the sentinel", reason)
		}
	}
}

func TestCZP5StatsigReasonsAreFixed(t *testing.T) {
	want := map[string]string{
		StatsigReasonMetaMissing:    "grok.com page has no grok-site-verification value",
		StatsigReasonMetaInvalid:    "grok.com grok-site-verification value is not a valid seed",
		StatsigReasonCurvesMissing:  "grok.com page has no animation curves",
		StatsigReasonCurvesInvalid:  "grok.com animation curves have an unexpected shape",
		StatsigReasonManualMissing:  "manual mode with no value",
		StatsigReasonManualInvalid:  "manual mode value is not a valid x-statsig-id",
		StatsigReasonSignerURLEmpty: "url mode with no signer URL",
	}
	for got, expected := range want {
		if got != expected {
			t.Errorf("reason constant = %q, want %q", got, expected)
		}
	}
}

func TestCZP5StatsigUnavailableReasonOfOtherErrors(t *testing.T) {
	if got := StatsigUnavailableReason(errors.New("boom")); got != "" {
		t.Fatalf("reason for a non-statsig error = %q, want empty", got)
	}
	if got := StatsigUnavailableReason(nil); got != "" {
		t.Fatalf("reason for nil = %q, want empty", got)
	}
	if got := StatsigUnavailableReason(fmt.Errorf("x: %w", ErrStatsigUnavailable)); got == "" {
		t.Fatal("bare ErrStatsigUnavailable has no reason, want a generic one")
	}
}
