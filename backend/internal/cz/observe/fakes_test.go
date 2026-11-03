package observe

import (
	"context"
	"errors"
	"reflect"
	"time"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	dashboardapp "github.com/chenyme/grok2api/backend/internal/application/dashboard"
	egressapp "github.com/chenyme/grok2api/backend/internal/application/egress"
	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	auditdomain "github.com/chenyme/grok2api/backend/internal/domain/audit"
	dashboarddomain "github.com/chenyme/grok2api/backend/internal/domain/dashboard"
	egressdomain "github.com/chenyme/grok2api/backend/internal/domain/egress"
)

// testToken is long enough for P6 (MinTokenLength).
const testToken = "observe-test-token-0123456789abcdef"

var t0 = time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)

func at(d time.Duration) *time.Time { v := t0.Add(d); return &v }

// canaryFill sets every string field (and every string in a []string, a
// map[string][]string or a []byte) reachable from v to a canary naming its
// field, except the fields in keep. A field upstream adds later is canaried
// too, so the leak test catches a view that starts copying it.
func canaryFill(v reflect.Value, path string, keep map[string]bool) {
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			return
		}
		canaryFill(v.Elem(), path, keep)
	case reflect.Struct:
		if v.Type() == reflect.TypeOf(time.Time{}) {
			return
		}
		for i := 0; i < v.NumField(); i++ {
			f := v.Type().Field(i)
			if !f.IsExported() || keep[f.Name] {
				continue
			}
			canaryFill(v.Field(i), path+"."+f.Name, keep)
		}
	case reflect.String:
		v.SetString("CANARYx9 " + path + " user@example.com sk-live-CANARY")
	case reflect.Slice:
		if v.Type().Elem().Kind() == reflect.Uint8 {
			v.SetBytes([]byte("CANARYx9 body " + path))
			return
		}
		if v.Type().Elem().Kind() == reflect.String {
			v.Set(reflect.ValueOf([]string{"CANARYx9 " + path}).Convert(v.Type()))
			return
		}
		for i := 0; i < v.Len(); i++ {
			canaryFill(v.Index(i), path, keep)
		}
	case reflect.Map:
		if v.Type().Key().Kind() == reflect.String && v.Type().Elem() == reflect.TypeOf([]string{}) {
			v.Set(reflect.ValueOf(map[string][]string{"Authorization": {"Bearer CANARYx9 " + path}, "X-Prompt": {"CANARYx9 prompt"}}).Convert(v.Type()))
		}
	}
}

// Enum fields keep real values: P6 passes these through only when they are a
// known value or a short code.
var keepAccount = map[string]bool{"Provider": true, "AuthType": true, "AuthStatus": true, "WebTier": true, "EgressAssignmentMode": true, "BuildRouteMode": true,
	"LinkedProvider": true, "Mode": true, "Source": true, "Type": true, "Status": true, "Unit": true, "Confidence": true}
var keepRecord = map[string]bool{"Provider": true, "Operation": true, "UsageSource": true, "EgressMode": true, "ModelPublicID": true, "ErrorCode": true, "Source": true}
var keepNode = map[string]bool{"Scope": true, "ProbeStatus": true, "ProbeProvider": true, "Status": true}

type fakeAccounts struct {
	views []accountapp.View
	err   error
	calls int
}

func (f *fakeAccounts) List(_ context.Context, page, pageSize int, search string, _ accountapp.ListFilter) ([]accountapp.View, int64, error) {
	f.calls++
	if f.err != nil {
		return nil, 0, f.err
	}
	if page != 1 || pageSize != MaxAccounts || search != "" {
		return nil, 0, errors.New("unexpected page")
	}
	return f.views, int64(len(f.views)), nil
}

func (f *fakeAccounts) Summary(context.Context) (accountapp.Summary, error) {
	f.calls++
	if f.err != nil {
		return accountapp.Summary{}, f.err
	}
	return accountapp.Summary{Total: 3, Available: 1, Recovering: 1, Attention: 1,
		Providers: map[string]accountapp.ProviderSummary{"grok_web": {Total: 3, Available: 1}},
		Recovery:  accountapp.RecoverySummary{Cooldown: 1}, Issues: accountapp.IssueSummary{Disabled: 1}}, nil
}

type fakeUsage struct{ periods []string }

