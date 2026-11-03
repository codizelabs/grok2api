// Package observe is CodizeLabs fork patch P6: a read-only view of grok2api's
// account pool, usage, recent requests and proxies for the CodizeLabs
// generation layer's dashboard (AIFriend DECISIONS D36, ADR-0001 review log).
//
// Every view is built field by field from grok2api's own services. Nothing is
// spread from a domain struct, so a field added upstream never leaks. No view
// carries a credential, token, cookie, e-mail, user id, account name, client
// IP, request header, request body, prompt text, proxy URL or exit IP:
// accounts and proxies show only an opaque keyed label (P3's labeler) and an
// "ab…yz" mask, and free-text errors become short codes.
package observe

import (
	"context"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	auditapp "github.com/chenyme/grok2api/backend/internal/application/audit"
	dashboardapp "github.com/chenyme/grok2api/backend/internal/application/dashboard"
	egressapp "github.com/chenyme/grok2api/backend/internal/application/egress"
	"github.com/chenyme/grok2api/backend/internal/cz/imaginepost"
	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	auditdomain "github.com/chenyme/grok2api/backend/internal/domain/audit"
	egressdomain "github.com/chenyme/grok2api/backend/internal/domain/egress"
)

// MinTokenLength is the shortest observe token P6 accepts; a shorter one
// leaves the routes unconfigured (503).
const MinTokenLength = 32

// MaxAccounts and MaxProxies bound one read; MaxRequests bounds the log.
const (
	MaxAccounts = 2000
	MaxProxies  = 2000
	MaxRequests = 100
)

// Accounts is the part of grok2api's account service P6 reads.
type Accounts interface {
	List(ctx context.Context, page, pageSize int, search string, filter accountapp.ListFilter) ([]accountapp.View, int64, error)
	Summary(ctx context.Context) (accountapp.Summary, error)
}

// Usage is the part of grok2api's dashboard service P6 reads.
type Usage interface {
	Get(ctx context.Context, rawPeriod, rawTimezone string) (dashboardapp.Result, error)
}

// Requests is the part of grok2api's audit service P6 reads.
type Requests interface {
	List(ctx context.Context, page, pageSize int) ([]auditdomain.Record, int64, error)
}

// Proxies is the part of grok2api's egress service P6 reads.
type Proxies interface {
	List(ctx context.Context, page, pageSize int, search string, filter egressapp.ListFilter) ([]egressdomain.PublicNode, int64, error)
}

var (
	_ Accounts = (*accountapp.Service)(nil)
	_ Usage    = (*dashboardapp.Service)(nil)
	_ Requests = (*auditapp.Service)(nil)
	_ Proxies  = (*egressapp.Service)(nil)
)

// ErrInvalidPeriod is a usage period other than 24h or 7d.
var ErrInvalidPeriod = errors.New("observe: period must be 24h or 7d")

// Options builds a Service. LabelSecret is the same secret P3 keys its labels
// with, so an account has one label across P3 and P6.
type Options struct {
	Accounts    Accounts
	Usage       Usage
	Requests    Requests
	Proxies     Proxies
	LabelSecret string
	Now         func() time.Time
}

// Service builds P6's views. It never writes.
type Service struct {
	accounts Accounts
	usage    Usage
	requests Requests
	proxies  Proxies
	labels   *imaginepost.Labeler
	now      func() time.Time
}

// New builds the service; a nil source answers "unavailable" for its view.
func New(o Options) *Service {
	now := o.Now
	if now == nil {
		now = time.Now
	}
	return &Service{accounts: o.Accounts, usage: o.Usage, requests: o.Requests, proxies: o.Proxies, labels: imaginepost.NewLabeler(o.LabelSecret), now: now}
}

// ErrUnavailable is a view whose source isn't wired.
var ErrUnavailable = errors.New("observe: source unavailable")

