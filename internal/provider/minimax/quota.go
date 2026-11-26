// Package minimax implements the MiniMax Token Plan subscription quota adapter.
//
// The quota surface is the documented Global Token Plan endpoint
// GET https://www.minimax.io/v1/token_plan/remains, authenticated with a Token
// Plan subscription key (sk-cp-..., "M Plan" in the console). It is a
// Controller-side API and is deliberately a different host from inference:
// OpenAI-compatible inference lives on api.minimax.io/v1 and Anthropic-compatible
// inference on api.minimax.io/anthropic, and neither may be configured here. See
// docs/minimax-quota-adapter.md for the measured response semantics and the
// limitations of mapping them.
package minimax

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gazon1/quota-router/internal/config"
	"github.com/gazon1/quota-router/internal/quota"
)

const (
	// Provider is the name these observations are reported under.
	Provider = "minimax"
	// Source names the documented endpoint these values come from. It is never an
	// inference endpoint, because a successful completion says nothing about
	// subscription quota.
	Source = "GET https://www.minimax.io/v1/token_plan/remains"

	// maxBody bounds the response this adapter will accept. The measured body is
	// about 1 KiB; anything larger is not the documented response, and the read is
	// capped one byte past this so an oversized answer is detected, not truncated.
	maxBody = 1 << 20

	// quotaHost is the only origin permitted to receive the Token Plan
	// credential. Loopback is the one other accepted origin, so a test or a local
	// interposer needs no production backdoor.
	quotaHost = "www.minimax.io"
)

var (
	// errUnsafeOrigin means the configured endpoint would carry the credential to
	// an origin that is not the documented quota host.
	errUnsafeOrigin = errors.New("quota endpoint is not the documented Global Token Plan host")
	// errOversizedBody means the answer was larger than the documented response.
	errOversizedBody = errors.New("response body exceeds the documented quota response size")
)

// Adapter polls the Token Plan quota endpoint. One attempt produces two
// independent signals: subscription quota from the documented fields, and
// provider health from the HTTP/business status. Neither is derived from the
// other.
type Adapter struct {
	// Client performs the request; nil means http.DefaultClient. The attempt is
	// bounded either way: the context deadline applies, and Timeout caps the
	// attempt even when the caller passes a deadline-less context.
	Client *http.Client
	// Timeout bounds this attempt; zero uses polling.request_timeout (see New).
	Timeout time.Duration
	// URL is the quota endpoint, for example
	// https://www.minimax.io/v1/token_plan/remains. Build it from the provider's
	// base URL, never from LiteLLM's api_base.
	URL string
	// Key is the Token Plan subscription key. config.Secret keeps it out of logs
	// and formatted output.
	Key config.Secret
	// LowFraction is the remaining fraction at or below which a window counts as
	// scarce; zero disables the classification.
	LowFraction float64

	// mu guards lastGood and haveGood, since a poller and an on-demand refresh may
	// overlap.
	mu       sync.Mutex
	lastGood quota.Snapshot
	haveGood bool
}

// path is the documented quota route, appended to the provider's base_url.
const path = "/token_plan/remains"

// New builds the adapter from the configured provider. The quota URL comes from
// providers.<name>.base_url (https://www.minimax.io/v1), never from LiteLLM's
// api_base: MiniMax serves quota and inference from different hosts, so an
// adapter pointed at the inference host would poll nothing and report UNKNOWN
// forever.
//
// A base URL that would not carry the Token Plan key to the documented quota
// host is an error here rather than a poll that leaks the credential somewhere
// else, so a misconfiguration fails at startup the way config validation does.
func New(p config.Provider, lowFraction float64, timeout time.Duration) (*Adapter, error) {
	raw := strings.TrimSuffix(p.BaseURL, "/") + path
	if err := checkOrigin(raw); err != nil {
		return nil, fmt.Errorf("providers.%s.base_url: %w", Provider, err)
	}
	return &Adapter{
		URL:         raw,
		Key:         p.APIKey,
		LowFraction: lowFraction,
		Timeout:     timeout,
	}, nil
}

// checkOrigin reports whether raw may receive the Token Plan credential: the
// documented Global quota host on https at the documented path, or a loopback
// address for tests and local interposition. The URL is never echoed, because a
// base URL can carry userinfo.
func checkOrigin(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.User != nil {
		return errUnsafeOrigin
	}
	if loopback(u.Hostname()) {
		return nil
	}
	if u.Scheme != "https" || u.Host != quotaHost || u.Path != "/v1"+path {
		return errUnsafeOrigin
	}
	return nil
}

