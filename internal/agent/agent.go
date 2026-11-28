// Package agent is the agent-side half of the routing contract: ask the
// controller for a route, call the LiteLLM proxy directly with the alias it
// returned, and report the attempt back.
//
// The controller is a decision point, never a data path: the prompt and the
// stream go to the proxy, and only task metadata and a session id ever reach
// the controller. See docs/agent-integration.md (issue #20).
package agent

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gazon1/quota-router/internal/routing"
	"github.com/gazon1/quota-router/internal/settings"
)

// Outcomes the usage endpoint accepts, for the caller that finishes a stream
// itself. Chat only distinguishes the two a complete response can prove.
const (
	OutcomeSuccess                = "SUCCESS"
	OutcomeFailedBeforeFirstToken = "FAILED_BEFORE_FIRST_TOKEN"
	OutcomePartialStream          = "PARTIAL_STREAM"
	OutcomeCancelled              = "CANCELLED"
)

// Provenance values the usage endpoint accepts. Nothing here estimates, so the
// default is Reported.
const (
	ProvenanceReported  = "reported"
	ProvenanceEstimated = "estimated"
)

// Source is the usage-report channel this client reports under.
const Source = "agent.usage_report"

// ErrControllerUnavailable means the controller could not answer at all: no
// connection, or a 5xx. Nothing was routed.
var ErrControllerUnavailable = errors.New("agent: controller unavailable")

// ErrNoRoute means the controller answered and had no eligible model. It is a
// policy answer, so it is never replaced by the configured manual route.
var ErrNoRoute = errors.New("agent: no eligible route")

// maxBody bounds one response body on either hop. A chat body that exceeds it is
// an error, not a silently truncated body.
const maxBody = 8 << 20

// defaultTimeout bounds one hop when HTTP is nil. A streaming agent owns its own
// transport: a stream can outlive any fixed cap.
const defaultTimeout = 60 * time.Second

const requestIDHeader = "X-Request-Id"

// Client is one agent's routing client. Both hops go over HTTP itself; there is
// no shared state between an agent process and this struct.
type Client struct {
	// ControllerURL is the controller root, for example http://127.0.0.1:8080.
	ControllerURL string
	// ProxyURL is the LiteLLM root, for example http://127.0.0.1:4000.
	ProxyURL string
	// Token is the controller bearer token
	// (QUOTA_ROUTER_AUTH__TOKEN). Empty refuses nothing here: the controller
	// answers 401, which this client surfaces rather than falling back.
	Token settings.Secret
	// ProxyKey is the proxy's master key, sent as the bearer token on the model
	// call. Empty sends no Authorization header.
	ProxyKey settings.Secret
	// Manual is the explicit route an operator pins for controller outages. The
	// zero value fails closed.
	Manual ManualRoute
	// HTTP is the transport both hops use. Nil means a bounded default.
	HTTP *http.Client
}

// ManualRoute is an operator's explicit stand-in for the controller. It is used
// only when the controller cannot answer, never to override an answer it gave,
// and the usage rows it produces carry no decision id because no decision
// existed.
type ManualRoute struct {
	Alias string
	// Provider is required with Alias: a usage report names the provider, and
	// the agent cannot derive it from an alias.
	Provider string
}

// Routed is the route one agent request uses, plus the ids that correlate the
// decision, the model call and the usage report.
type Routed struct {
	// Alias is the catalogue alias to call the proxy with.
	Alias string
	// Provider is the provider that alias belongs to.
	Provider string
	// DecisionID is the controller's routing decision. Empty for a manual route.
	DecisionID string
	// RequestID is minted here, sent to the controller and the proxy as
	// X-Request-Id, and reused as the usage report's request_id.
	RequestID string
	// Manual reports that the controller could not answer and Manual was used.
	Manual bool
}

// Usage is what the proxy reported for one attempt. A nil count was not
// reported; it is not zero.
type Usage struct {
	InputTokens  *int
	OutputTokens *int
}