// QuotaWindow is one quota window of an account.
type QuotaWindow struct {
	Mode         string     `json:"mode"`
	Remaining    int        `json:"remaining"`
	Total        int        `json:"total"`
	UsagePercent float64    `json:"usage_percent"`
	WindowS      int        `json:"window_s"`
	ResetAt      *time.Time `json:"reset_at"`
	SyncedAt     *time.Time `json:"synced_at"`
	Source       string     `json:"source"`
}

// QuotaSummary is grok2api's own one-line reading of an account's quota.
type QuotaSummary struct {
	Type         string     `json:"type"`
	Unit         string     `json:"unit"`
	Remaining    float64    `json:"remaining"`
	Limit        float64    `json:"limit"`
	LimitKnown   bool       `json:"limit_known"`
	UsagePercent float64    `json:"usage_percent"`
	WindowHours  int        `json:"window_hours"`
	Status       string     `json:"status"`
	NextProbeAt  *time.Time `json:"next_probe_at"`
}

// Account is one account as P6 shows it.
type Account struct {
	Label          string        `json:"label"`
	Provider       string        `json:"provider"`
	IdentityMasked string        `json:"identity_masked"`
	Tier           string        `json:"tier"`
	State          string        `json:"state"`
	Reason         string        `json:"reason"`
	Until          *time.Time    `json:"until"`
	Enabled        bool          `json:"enabled"`
	AuthStatus     string        `json:"auth_status"`
	Priority       int           `json:"priority"`
	MaxConcurrent  int           `json:"max_concurrent"`
	FailureCount   int           `json:"failure_count"`
	CooldownUntil  *time.Time    `json:"cooldown_until"`
	LastErrorCode  string        `json:"last_error_code"`
	LastUsedAt     *time.Time    `json:"last_used_at"`
	Quota          QuotaSummary  `json:"quota"`
	QuotaWindows   []QuotaWindow `json:"quota_windows"`
}

// AccountList is GET /cz/v1/observe/accounts.
type AccountList struct {
	Accounts  []Account `json:"accounts"`
	Total     int64     `json:"total"`
	Truncated bool      `json:"truncated"`
	ReadAt    time.Time `json:"read_at"`
}

// ProviderCount is one provider's account counts.
type ProviderCount struct {
	Total     int64 `json:"total"`
	Available int64 `json:"available"`
}

// Summary is GET /cz/v1/observe/summary.
type Summary struct {
	Total          int64                    `json:"total"`
	Available      int64                    `json:"available"`
	Recovering     int64                    `json:"recovering"`
	Attention      int64                    `json:"attention"`
	Cooldown       int64                    `json:"cooldown"`
	WaitingReset   int64                    `json:"waiting_reset"`
	Probing        int64                    `json:"probing"`
	Disabled       int64                    `json:"disabled"`
	ReauthRequired int64                    `json:"reauth_required"`
	Providers      map[string]ProviderCount `json:"providers"`
	ReadAt         time.Time                `json:"read_at"`
}

// UsagePoint is one bucket of the request series.
type UsagePoint struct {
	Start    time.Time `json:"start"`
	Requests int64     `json:"requests"`
}

// ProviderUsage is one provider's share of the period.
type ProviderUsage struct {
	Provider   string `json:"provider"`
	Requests   int64  `json:"requests"`
	Successful int64  `json:"successful"`
}

// UsageView is GET /cz/v1/observe/usage.
type UsageView struct {
	Period            string          `json:"period"`
	Start             time.Time       `json:"start"`
	End               time.Time       `json:"end"`
	Requests          int64           `json:"requests"`
	Successful        int64           `json:"successful"`
	Failed            int64           `json:"failed"`
	SuccessRate       *float64        `json:"success_rate"`
	AvgFirstTokenMS   *float64        `json:"avg_first_token_ms"`
	FirstTokenSamples int64           `json:"first_token_samples"`
	Series            []UsagePoint    `json:"series"`
	Providers         []ProviderUsage `json:"providers"`
	ReadAt            time.Time       `json:"read_at"`
}

