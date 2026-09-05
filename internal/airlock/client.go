package airlock

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/gitremote"
)

// The transport. One place composes a request, one place reads a response, and
// one place decides whether an unanswered call may be sent again — because the
// interesting property of this client is not what it can fetch but what it must
// never do twice with a fresh key.

// maxErrorBody bounds how much of an unparseable error response is retained.
// The contract binds message content, but a body this client failed to decode
// is by definition not the contract's shape, so it is truncated and never
// logged with the request that produced it.
const maxErrorBody = 4 << 10

// maxResponseBody bounds a successful body. An event page is bounded by the
// profile's per-chunk and per-page limits, so a body far past that is a server
// this client should refuse rather than buffer.
const maxResponseBody = 64 << 20

// Timeouts are the client's own clocks. None of them is a run deadline: the
// run's hard and output-stall timeouts are enforced by Airlock precisely so
// both continue while BEN is disconnected (remote.ProcessSpec.Limits), and
// nothing here may reset either.
type Timeouts struct {
	// Request bounds one HTTP round trip that is not a long poll.
	Request time.Duration
	// Poll bounds one long-poll round trip — reading events or waiting for a
	// run — and is therefore expected to be several times Request.
	Poll time.Duration
	// PollWait is how long the server is asked to hold a long poll open. It is
	// sent as `wait_seconds` and must be comfortably under Poll, or the client
	// gives up on a request the server was about to answer.
	PollWait time.Duration
	// Settle bounds waiting for a sandbox to reach a state a run can be
	// dispatched into, or for a deletion's three evidence fields to confirm.
	Settle time.Duration
	// Retries bounds how many times one request is re-sent after a retryable
	// refusal or a transport error. Bounded rather than "until ctx ends" so the
	// operation returns control to its caller; the event pump then applies its
	// longer reconnect policy without making every other call unbounded (#275).
	Retries int
}

// DefaultTimeouts are the values an omitted block resolves to.
var DefaultTimeouts = Timeouts{
	Request:  30 * time.Second,
	Poll:     70 * time.Second,
	PollWait: 30 * time.Second,
	Settle:   5 * time.Minute,
	Retries:  4,
}

func (t Timeouts) withDefaults() Timeouts {
	if t.Request <= 0 {
		t.Request = DefaultTimeouts.Request
	}
	if t.Poll <= 0 {
		t.Poll = DefaultTimeouts.Poll
	}
	if t.PollWait <= 0 {
		t.PollWait = DefaultTimeouts.PollWait
	}
	if t.Settle <= 0 {
		t.Settle = DefaultTimeouts.Settle
	}
	if t.Retries <= 0 {
		t.Retries = DefaultTimeouts.Retries
	}
	return t
}

// client is the authenticated HTTP boundary.
type client struct {
	base     *url.URL
	http     *http.Client
	auth     core.Source
	timeouts Timeouts
	// sleep is the retry delay, injected so a contract test can prove the
	// backoff arithmetic without spending it.
	sleep func(context.Context, time.Duration) error
}