// Reply is one non-streaming proxy response, unchanged.
type Reply struct {
	Status int
	Body   []byte
	// Usage is nil when the proxy reported no usage object.
	Usage *Usage
	// Outcome is SUCCESS for a 2xx response and FAILED_BEFORE_FIRST_TOKEN
	// otherwise: a complete response cannot end in a partial stream.
	Outcome string
}

// Attempt is one finished attempt as the usage endpoint records it.
type Attempt struct {
	// Number is the 1-based attempt within the request; 0 means the first.
	Number int
	// Outcome is one of the Outcome constants. Empty is stored as UNKNOWN.
	Outcome string
	// Counts the provider reported. Nil stays missing, never zero.
	InputTokens  *int
	OutputTokens *int
	// Provenance defaults to ProvenanceReported. Set ProvenanceEstimated only
	// for a count this installation derived, for example by counting tokens on a
	// stream that returned no usage object.
	Provenance string
}

// Attempt turns a complete reply into the attempt a report carries.
func (r Reply) Attempt(number int) Attempt {
	a := Attempt{Number: number, Outcome: r.Outcome}
	if r.Usage != nil {
		a.InputTokens = r.Usage.InputTokens
		a.OutputTokens = r.Usage.OutputTokens
	}
	return a
}

// Route asks the controller for a route. req is the versioned contract, sent
// unchanged: the client adds an id, never task metadata the caller did not set.
//
// When the controller cannot answer and Manual is configured, the manual route
// is returned with Manual set and a nil error. Every other failure is surfaced.
func (c *Client) Route(ctx context.Context, req routing.RouteRequest) (Routed, error) {
	if strings.TrimSpace(c.ControllerURL) == "" {
		return Routed{}, errors.New("agent: ControllerURL is not configured")
	}
	if c.Manual.Alias != "" && c.Manual.Provider == "" {
		return Routed{}, errors.New("agent: Manual.Alias needs Manual.Provider; a usage report names the provider")
	}
	if err := req.Validate(); err != nil {
		return Routed{}, fmt.Errorf("agent: %w", err)
	}
	body, err := json.Marshal(req)
	if err != nil {
		return Routed{}, fmt.Errorf("agent: encode route request: %w", err)
	}
	id := newRequestID()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url(c.ControllerURL, "/v1/route"), bytes.NewReader(body))
	if err != nil {
		return Routed{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set(requestIDHeader, id)
	c.authorize(httpReq, c.Token)

	resp, err := c.httpClient().Do(httpReq)
	if err != nil {
		return c.unavailable(id, err)
	}
	defer resp.Body.Close()
	payload, err := readBody(resp.Body)
	if err != nil {
		return Routed{}, fmt.Errorf("agent: read route response: %w", err)
	}

	switch {
	case resp.StatusCode == http.StatusOK:
		var out struct {
			Decision routing.Decision `json:"decision"`
		}
		if err := json.Unmarshal(payload, &out); err != nil {
			return Routed{}, fmt.Errorf("agent: decode route decision: %w", err)
		}
		d := out.Decision
		if d.DecisionID == "" {
			return Routed{}, errors.New("agent: route response carried no decision")
		}
		if d.SelectedAlias == "" {
			return Routed{}, fmt.Errorf("%w: %s", ErrNoRoute, strings.Join(d.Reasons, "; "))
		}
		// A route without a provider is unusable: the usage report requires one
		// and the agent cannot derive it from an alias. Fail here, where the
		// cause is visible, rather than at report time.
		if d.SelectedProvider == "" {
			return Routed{}, fmt.Errorf("agent: route response selected %q without a provider", d.SelectedAlias)
		}
		return Routed{
			Alias:      d.SelectedAlias,
			Provider:   d.SelectedProvider,
			DecisionID: d.DecisionID,
			RequestID:  id,
		}, nil
	case resp.StatusCode == http.StatusConflict:
		return Routed{}, fmt.Errorf("%w: %s", ErrNoRoute, errorDetail(payload))
	case resp.StatusCode >= 500:
		return c.unavailable(id, fmt.Errorf("status %d: %s", resp.StatusCode, errorDetail(payload)))
	default:
		return Routed{}, fmt.Errorf("agent: routing request rejected with status %d: %s", resp.StatusCode, errorDetail(payload))
	}
}

// ProxyRequest builds the model call for a routed decision: the caller's body
// with "model" replaced by the routed alias, addressed to the proxy. The caller
// owns the transport, so a streaming agent reads the SSE body as it arrives and
// the controller is never in the data path.
func (c *Client) ProxyRequest(ctx context.Context, routed Routed, body []byte) (*http.Request, error) {
	if strings.TrimSpace(c.ProxyURL) == "" {
		return nil, errors.New("agent: ProxyURL is not configured")
	}
	if routed.Alias == "" {
		return nil, errors.New("agent: routed alias is empty")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, fmt.Errorf("agent: chat body must be a JSON object: %w", err)
	}
	// "null" unmarshals into a nil map with no error, so the check cannot be the
	// unmarshal alone: indexing it below would panic.
	if fields == nil {
		return nil, errors.New("agent: chat body must be a JSON object, got null")
	}
	// Rewriting model is the point: the proxy is called with the alias the
	// controller returned, not with one the caller guessed.
	alias, err := json.Marshal(routed.Alias)
	if err != nil {
		return nil, fmt.Errorf("agent: encode alias: %w", err)
	}
	fields["model"] = alias
	out, err := json.Marshal(fields)
	if err != nil {
		return nil, fmt.Errorf("agent: encode chat body: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url(c.ProxyURL, "/v1/chat/completions"), bytes.NewReader(out))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if routed.RequestID != "" {
		req.Header.Set(requestIDHeader, routed.RequestID)
	}
	c.authorize(req, c.ProxyKey)
	return req, nil
}

// Chat performs one non-streaming model call through the proxy. A streaming
// caller uses ProxyRequest instead and reports its own Attempt.
func (c *Client) Chat(ctx context.Context, routed Routed, body []byte) (Reply, error) {
	req, err := c.ProxyRequest(ctx, routed, body)
	if err != nil {
		return Reply{}, err
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return Reply{}, fmt.Errorf("agent: call proxy: %w", err)
	}
	defer resp.Body.Close()
	payload, err := readBody(resp.Body)
	if err != nil {
		return Reply{}, fmt.Errorf("agent: read proxy response: %w", err)
	}
	outcome := OutcomeFailedBeforeFirstToken
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		outcome = OutcomeSuccess
	}
	return Reply{Status: resp.StatusCode, Body: payload, Usage: parseUsage(payload), Outcome: outcome}, nil
}

// Report sends one finished attempt to the controller, correlated by the ids
// Route minted and the decision it received. It is safe to repeat: the endpoint
// deduplicates on (request_id, attempt).
func (c *Client) Report(ctx context.Context, routed Routed, attempt Attempt) error {
	if strings.TrimSpace(c.ControllerURL) == "" {
		return errors.New("agent: ControllerURL is not configured")
	}
	if routed.RequestID == "" {
		return errors.New("agent: routed request id is empty")
	}
	if routed.Provider == "" {
		return errors.New("agent: routed provider is empty; a usage record requires it")
	}
	if attempt.Provenance == "" {
		attempt.Provenance = ProvenanceReported
	}
	body, err := json.Marshal(usageBody{
		RequestID:    routed.RequestID,
		Attempt:      attempt.Number,
		Provider:     routed.Provider,
		Model:        routed.Alias,
		InputTokens:  attempt.InputTokens,
		OutputTokens: attempt.OutputTokens,
		Provenance:   attempt.Provenance,
		Source:       Source,
		Outcome:      attempt.Outcome,
		DecisionID:   routed.DecisionID,
	})
	if err != nil {
		return fmt.Errorf("agent: encode usage report: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url(c.ControllerURL, "/v1/usage"), bytes.NewReader(body))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set(requestIDHeader, routed.RequestID)
	c.authorize(httpReq, c.Token)

	resp, err := c.httpClient().Do(httpReq)
	if err != nil {
		return fmt.Errorf("agent: report usage: %w", err)
	}
	defer resp.Body.Close()
	payload, err := readBody(resp.Body)
	if err != nil {
		return fmt.Errorf("agent: read usage response: %w", err)
	}
	if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("agent: usage report rejected with status %d: %s", resp.StatusCode, errorDetail(payload))
	}
	return nil
}

// usageBody is this client's view of the POST /v1/usage contract
// (docs/request-usage.md). The endpoint rejects unknown fields, so it carries no
// content beyond these.
type usageBody struct {
	RequestID    string `json:"request_id"`
	Attempt      int    `json:"attempt,omitempty"`
	Provider     string `json:"provider"`
	Model        string `json:"model,omitempty"`
	InputTokens  *int   `json:"input_tokens,omitempty"`
	OutputTokens *int   `json:"output_tokens,omitempty"`
	Provenance   string `json:"provenance"`
	Source       string `json:"source"`
	Outcome      string `json:"outcome,omitempty"`
	DecisionID   string `json:"decision_id,omitempty"`
}

func (c *Client) unavailable(id string, cause error) (Routed, error) {
	// A cancelled or expired caller is not an outage. Substituting a manual route
	// here would let an abandoned request continue down another path, so the
	// caller's own error is returned unchanged and errors.Is still matches it.
	if errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded) {
		return Routed{}, fmt.Errorf("agent: %w", cause)
	}
	if c.Manual.Alias == "" {
		return Routed{}, fmt.Errorf("%w: %w", ErrControllerUnavailable, cause)
	}
	return Routed{Alias: c.Manual.Alias, Provider: c.Manual.Provider, RequestID: id, Manual: true}, nil
}

// defaultHTTP is shared so a client with no HTTP set still reuses connections.
var defaultHTTP = &http.Client{Timeout: defaultTimeout}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return defaultHTTP
}