// Request is one entry of the recent request log: no prompt, header, body,
// client key, client IP, path or account name.
type Request struct {
	At            time.Time `json:"at"`
	Operation     string    `json:"operation"`
	Model         string    `json:"model"`
	Provider      string    `json:"provider"`
	AccountLabel  string    `json:"account_label"`
	StatusCode    int       `json:"status_code"`
	OK            bool      `json:"ok"`
	ErrorCode     string    `json:"error_code"`
	DurationMS    int64     `json:"duration_ms"`
	FirstTokenMS  *int64    `json:"first_token_ms"`
	Attempts      int       `json:"attempts"`
	Streaming     bool      `json:"streaming"`
	OutputImages  int64     `json:"output_images"`
	OutputSeconds int64     `json:"output_seconds"`
}

// RequestList is GET /cz/v1/observe/requests.
type RequestList struct {
	Requests []Request `json:"requests"`
	ReadAt   time.Time `json:"read_at"`
}

// Proxy is one egress node as P6 shows it: no name, URL, exit IP or cookie.
type Proxy struct {
	Label            string     `json:"label"`
	Scope            string     `json:"scope"`
	Enabled          bool       `json:"enabled"`
	State            string     `json:"state"`
	Health           float64    `json:"health"`
	ProbeStatus      string     `json:"probe_status"`
	ProbeLatencyMS   int        `json:"probe_latency_ms"`
	LastProbedAt     *time.Time `json:"last_probed_at"`
	FailureCount     int        `json:"failure_count"`
	CooldownUntil    *time.Time `json:"cooldown_until"`
	AssignedAccounts int        `json:"assigned_accounts"`
	Capacity         int        `json:"capacity"`
	LastErrorCode    string     `json:"last_error_code"`
}

// ProxyList is GET /cz/v1/observe/proxies. Direct mode has no nodes.
type ProxyList struct {
	Proxies   []Proxy   `json:"proxies"`
	Total     int64     `json:"total"`
	Healthy   int       `json:"healthy"`
	Unhealthy int       `json:"unhealthy"`
	Unknown   int       `json:"unknown"`
	Cooling   int       `json:"cooling"`
	Truncated bool      `json:"truncated"`
	ReadAt    time.Time `json:"read_at"`
}

// MaskIdentity shows the first two and last two characters of an account's
// name or e-mail, and nothing for one too short to mask.
func MaskIdentity(value string) string {
	r := []rune(strings.TrimSpace(value))
	if len(r) <= 6 {
		return "…"
	}
	return string(r[:2]) + "…" + string(r[len(r)-2:])
}

var codeRE = regexp.MustCompile(`^[a-z0-9_.]{1,64}$`)
var modelRE = regexp.MustCompile(`^[A-Za-z0-9._:/-]{1,64}$`)

// ErrorCode turns grok2api's free-text last error (which may quote upstream
// text) into a short code. Nothing of the text itself is returned.
func ErrorCode(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	has := func(parts ...string) bool {
		for _, p := range parts {
			if strings.Contains(s, p) {
				return true
			}
		}
		return false
	}
	switch {
	case s == "":
		return ""
	case has("429", "rate limit", "rate_limit", "ratelimit", "too many"):
		return "rate_limited"
	case has("quota", "exhaust", "limit reached", "usage limit"):
		return "quota"
	case has("401", "unauthor", "reauth", "invalid_grant", "expired", "token"):
		return "auth"
	case has("403", "forbidden", "denied"):
		return "forbidden"
	case has("statsig"):
		return "statsig"
	case has("cloudflare", "challenge", "captcha"):
		return "challenge"
	case has("timeout", "deadline"):
		return "timeout"
	case has("proxy", "egress", "dial", "connect", "transport", "eof", "tls", "reset by peer"):
		return "network"
	case has("500", "502", "503", "504", "upstream"):
		return "upstream_error"
	default:
		return "error"
	}
}

// safeCode passes grok2api's own short error codes through and anything else
// as "error".
func safeCode(raw string) string {
	if raw == "" {
		return ""
	}
	if codeRE.MatchString(raw) {
		return raw
	}
	return "error"
}

