package runtime

import (
	"bytes"
	"context"
	secure "crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type Client struct {
	base    *url.URL
	http    *http.Client
	bearer  string
	headers http.Header
	retries int
	owned   bool
	errors  map[string]func(*APIError) error
	tokens  *TokenManager
}
type Option func(*Client) error

func WithBaseURL(v string) Option {
	return func(c *Client) error {
		u, e := url.Parse(v)
		if e != nil {
			return e
		}
		if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return fmt.Errorf("invalid base URL")
		}
		c.base = u
		return nil
	}
}
func WithBearerToken(v string) Option { return func(c *Client) error { c.bearer = v; return nil } }
func WithHTTPClient(v *http.Client) Option {
	return func(c *Client) error {
		if v == nil {
			return fmt.Errorf("nil HTTP client")
		}
		c.http = v
		c.owned = false
		return nil
	}
}
func WithMaxRetries(v int) Option {
	return func(c *Client) error {
		if v < 0 || v > 10 {
			return fmt.Errorf("max retries must be 0..10")
		}
		c.retries = v
		return nil
	}
}
func WithTokenManager(m *TokenManager) Option {
	return func(c *Client) error {
		if m == nil {
			return fmt.Errorf("nil token manager")
		}
		c.tokens = m
		return nil
	}
}
func WithErrorFactories(factories map[string]func(*APIError) error) Option {
	return func(c *Client) error {
		c.errors = make(map[string]func(*APIError) error, len(factories))
		for k, v := range factories {
			c.errors[k] = v
		}
		return nil
	}
}
func WithAPIKey(name, value string) Option {
	return func(c *Client) error {
		if name == "" {
			return fmt.Errorf("empty API key header")
		}
		c.headers.Set(name, value)
		return nil
	}
}
func NewClient(opts ...Option) (*Client, error) {
	transport := http.DefaultTransport
	owned := false
	if standard, ok := transport.(*http.Transport); ok {
		transport = standard.Clone()
		owned = true
	}
	c := &Client{http: &http.Client{Timeout: 60 * time.Second, Transport: transport}, headers: make(http.Header), retries: 2, owned: owned}
	if e := WithBaseURL("https://api.appforges.com")(c); e != nil {
		return nil, e
	}
	for _, o := range opts {
		if err := o(c); err != nil {
			_ = c.Close()
			return nil, err
		}
	}
	return c, nil
}
func (c *Client) Close() error {
	if c.owned {
		c.http.CloseIdleConnections()
	}
	return nil
}

type Request struct {
	Method, Path     string
	PathParams       map[string]string
	Query            url.Values
	Headers          http.Header
	Body             any
	Idempotent       bool
	Fields           []Field
	NullableResponse bool
}
type RequestOption func(*Request)

func WithHeader(name, value string) RequestOption {
	return func(r *Request) {
		if r.Headers == nil {
			r.Headers = make(http.Header)
		}
		r.Headers.Set(name, value)
	}
}
func WithIdempotencyKey(value string) RequestOption { return WithHeader("x-idempotency-key", value) }

type APIError struct {
	StatusCode                         int
	Reason, Domain, RequestID, TraceID string
	Metadata                           map[string]any
	RetryAfter                         time.Duration
	Message                            string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("AppForges HTTP %d %s: %s", e.StatusCode, e.Reason, e.Message)
}

type RateLimitExceededError struct{ *APIError }