// loopback reports whether host is a local address.
func loopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Fetch performs one bounded poll of the documented quota endpoint. When this
// attempt establishes no usable quota, the last observation that did is returned
// marked Stale instead, as diagnostics only: StateAt reads it as UNKNOWN, so no
// consumer can route on it. The attempt's own reason still travels in the health
// report.
func (a *Adapter) Fetch(ctx context.Context) (quota.Snapshot, quota.ProviderState) {
	snap, state := a.observe(ctx)

	a.mu.Lock()
	defer a.mu.Unlock()
	if snap.Confidence != quota.ConfidenceNone {
		// An attempt that started earlier but finished later must not replace, or
		// be returned instead of, a newer observation: the caller would otherwise
		// receive an older quota reading as fresh and regress with it. Handing back
		// the retained newer snapshot keeps Cache and callers agreeing; health still
		// describes the attempt that just completed.
		if a.haveGood && snap.ObservedAt.Before(a.lastGood.ObservedAt) {
			return a.lastGood, state
		}
		a.lastGood, a.haveGood = snap, true
		return snap, state
	}
	if !a.haveGood {
		return snap, state
	}
	stale := a.lastGood
	stale.Stale = true
	return stale, state
}

// observe performs the request and reports what it can establish.
func (a *Adapter) observe(ctx context.Context) (quota.Snapshot, quota.ProviderState) {
	at := time.Now().UTC()
	status, body, err := a.get(ctx)
	if err != nil {
		// A refused request or an oversized answer is not a transport failure and
		// not a quota claim: nothing useful came back either way.
		if errors.Is(err, errUnsafeOrigin) || errors.Is(err, errOversizedBody) {
			return quota.Failed(Provider, Source, at, quota.MalformedResponse),
				quota.NewProviderState(Provider, quota.Degraded, quota.ReasonServerError, at)
		}
		snap := quota.Failed(Provider, Source, at, quota.TransientFetchFailure)
		return snap, quota.NewProviderState(Provider, quota.Unavailable, transportReason(err), at)
	}
	if status != http.StatusOK {
		reason, health, why := statusFailure(status)
		return quota.Failed(Provider, Source, at, reason), quota.NewProviderState(Provider, health, why, at)
	}

	snap := Parse(body, at, a.LowFraction)
	state, reason := snap.State()
	if state != quota.QuotaUnknown {
		return snap, quota.NewProviderState(Provider, quota.Healthy, quota.ReasonNone, at)
	}
	// A 200 that yields no usable quota is a degraded provider, and the quota
	// reason says which kind: a rejected credential or an unusable body.
	switch reason {
	case quota.AuthFailure:
		return snap, quota.NewProviderState(Provider, quota.Unavailable, quota.ReasonAuthFailed, at)
	case quota.TransientFetchFailure:
		return snap, quota.NewProviderState(Provider, quota.Degraded, quota.ReasonRateLimited, at)
	}
	return snap, quota.NewProviderState(Provider, quota.Degraded, quota.ReasonServerError, at)
}

// get performs the request and reads a bounded body. A non-positive Timeout is
// still bounded: an unbounded attempt is not a context-aware adapter.
func (a *Adapter) get(ctx context.Context) (int, []byte, error) {
	timeout := a.Timeout
	if timeout <= 0 {
		timeout = time.Minute
	}
	// Checked before the credential is attached, so a URL set directly on an
	// Adapter cannot send the key to another origin either.
	if err := checkOrigin(a.URL); err != nil {
		return 0, nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.URL, nil)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+string(a.Key))
	client := a.Client
	if client == nil {
		client = http.DefaultClient
	}
	res, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, maxBody+1))
	if err != nil {
		return 0, nil, err
	}
	if len(body) > maxBody {
		return 0, nil, errOversizedBody
	}
	return res.StatusCode, body, nil
}