func safeModel(raw string) string {
	if modelRE.MatchString(raw) {
		return raw
	}
	return ""
}

func (s *Service) accountLabel(id uint64) string { return s.labels.AccountLabel(id) }

func future(t *time.Time, now time.Time) bool { return t != nil && t.After(now) }

func copyTime(t *time.Time) *time.Time {
	if t == nil || t.IsZero() {
		return nil
	}
	v := t.UTC()
	return &v
}

// accountState is the dashboard's state for an account: disabled, failed
// (re-auth needed), cooling (a cool-down or a quota waiting for its reset),
// or active.
func accountState(v accountapp.View, now time.Time) (string, string, *time.Time) {
	c := v.Credential
	switch {
	case !c.Enabled:
		return "disabled", "config", nil
	case c.AuthStatus == accountdomain.AuthStatusReauthRequired:
		return "failed", "auth", nil
	case future(c.CooldownUntil, now):
		return "cooling", "cooldown", copyTime(c.CooldownUntil)
	case v.Quota.Status == accountapp.QuotaStatusWaitingReset || v.Quota.Status == accountapp.QuotaStatusProbing:
		var until *time.Time
		for _, w := range v.QuotaWindows {
			if w.Total > 0 && w.Remaining <= 0 && future(w.ResetAt, now) && (until == nil || w.ResetAt.Before(*until)) {
				until = copyTime(w.ResetAt)
			}
		}
		if until == nil {
			until = copyTime(v.Quota.NextProbeAt)
		}
		return "cooling", "quota", until
	default:
		return "active", "", nil
	}
}