func (f *fakeUsage) Get(_ context.Context, period, tz string) (dashboardapp.Result, error) {
	f.periods = append(f.periods, period+"|"+tz)
	return dashboardapp.Result{Period: dashboardapp.Period(period), Range: dashboardapp.Range{Start: t0.Add(-24 * time.Hour), End: t0},
		Usage:     dashboarddomain.Usage{Requests: 40, SuccessfulRequests: 38, FailedRequests: 2, FirstTokenSamples: 4, FirstTokenTotalMS: 4000, InputTokens: 1},
		Series:    []dashboardapp.SeriesPoint{{Start: t0.Add(-time.Hour), Requests: 5}},
		Providers: []dashboarddomain.ProviderUsage{{Provider: "grok_web", Requests: 40, SuccessfulRequests: 38}, {Provider: "CANARYx9 provider", Requests: 1}},
		TopModels: []dashboardapp.ModelUsage{{Model: "CANARYx9 model"}}}, nil
}

type fakeRequests struct {
	records []auditdomain.Record
	limits  []int
}

func (f *fakeRequests) List(_ context.Context, page, pageSize int) ([]auditdomain.Record, int64, error) {
	f.limits = append(f.limits, pageSize)
	n := min(pageSize, len(f.records))
	return f.records[:n], int64(len(f.records)), nil
}

type fakeProxies struct{ nodes []egressdomain.PublicNode }

func (f *fakeProxies) List(context.Context, int, int, string, egressapp.ListFilter) ([]egressdomain.PublicNode, int64, error) {
	return f.nodes, int64(len(f.nodes)), nil
}

// canaryWorld is a pool whose every secret-bearing field is a canary.
func canaryWorld() (*fakeAccounts, *fakeUsage, *fakeRequests, *fakeProxies) {
	acct := func(id uint64, mutate func(*accountapp.View)) accountapp.View {
		v := accountapp.View{Credential: accountdomain.Credential{ID: id, Provider: accountdomain.ProviderWeb, AuthStatus: accountdomain.AuthStatusActive,
			WebTier: accountdomain.WebTierSuper, Enabled: true, LinkedAccounts: []accountdomain.LinkedAccount{{ID: 9}}},
			Billing:      &accountdomain.Billing{History: []accountdomain.BillingHistoryEntry{{}}},
			QuotaWindows: []accountdomain.QuotaWindow{{Mode: accountdomain.QuotaModeWebVideo, Remaining: 3, Total: 10, Source: accountdomain.QuotaSourceUpstream, ResetAt: at(time.Hour)}}}
		canaryFill(reflect.ValueOf(&v), "account", keepAccount)
		v.Credential.LastError = "429 Too Many Requests for CANARYx9 user@example.com token sk-live-CANARY"
		if mutate != nil {
			mutate(&v)
		}
		return v
	}
	accounts := &fakeAccounts{views: []accountapp.View{
		acct(1, nil),
		acct(2, func(v *accountapp.View) { v.Credential.CooldownUntil = at(30 * time.Minute) }),
		acct(3, func(v *accountapp.View) { v.Credential.Enabled = false }),
		acct(4, func(v *accountapp.View) { v.Credential.AuthStatus = accountdomain.AuthStatusReauthRequired }),
		acct(5, func(v *accountapp.View) {
			v.Quota.Status = accountapp.QuotaStatusWaitingReset
			v.QuotaWindows[0].Remaining = 0
		}),
	}}
	id := uint64(1)
	first := int64(800)
	rec := auditdomain.Record{Provider: "grok_web", Operation: auditdomain.OperationVideo, ModelPublicID: "grok-imagine-video", ErrorCode: "",
		AccountID: &id, StatusCode: 200, DurationMS: 4200, FirstTokenMS: &first, AttemptCount: 1, CreatedAt: t0,
		Attempts: []auditdomain.Attempt{{}}, EgressMode: auditdomain.EgressModeProxy}
	canaryFill(reflect.ValueOf(&rec), "record", keepRecord)
	bad := rec
	bad.StatusCode, bad.ErrorCode = 429, "CANARYx9 upstream said user@example.com"
	requests := &fakeRequests{records: []auditdomain.Record{rec, bad}}
	node := egressdomain.PublicNode{ID: 7, Scope: egressdomain.ScopeWeb, Enabled: true, ProbeStatus: egressdomain.ProbeStatusHealthy, Health: 0.98, ProbeLatencyMS: 180,
		AssignedAccountCount: 2, AccountCapacity: 5}
	canaryFill(reflect.ValueOf(&node), "node", keepNode)
	node.LastError = "proxy dial tcp CANARYx9 10.0.0.1:8080: connection refused"
	cooling := node
	cooling.ID, cooling.CooldownUntil, cooling.ProbeStatus = 8, at(time.Minute), egressdomain.ProbeStatusUnhealthy
	return accounts, &fakeUsage{}, requests, &fakeProxies{nodes: []egressdomain.PublicNode{node, cooling}}
}

func newCanaryService() *Service {
	a, u, r, p := canaryWorld()
	return New(Options{Accounts: a, Usage: u, Requests: r, Proxies: p, LabelSecret: "label-secret", Now: func() time.Time { return t0 }})
}