// Parse maps one 200 response body into a quota snapshot. The endpoint answers
// 200 even when it rejects the credential, so the business status in base_resp,
// never the HTTP status alone, decides whether the body is a quota report.
func Parse(body []byte, at time.Time, lowFraction float64) quota.Snapshot {
	var r remains
	if err := json.Unmarshal(body, &r); err != nil {
		return quota.Failed(Provider, Source, at, quota.MalformedResponse)
	}
	if reason := r.businessReason(); reason != "" {
		return quota.Failed(Provider, Source, at, reason)
	}
	snap := quota.Observed(Provider, Source, at, r.windows(), lowFraction)
	// Provenance records which raw fields each normalized value came from, so a
	// later schema drift is visible instead of silently guessed at.
	snap.Provenance = map[string]string{
		"remaining": "current_interval_remaining_percent, current_weekly_remaining_percent",
		"resets_at": "end_time, weekly_end_time",
	}
	return snap
}

// remains is the documented response. Only fields this adapter maps are decoded;
// the counts and the status enum are deliberately absent because they are not a
// quota basis (see docs/minimax-quota-adapter.md).
type remains struct {
	ModelRemains []modelRemains `json:"model_remains"`
	BaseResp     baseResp       `json:"base_resp"`
}

type modelRemains struct {
	ModelName     string   `json:"model_name"`
	EndTime       *int64   `json:"end_time"`
	WeeklyEndTime *int64   `json:"weekly_end_time"`
	IntervalPct   *float64 `json:"current_interval_remaining_percent"`
	WeeklyPct     *float64 `json:"current_weekly_remaining_percent"`
}

type baseResp struct {
	StatusCode *int `json:"status_code"`
}

// businessReason reports why a body carries no quota report, or "" when it does.
// A missing base_resp is not the documented success either: a 200 whose shape
// drifted cannot be read as quota.
func (r remains) businessReason() quota.UnknownReason {
	if r.BaseResp.StatusCode == nil {
		return quota.MalformedResponse
	}
	switch *r.BaseResp.StatusCode {
	case 0:
		if len(r.ModelRemains) == 0 {
			return quota.MalformedResponse
		}
		return ""
	case 1004, 2049, 2057:
		// login fail / invalid API key / this is not a Token Plan key.
		return quota.AuthFailure
	case 1002, 2062:
		// rate limit reached; also documented as "no active M Plan subscription"
		// when querying usage, so it is a transient condition, not a quota claim.
		return quota.TransientFetchFailure
	}
	return quota.MalformedResponse
}

// windows maps every reported model entry into its rolling 5-hour and weekly
// window. Each entry is a separate quota bucket, so all of them are reported and
// the worst one decides the state, exactly as Derive does.
func (r remains) windows() []quota.Window {
	out := make([]quota.Window, 0, 2*len(r.ModelRemains))
	for _, m := range r.ModelRemains {
		out = append(out,
			m.window(m.ModelName+"-5h", m.IntervalPct, m.EndTime),
			m.window(m.ModelName+"-weekly", m.WeeklyPct, m.WeeklyEndTime))
	}
	return out
}

// window maps one reported percentage into a quota window. The percentage is the
// only quota figure the response states, so the window is read on the percentage
// scale it is reported in: 100% of the window is the limit, and the reported
// value is what remains. A percentage that is absent or outside 0..100 (NaN
// included) stays unreported rather than becoming zero, which makes the whole
// snapshot UNKNOWN instead of a quota claim.
func (m modelRemains) window(name string, percent *float64, end *int64) quota.Window {
	w := quota.Window{Name: name}
	if percent != nil && *percent >= 0 && *percent <= 100 {
		limit, remaining := 100.0, *percent
		w.Limit, w.Remaining = &limit, &remaining
	}
	if end != nil && *end > 0 {
		// Reported in epoch milliseconds.
		w.ResetsAt = time.UnixMilli(*end)
	}
	return w
}

// statusFailure maps a non-200 answer to the two closed vocabularies.
func statusFailure(status int) (quota.UnknownReason, quota.Health, quota.HealthReason) {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return quota.AuthFailure, quota.Unavailable, quota.ReasonAuthFailed
	case http.StatusTooManyRequests:
		return quota.TransientFetchFailure, quota.Degraded, quota.ReasonRateLimited
	}
	return quota.TransientFetchFailure, quota.Unavailable, quota.ReasonServerError
}

// transportReason distinguishes a deadline from an unreachable host. The quota
// reason is the same for both, because neither establishes quota; only health
// tells an operator which one it was.
func transportReason(err error) quota.HealthReason {
	if errors.Is(err, context.DeadlineExceeded) {
		return quota.ReasonTimeout
	}
	var timeout net.Error
	if errors.As(err, &timeout) && timeout.Timeout() {
		return quota.ReasonTimeout
	}
	return quota.ReasonUnreachable
}
