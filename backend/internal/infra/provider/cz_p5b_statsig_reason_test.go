package provider

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestCZP5bStatsigReasonBrowserRequired(t *testing.T) {
	if StatsigReasonBrowserRequired == "" {
		t.Fatal("StatsigReasonBrowserRequired is empty")
	}
	if !strings.Contains(StatsigReasonBrowserRequired, "browser") {
		t.Fatalf("StatsigReasonBrowserRequired = %q, want it to mention %q", StatsigReasonBrowserRequired, "browser")
	}
	err := fmt.Errorf("sync quota: %w", NewStatsigUnavailable(StatsigReasonBrowserRequired))
	if !errors.Is(err, ErrStatsigUnavailable) {
		t.Fatalf("err = %v, want it to wrap ErrStatsigUnavailable", err)
	}
	if got := StatsigUnavailableReason(err); got != StatsigReasonBrowserRequired {
		t.Fatalf("StatsigUnavailableReason = %q, want %q", got, StatsigReasonBrowserRequired)
	}
}