func (s *Service) account(v accountapp.View, now time.Time) Account {
	c := v.Credential
	state, reason, until := accountState(v, now)
	windows := make([]QuotaWindow, 0, len(v.QuotaWindows))
	for _, w := range v.QuotaWindows {
		windows = append(windows, QuotaWindow{Mode: safeCode(w.Mode), Remaining: w.Remaining, Total: w.Total, UsagePercent: w.UsagePercent,
			WindowS: w.WindowSeconds, ResetAt: copyTime(w.ResetAt), SyncedAt: copyTime(w.SyncedAt), Source: safeCode(string(w.Source))})
	}
	q := v.Quota
	return Account{
		Label: s.accountLabel(c.ID), Provider: string(c.Provider), IdentityMasked: MaskIdentity(firstNonEmpty(c.Email, c.Name)),
		Tier: safeCode(string(c.WebTier)), State: state, Reason: reason, Until: until, Enabled: c.Enabled, AuthStatus: safeCode(strings.ToLower(string(c.AuthStatus))),
		Priority: c.Priority, MaxConcurrent: c.MaxConcurrent, FailureCount: c.FailureCount, CooldownUntil: copyTime(c.CooldownUntil),
		LastErrorCode: ErrorCode(c.LastError), LastUsedAt: copyTime(c.LastUsedAt),
		Quota: QuotaSummary{Type: safeCode(string(q.Type)), Unit: safeCode(q.Unit), Remaining: q.Remaining, Limit: q.Limit, LimitKnown: q.LimitKnown,
			UsagePercent: q.UsagePercent, WindowHours: q.WindowHours, Status: safeCode(strings.ToLower(string(q.Status))), NextProbeAt: copyTime(q.NextProbeAt)},
		QuotaWindows: windows,
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// Accounts lists every account (up to MaxAccounts), highest priority first as
// grok2api's own list orders them.
func (s *Service) Accounts(ctx context.Context) (AccountList, error) {
	if s.accounts == nil {
		return AccountList{}, ErrUnavailable
	}
	values, total, err := s.accounts.List(ctx, 1, MaxAccounts, "", accountapp.ListFilter{})
	if err != nil {
		return AccountList{}, err
	}
	now := s.now().UTC()
	out := make([]Account, 0, len(values))
	for _, v := range values {
		out = append(out, s.account(v, now))
	}
	return AccountList{Accounts: out, Total: total, Truncated: total > int64(len(out)), ReadAt: now}, nil
}

// Summary is grok2api's own account summary counts.
func (s *Service) Summary(ctx context.Context) (Summary, error) {
	if s.accounts == nil {
		return Summary{}, ErrUnavailable
	}
	v, err := s.accounts.Summary(ctx)
	if err != nil {
		return Summary{}, err
	}
	providers := make(map[string]ProviderCount, len(v.Providers))
	for _, p := range accountdomain.Providers() {
		c := v.Providers[string(p)]
		providers[string(p)] = ProviderCount{Total: c.Total, Available: c.Available}
	}
	return Summary{Total: v.Total, Available: v.Available, Recovering: v.Recovering, Attention: v.Attention, Cooldown: v.Recovery.Cooldown,
		WaitingReset: v.Recovery.WaitingReset, Probing: v.Recovery.Probing, Disabled: v.Issues.Disabled, ReauthRequired: v.Issues.ReauthRequired,
		Providers: providers, ReadAt: s.now().UTC()}, nil
}

// Usage is requests, success rate and first-token latency for 24h or 7d.
func (s *Service) Usage(ctx context.Context, period string) (UsageView, error) {
	if period == "" {
		period = "24h"
	}
	if period != "24h" && period != "7d" {
		return UsageView{}, ErrInvalidPeriod
	}
	if s.usage == nil {
		return UsageView{}, ErrUnavailable
	}
	r, err := s.usage.Get(ctx, period, "UTC")
	if err != nil {
		return UsageView{}, err
	}
	u := r.Usage
	view := UsageView{Period: period, Start: r.Range.Start.UTC(), End: r.Range.End.UTC(), Requests: u.Requests, Successful: u.SuccessfulRequests,
		Failed: u.FailedRequests, FirstTokenSamples: u.FirstTokenSamples, Series: []UsagePoint{}, Providers: []ProviderUsage{}, ReadAt: s.now().UTC()}
	if u.Requests > 0 {
		rate := float64(u.SuccessfulRequests) / float64(u.Requests)
		view.SuccessRate = &rate
	}
	if u.FirstTokenSamples > 0 {
		avg := float64(u.FirstTokenTotalMS) / float64(u.FirstTokenSamples)
		view.AvgFirstTokenMS = &avg
	}
	for _, p := range r.Series {
		view.Series = append(view.Series, UsagePoint{Start: p.Start.UTC(), Requests: p.Requests})
	}
	for _, p := range r.Providers {
		if accountdomain.Provider(p.Provider).IsValid() {
			view.Providers = append(view.Providers, ProviderUsage{Provider: p.Provider, Requests: p.Requests, Successful: p.SuccessfulRequests})
		}
	}
	return view, nil
}

// RecentRequests is the newest requests, newest first, at most MaxRequests.
func (s *Service) RecentRequests(ctx context.Context, limit int) (RequestList, error) {
	if limit <= 0 || limit > MaxRequests {
		limit = 50
	}
	if s.requests == nil {
		return RequestList{}, ErrUnavailable
	}
	values, _, err := s.requests.List(ctx, 1, limit)
	if err != nil {
		return RequestList{}, err
	}
	out := make([]Request, 0, len(values))
	for _, r := range values {
		label := ""
		if r.AccountID != nil {
			label = s.accountLabel(*r.AccountID)
		}
		var first *int64
		if r.FirstTokenMS != nil {
			v := *r.FirstTokenMS
			first = &v
		}
		provider := ""
		if accountdomain.Provider(r.Provider).IsValid() {
			provider = r.Provider
		}
		code := safeCode(r.ErrorCode)
		out = append(out, Request{At: r.CreatedAt.UTC(), Operation: safeCode(string(r.Operation)), Model: safeModel(r.ModelPublicID), Provider: provider,
			AccountLabel: label, StatusCode: r.StatusCode, OK: r.StatusCode > 0 && r.StatusCode < 400 && code == "", ErrorCode: code,
			DurationMS: r.DurationMS, FirstTokenMS: first, Attempts: r.AttemptCount, Streaming: r.Streaming,
			OutputImages: r.MediaOutputImages, OutputSeconds: r.MediaOutputSeconds})
	}
	return RequestList{Requests: out, ReadAt: s.now().UTC()}, nil
}

// Proxies is every egress node's health. In direct mode the list is empty.
func (s *Service) Proxies(ctx context.Context) (ProxyList, error) {
	if s.proxies == nil {
		return ProxyList{}, ErrUnavailable
	}
	values, total, err := s.proxies.List(ctx, 1, MaxProxies, "", egressapp.ListFilter{})
	if err != nil {
		return ProxyList{}, err
	}
	now := s.now().UTC()
	out := ProxyList{Proxies: make([]Proxy, 0, len(values)), Total: total, Truncated: total > int64(len(values)), ReadAt: now}
	for _, n := range values {
		state := "healthy"
		switch {
		case !n.Enabled:
			state = "disabled"
		case future(n.CooldownUntil, now):
			state = "cooling"
			out.Cooling++
		case n.ProbeStatus == egressdomain.ProbeStatusUnhealthy:
			state = "unhealthy"
		case n.ProbeStatus == egressdomain.ProbeStatusHealthy:
			state = "healthy"
		default:
			state = "unknown"
		}
		switch n.ProbeStatus {
		case egressdomain.ProbeStatusHealthy:
			out.Healthy++
		case egressdomain.ProbeStatusUnhealthy:
			out.Unhealthy++
		default:
			out.Unknown++
		}
		lastErr := ErrorCode(n.LastError)
		if lastErr == "" {
			lastErr = ErrorCode(n.ProbeError)
		}
		out.Proxies = append(out.Proxies, Proxy{Label: "proxy_" + s.labels.Ref("egress-node:"+strconv.FormatUint(n.ID, 10)), Scope: safeCode(string(n.Scope)),
			Enabled: n.Enabled, State: state, Health: n.Health, ProbeStatus: safeCode(string(n.ProbeStatus)), ProbeLatencyMS: n.ProbeLatencyMS,
			LastProbedAt: copyTime(n.LastProbedAt), FailureCount: n.FailureCount, CooldownUntil: copyTime(n.CooldownUntil),
			AssignedAccounts: n.AssignedAccountCount, Capacity: n.AccountCapacity, LastErrorCode: lastErr})
	}
	return out, nil
}

// Overview is GET /cz/v1/observe/overview: everything the dashboard polls, in
// one read. A part whose source fails is null and named in Errors.
type Overview struct {
	Summary  *Summary     `json:"summary"`
	Accounts *AccountList `json:"accounts"`
	Usage    *UsageView   `json:"usage"`
	Requests *RequestList `json:"requests"`
	Proxies  *ProxyList   `json:"proxies"`
	Errors   []string     `json:"errors"`
	ReadAt   time.Time    `json:"read_at"`
}

// Overview reads every view once.
func (s *Service) Overview(ctx context.Context, requests int) Overview {
	o := Overview{Errors: []string{}, ReadAt: s.now().UTC()}
	if v, err := s.Summary(ctx); err == nil {
		o.Summary = &v
	} else {
		o.Errors = append(o.Errors, "summary")
	}
	if v, err := s.Accounts(ctx); err == nil {
		o.Accounts = &v
	} else {
		o.Errors = append(o.Errors, "accounts")
	}
	if v, err := s.Usage(ctx, "24h"); err == nil {
		o.Usage = &v
	} else {
		o.Errors = append(o.Errors, "usage")
	}
	if v, err := s.RecentRequests(ctx, requests); err == nil {
		o.Requests = &v
	} else {
		o.Errors = append(o.Errors, "requests")
	}
	if v, err := s.Proxies(ctx); err == nil {
		o.Proxies = &v
	} else {
		o.Errors = append(o.Errors, "proxies")
	}
	return o
}