func (c *Client) authorize(req *http.Request, secret settings.Secret) {
	if secret != "" {
		req.Header.Set("Authorization", "Bearer "+string(secret))
	}
}

func (c *Client) url(base, path string) string {
	return strings.TrimRight(base, "/") + path
}

// ParseUsage reads the token counts from a proxy response body, for a streaming
// caller that reassembles the final chunk itself. Nil means the response carried
// no usage object; a count inside it may still be nil, and a reported zero is a
// value.
func ParseUsage(payload []byte) *Usage { return parseUsage(payload) }

func parseUsage(payload []byte) *Usage {
	var out struct {
		Usage *struct {
			PromptTokens     *int `json:"prompt_tokens"`
			CompletionTokens *int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(payload, &out); err != nil || out.Usage == nil {
		return nil
	}
	return &Usage{InputTokens: out.Usage.PromptTokens, OutputTokens: out.Usage.CompletionTokens}
}

// errorDetail renders the controller's error envelope for a message. A body
// that is not that envelope is described, never echoed: an unrecognised
// response has no guarantee of being credential-free.
func errorDetail(payload []byte) string {
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(payload, &env); err != nil || env.Error.Code == "" {
		return "unrecognised error response"
	}
	if env.Error.Message == "" {
		return env.Error.Code
	}
	return env.Error.Code + ": " + env.Error.Message
}

func readBody(r io.Reader) ([]byte, error) {
	payload, err := io.ReadAll(io.LimitReader(r, maxBody+1))
	if err != nil {
		return nil, err
	}
	if len(payload) > maxBody {
		return nil, fmt.Errorf("response exceeds %d bytes", maxBody)
	}
	return payload, nil
}

func newRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}
