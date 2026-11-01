package gateway

import (
	"context"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/media"
)

func (a *p2CountingVideoAdapter) setGenerate(generate func(context.Context) error) {
	a.mu.Lock()
	a.generate = generate
	a.mu.Unlock()
}

func (h *p2Harness) claim(t *testing.T, id string) media.Job {
	t.Helper()
	job, claimed, err := h.service.claimVideoJob(h.ctx, id)
	if err != nil || !claimed {
		t.Fatalf("claim %s: claimed=%v err=%v", id, claimed, err)
	}
	return job
}

// A shutdown that lands after the create was sent may have produced a paid
// generation: the job must end "interrupted" at once, never be deferred, and
// recovery must never send it again (ADR 0001 § 7, P2).
func TestP2ShutdownAfterCreateStartedMarksInterrupted(t *testing.T) {
	h := newP2Harness(t, nil)
	const id = "video_p2_shutdown_after_create"
	h.create(t, h.job(id, media.StatusQueued, time.Now(), nil))
	job := h.claim(t, id)

	parent, shutdown := context.WithCancel(h.ctx)
	defer shutdown()
	h.adapter.setGenerate(func(context.Context) error {
		shutdown() // the worker is told to stop while the create is in flight
		return context.Canceled
	})
	h.service.runVideoJob(parent, job, h.route)
	if calls := h.adapter.Calls(); calls != 1 {
		t.Fatalf("GenerateVideo calls = %d, want 1 (the create that was in flight)", calls)
	}
	stored := h.get(t, id)
	if stored.Status != media.StatusFailed || stored.ErrorCode != "interrupted" {
		t.Errorf("after shutdown mid-create: status = %q, code = %q, want %q / %q (not deferred)",
			stored.Status, stored.ErrorCode, media.StatusFailed, "interrupted")
	}

	// Even once a deferral's lease would have run out, recovery must not re-send.
	h.adapter.setGenerate(nil)
	if stored.LeaseUntil != nil {
		expired := time.Now().Add(-time.Minute).UTC()
		stored.LeaseUntil = &expired
		if err := h.mediaRepo.UpdateMediaJob(h.ctx, stored); err != nil {
			t.Fatal(err)
		}
	}
	stored = h.recoverAndRun(t, id, time.Second)
	if calls := h.adapter.Calls(); calls != 1 {
		t.Errorf("GenerateVideo calls after recovery = %d, want still 1 (0 re-sends)", calls)
	}
	if stored.Status != media.StatusFailed || stored.ErrorCode != "interrupted" {
		t.Errorf("after recovery: status = %q, code = %q, want %q / %q", stored.Status, stored.ErrorCode, media.StatusFailed, "interrupted")
	}
}

// A shutdown before anything was sent defers the job as before; once the
// deferral is over, the same process still runs it exactly once.
func TestP2ShutdownBeforeSendDefersAndStillRunsOnce(t *testing.T) {
	h := newP2Harness(t, nil)
	const id = "video_p2_shutdown_before_send"
	h.create(t, h.job(id, media.StatusQueued, time.Now(), nil))
	job := h.claim(t, id)

	parent, shutdown := context.WithCancel(h.ctx)
	shutdown() // stopped before the worker reaches the create
	h.service.runVideoJob(parent, job, h.route)
	if calls := h.adapter.Calls(); calls != 0 {
		t.Fatalf("GenerateVideo calls = %d, want 0 (shutdown came before the send)", calls)
	}
	stored := h.get(t, id)
	if p2IsTerminal(stored.Status) {
		t.Fatalf("pre-send shutdown: status = %q, code = %q, want the job deferred (not terminal)", stored.Status, stored.ErrorCode)
	}

	// The deferral's delay passes.
	expired := time.Now().Add(-time.Minute).UTC()
	stored.LeaseUntil = &expired
	if err := h.mediaRepo.UpdateMediaJob(h.ctx, stored); err != nil {
		t.Fatal(err)
	}
	stored = h.recoverAndRun(t, id, 5*time.Second)
	if calls := h.adapter.Calls(); calls != 1 {
		t.Errorf("GenerateVideo calls = %d, want exactly 1", calls)
	}
	if stored.Status != media.StatusCompleted || stored.ResultAssetID != "video_asset_00001" {
		t.Errorf("job status = %q, asset = %q, error = %q/%q, want completed with video_asset_00001",
			stored.Status, stored.ResultAssetID, stored.ErrorCode, stored.ErrorMessage)
	}
}