// newClient validates the endpoint and builds the transport.
//
// The base URL is checked with the same helper both Git drivers use
// (gitremote.EmbedsCredential), not a second implementation of the rule. A
// credential in a URL is the failure mode that survives every other precaution:
// it ends up in a log line, a config dump, and a process listing, and this
// package's whole auth story is a bearer token from a credential source.
func newClient(baseURL string, auth core.Source, tlsCfg *tls.Config, timeouts Timeouts, transport http.RoundTripper) (*client, error) {
	raw := strings.TrimSpace(baseURL)
	if raw == "" {
		return nil, fmt.Errorf("%w: base URL is empty", ErrConfig)
	}
	if gitremote.EmbedsCredential(raw) {
		// The value is deliberately absent from the message: it is the thing that
		// must not be logged.
		return nil, fmt.Errorf("%w: base URL embeds credentials; authenticate through an auth source", ErrConfig)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: base URL is not a URL", ErrConfig)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("%w: base URL has no host", ErrConfig)
	}
	switch u.Scheme {
	case "https":
	case "http":
		// Refused rather than warned about. The token is a bearer credential and
		// the process bytes are a run's whole output; there is no deployment in
		// which sending either in the clear is a considered choice, and a warning
		// is a refusal nobody reads.
		return nil, fmt.Errorf("%w: base URL must be https", ErrConfig)
	default:
		return nil, fmt.Errorf("%w: base URL scheme %q is not supported", ErrConfig, u.Scheme)
	}
	if u.RawQuery != "" || u.ForceQuery || strings.Contains(raw, "#") {
		return nil, fmt.Errorf("%w: base URL must not contain a query or fragment", ErrConfig)
	}
	if auth == nil {
		return nil, fmt.Errorf("%w: no auth source", ErrConfig)
	}
	u.Path = strings.TrimSuffix(u.Path, "/")

	timeouts = timeouts.withDefaults()
	rt := transport
	if rt == nil {
		base := http.DefaultTransport.(*http.Transport).Clone()
		base.TLSClientConfig = tlsCfg
		rt = base
	}
	return &client{
		base: u,
		// No client-level Timeout: each call sets its own deadline through the
		// context, and a Timeout here would silently cap a long poll at the
		// short-request bound.
		http:     &http.Client{Transport: rt},
		auth:     auth,
		timeouts: timeouts,
		sleep:    sleepCtx,
	}, nil
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// request is one call. Idem is empty for a naturally idempotent route and the
// client-chosen key otherwise; it is never regenerated between attempts, which
// is the whole of the at-most-once guarantee.
type request struct {
	method string
	path   string
	query  url.Values
	idem   string
	body   any
	// authToken is set for a keyed mutation whose attempts must stay under one
	// credential. It is held in memory, never encoded or logged, and reused
	// across the client's internal retries so an environment rotation cannot
	// move a request between principal scopes. Durable creates/starts also
	// persist principalBinding; a signal compares it with its run's durable
	// binding and then uses this token as the retry-lifetime fence.
	authToken string
	// long marks a route that blocks server-side, selecting the poll deadline.
	long bool
	// out receives the decoded 2xx body when non-nil.
	out any
}

// keyedCredential is the one credential and non-secret principal fence used
// for a keyed side effect. The two must be obtained together: fetching once to
// persist a fence and again to send the request would leave an environment
// rotation between those reads able to cross the very boundary being fenced.
type keyedCredential struct {
	token            string
	principalBinding string
}

func (c *client) fetchCredential(ctx context.Context) (string, error) {
	token, err := c.auth.Fetch(ctx, core.PurposeSubstrate)
	if err != nil {
		// The source's own error already names the authority and never the
		// token (core.CredentialError).
		return "", fmt.Errorf("airlock: obtaining the backend credential: %w", err)
	}
	if token.Value == "" {
		return "", fmt.Errorf("airlock: %w", core.ErrCredentialEmpty)
	}
	return token.Value, nil
}

// keyedAuth snapshots one credential for every attempt of a keyed request.
// Durable creates/starts obtain it before their write-ahead reservation lands;
// signalRun retains it for the request's retry lifetime. A source that declares
// one stable service principal may rotate tokens without invalidating recovery.
// An opaque source makes no such promise, so its exact token is the conservative
// principal fence.
func (c *client) keyedAuth(ctx context.Context) (keyedCredential, error) {
	token, err := c.fetchCredential(ctx)
	if err != nil {
		return keyedCredential{}, err
	}
	descriptor := c.auth.Descriptor()
	domain, material := "source-principal", descriptor.PrincipalKey
	if material == "" {
		domain, material = "opaque-token", token
	}
	sum := sha256.Sum256([]byte("ben.airlock." + domain + "\x00" + descriptor.Kind + "\x00" + material))
	return keyedCredential{
		token:            token,
		principalBinding: domain + "-sha256:" + hex.EncodeToString(sum[:]),
	}, nil
}

// do performs one request with the contract's retry rules applied.
//
// Two things make the retry safe rather than hopeful. Every route in the v2
// contract is either naturally idempotent — it names a target state, or is a
// read — or is keyed, and this client sends the *same* key on every attempt. So
// an unanswered request is always safe to repeat, which is the property that
// lets a transport error be retried at all.
func (c *client) do(ctx context.Context, req request) error {
	var lastErr error
	for attempt := 0; attempt <= c.timeouts.Retries; attempt++ {
		if attempt > 0 {
			delay := retryDelay(lastErr, attempt)
			if err := c.sleep(ctx, delay); err != nil {
				return errors.Join(lastErr, err)
			}
		}
		err := c.attempt(ctx, req)
		if err == nil {
			return nil
		}
		// A context that ended is this daemon's decision, never the backend's.
		// It is returned verbatim so a caller can tell a disconnect from a
		// refusal — remote.Attempt turns exactly that distinction into "the
		// reader stopped" rather than an invented terminal event.
		if ctx.Err() != nil {
			return err
		}
		if !retryable(err) {
			return err
		}
		lastErr = err
	}
	return lastErr
}

// retryDelay is the server's stated delay where it gave one, and a bounded
// exponential backoff otherwise. The server's number wins: `retry_after_seconds`
// is required and non-null for every retryable code in the contract, precisely
// so a conformant client does not hot-loop on `run_conflict`.
func retryDelay(err error, attempt int) time.Duration {
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.RetryAfter > 0 {
		return apiErr.RetryAfter
	}
	delay := time.Duration(1<<uint(attempt-1)) * 250 * time.Millisecond
	if delay > 8*time.Second {
		delay = 8 * time.Second
	}
	return delay
}

func (c *client) attempt(ctx context.Context, req request) error {
	deadline := c.timeouts.Request
	if req.long {
		deadline = c.timeouts.Poll
	}
	ctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()

	var body io.Reader
	if req.body != nil {
		encoded, err := json.Marshal(req.body)
		if err != nil {
			// Unreachable for the request types in this package, and stated
			// rather than assumed: the encoded body is the idempotency
			// fingerprint, so a body that cannot be encoded is a request that
			// must not be sent under a key that will later replay.
			return fmt.Errorf("airlock: encoding the %s request: %w", req.path, err)
		}
		body = bytes.NewReader(encoded)
	}

	endpoint := *c.base
	endpoint.Path = c.base.Path + req.path
	endpoint.RawQuery = req.query.Encode()

	httpReq, err := http.NewRequestWithContext(ctx, req.method, endpoint.String(), body)
	if err != nil {
		return fmt.Errorf("airlock: building the %s request: %w", req.path, err)
	}
	token := req.authToken
	if token == "" {
		var err error
		token, err = c.fetchCredential(ctx)
		if err != nil {
			return err
		}
	}
	httpReq.Header.Set("Authorization", "Bearer "+token)
	httpReq.Header.Set("Accept", "application/json")
	if body != nil {
		httpReq.Header.Set("Content-Type", "application/json")
	}
	if req.idem != "" {
		httpReq.Header.Set("Idempotency-Key", req.idem)
	}

	resp, err := c.http.Do(httpReq)
	if err != nil {
		// url.Error stringifies the whole URL. That is safe here — a base URL
		// carrying userinfo was refused at construction — but the request is
		// still described by path rather than by the composed URL, so a future
		// query parameter cannot leak into a log through this seam.
		return fmt.Errorf("airlock: %s %s: %w", req.method, req.path, redactURLError(err))
	}
	defer resp.Body.Close() //nolint:errcheck // read-only; the decode error is the one that matters

	if resp.StatusCode >= 400 {
		return decodeError(resp)
	}
	if req.out == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBody))
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBody)).Decode(req.out); err != nil {
		return fmt.Errorf("airlock: decoding the %s response: %w", req.path, err)
	}
	return nil
}