func (e *RateLimitExceededError) Unwrap() error { return e.APIError }
func retryAfter(v string) time.Duration {
	if n, e := strconv.Atoi(strings.TrimSpace(v)); e == nil && n >= 0 {
		return time.Duration(n) * time.Second
	}
	if t, e := http.ParseTime(v); e == nil {
		return max(time.Until(t), 0)
	}
	return 0
}
func apiError(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var payload map[string]any
	_ = json.Unmarshal(b, &payload)
	status := payload
	if nested, ok := payload["error"].(map[string]any); ok {
		status = nested
	}
	info := status
	switch details := status["details"].(type) {
	case map[string]any:
		info = details
	case []any:
		for _, item := range details {
			if v, ok := item.(map[string]any); ok {
				if _, found := v["reason"]; found {
					info = v
					break
				}
			}
		}
	}
	str := func(m map[string]any, key string) string { value, _ := m[key].(string); return value }
	e := &APIError{StatusCode: resp.StatusCode, Reason: str(info, "reason"), Domain: str(info, "domain"), Message: str(status, "message"), RequestID: resp.Header.Get("X-Request-Id"), TraceID: resp.Header.Get("X-Trace-Id"), RetryAfter: retryAfter(resp.Header.Get("Retry-After"))}
	if e.Reason == "" {
		e.Reason = str(status, "reason")
		if e.Reason == "" {
			e.Reason = str(status, "code")
		}
	}
	e.Metadata, _ = info["metadata"].(map[string]any)
	if e.Metadata == nil && info != nil {
		e.Metadata = info
	}
	if e.TraceID == "" {
		if p := strings.Split(resp.Header.Get("Traceparent"), "-"); len(p) == 4 {
			e.TraceID = p[1]
		}
	}
	if resp.StatusCode == 429 {
		return &RateLimitExceededError{e}
	}
	return e
}
func (c *Client) send(ctx context.Context, r Request, opts ...RequestOption) (*http.Response, error) {
	if len(r.Fields) > 0 {
		if _, err := MarshalModel(r.Fields...); err != nil {
			return nil, err
		}
	}
	if r.Headers == nil {
		r.Headers = make(http.Header)
	} else {
		r.Headers = r.Headers.Clone()
	}
	for _, o := range opts {
		o(&r)
	}
	p := r.Path
	for k, v := range r.PathParams {
		if v == "" {
			return nil, fmt.Errorf("empty path parameter %s", k)
		}
		p = strings.ReplaceAll(p, "{"+k+"}", url.PathEscape(v))
	}
	if strings.Contains(p, "{") {
		return nil, fmt.Errorf("unresolved path parameter")
	}
	if !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") {
		return nil, fmt.Errorf("invalid request path")
	}
	u := *c.base
	u.Path = ""
	u.RawPath = ""
	target := strings.TrimSuffix(u.String(), "/") + strings.TrimSuffix(c.base.EscapedPath(), "/") + p
	if len(r.Query) > 0 {
		target += "?" + r.Query.Encode()
	}
	var body []byte
	var err error
	if optional, ok := r.Body.(presence); ok && !optional.IsSet() {
		r.Body = nil
	}
	if !nilValue(r.Body) {
		body, err = json.Marshal(r.Body)
		if err != nil {
			return nil, err
		}
	}
	readSafe := r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions
	if r.Idempotent && !readSafe && r.Headers.Get("x-idempotency-key") == "" {
		key := make([]byte, 16)
		if _, err := secure.Read(key); err != nil {
			return nil, err
		}
		key[6] = (key[6] & 0x0f) | 0x40
		key[8] = (key[8] & 0x3f) | 0x80
		r.Headers.Set("x-idempotency-key", fmt.Sprintf("%x-%x-%x-%x-%x", key[:4], key[4:6], key[6:8], key[8:10], key[10:]))
	}
	safe := readSafe || r.Headers.Get("x-idempotency-key") != ""
	for attempt := 0; ; attempt++ {
		req, e := http.NewRequestWithContext(ctx, r.Method, target, bytes.NewReader(body))
		if e != nil {
			return nil, e
		}
		req.Header = c.headers.Clone()
		for k, v := range r.Headers {
			req.Header[k] = v
		}
		bearer := c.bearer
		if c.tokens != nil {
			bearer, e = c.tokens.Token(ctx)
			if e != nil {
				return nil, e
			}
		}
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		if req.Header.Get("Accept") == "" {
			req.Header.Set("Accept", "application/json")
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, e := c.http.Do(req)
		if ctx.Err() != nil {
			if resp != nil {
				_ = resp.Body.Close()
			}
			return nil, ctx.Err()
		}
		retry := safe && attempt < c.retries && (e != nil || (resp.StatusCode == 429 || resp.StatusCode == 408 || resp.StatusCode >= 500))
		if !retry {
			if e != nil {
				return nil, e
			}
			if resp.StatusCode >= 400 {
				err = apiError(resp)
				var api *APIError
				if errors.As(err, &api) {
					if factory := c.errors[api.Reason]; factory != nil {
						err = factory(api)
					}
				}
				_ = resp.Body.Close()
				return nil, err
			}
			return resp, nil
		}
		delay := time.Duration(250*(1<<attempt))*time.Millisecond + time.Duration(rand.IntN(100))*time.Millisecond
		if resp != nil {
			if d := retryAfter(resp.Header.Get("Retry-After")); d > 0 {
				delay = d
			}
			_ = resp.Body.Close()
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}
func Execute[T any](ctx context.Context, c *Client, r Request, opts ...RequestOption) (T, error) {
	var value T
	resp, err := c.send(ctx, r, opts...)
	if err != nil {
		return value, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == 204 {
		return value, nil
	}
	decoder := json.NewDecoder(resp.Body)
	if err = decoder.Decode(&value); err != nil {
		return value, err
	}
	if nilValue(value) && !r.NullableResponse {
		return value, fmt.Errorf("response null is not allowed")
	}
	var extra any
	if e := decoder.Decode(&extra); !errors.Is(e, io.EOF) {
		if e == nil {
			return value, fmt.Errorf("multiple JSON values")
		}
		return value, e
	}
	return value, nil
}