// decodeError turns a non-2xx response into an APIError.
//
// A body this client cannot decode is not the contract's shape, so it is
// reported as an internal error — which is retryable, and correctly so: an
// unparseable 500 from a proxy in front of Airlock is exactly the transient
// case, and treating it as permanent would fail a claim over a load balancer.
func decodeError(resp *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	var envelope errorEnvelope
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	decodeErr := decoder.Decode(&envelope)
	var extra any
	if decodeErr == nil {
		switch err := decoder.Decode(&extra); {
		case err == io.EOF:
		case err != nil:
			decodeErr = err
		default:
			decodeErr = errors.New("trailing JSON value")
		}
	}
	if decodeErr != nil || envelope.Error.Code == "" {
		return &APIError{
			Status:  resp.StatusCode,
			Code:    CodeInternal,
			Message: fmt.Sprintf("unparseable error body (%d bytes)", len(raw)),
		}
	}
	e := envelope.Error
	out := &APIError{
		Status:    resp.StatusCode,
		Code:      e.Code,
		Message:   e.Message,
		RequestID: e.RequestID,
	}
	if e.RetryAfterSeconds != nil && *e.RetryAfterSeconds >= 0 {
		out.RetryAfter = time.Duration(*e.RetryAfterSeconds) * time.Second
	}
	if id, ok := e.Details["active_run_id"].(string); ok {
		out.ActiveRunID = id
	}
	if limit := detailSeq(e.Details, "limit_bytes"); limit != nil {
		out.LimitBytes = *limit
	}
	out.ExpectedOffset = detailSeq(e.Details, "expected_offset")
	out.RequestedAfter = detailSeq(e.Details, "requested_after")
	out.OldestAvailableSeq = detailSeq(e.Details, "oldest_available_seq")
	return out
}

// detailSeq lifts a backend sequence out of an error's details, and reports
// absence for anything that is not one.
//
// Strict, because these two numbers are the whole warrant for advancing a cursor
// over events BEN never read (remote.RetentionGap). Parsing the original JSON
// token rather than a float preserves every int64 exactly and rejects a value
// outside that range; rounding or saturating one would manufacture a range out
// of a malformed answer.
func detailSeq(details map[string]any, key string) *int64 {
	raw, ok := details[key].(json.Number)
	if !ok {
		return nil
	}
	seq, err := strconv.ParseInt(raw.String(), 10, 64)
	if err != nil || seq < 0 {
		return nil
	}
	return &seq
}

// redactURLError strips the URL from a transport error, keeping the cause.
//
// Belt and braces over the construction-time refusal: net/http wraps every
// transport failure in a *url.Error whose Error() prints the URL in full, and
// this client's requests are the one place a token could ever be composed into
// one. Removing the URL rather than scrubbing it, because scrubbing rendered
// text cannot be made safe (config.RenderRefusal makes the same call).
func redactURLError(err error) error {
	var urlErr *url.Error
	if !errors.As(err, &urlErr) {
		return err
	}
	return fmt.Errorf("%s: %w", urlErr.Op, urlErr.Err)
}
