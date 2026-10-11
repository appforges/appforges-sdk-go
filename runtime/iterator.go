package runtime

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"net/http"
	"strings"
)

// Page holds an extracted page without imposing a generated response shape.
type Page[T any] struct {
	Items     []T
	NextToken string
}

// Paginate fetches lazily and rejects repeated cursors rather than looping forever.
func Paginate[T any](ctx context.Context, fetch func(context.Context, string) (Page[T], error), options ...PaginationOption) iter.Seq2[T, error] {
	config := paginationConfig{}
	for _, option := range options {
		option(&config)
	}
	return func(yield func(T, error) bool) {
		token := config.startToken
		seen := map[string]bool{}
		if token != "" {
			seen[token] = true
		}
		for {
			if err := ctx.Err(); err != nil {
				var zero T
				yield(zero, err)
				return
			}
			page, err := fetch(ctx, token)
			if err != nil {
				var zero T
				yield(zero, err)
				return
			}
			for _, item := range page.Items {
				if !yield(item, nil) {
					return
				}
			}
			if page.NextToken == "" {
				return
			}
			if seen[page.NextToken] {
				var zero T
				yield(zero, fmt.Errorf("pagination returned repeated cursor"))
				return
			}
			seen[page.NextToken] = true
			token = page.NextToken
		}
	}
}

// Stream reads JSON SSE events incrementally, closing the body on early exit.
func Stream[T any](ctx context.Context, c *Client, r Request, opts ...RequestOption) iter.Seq2[T, error] {
	return func(yield func(T, error) bool) {
		streamOptions := append(append([]RequestOption(nil), opts...), WithHeader("Accept", "text/event-stream"))
		resp, err := c.send(ctx, r, streamOptions...)
		if err != nil {
			var zero T
			yield(zero, err)
			return
		}
		defer func() { _ = resp.Body.Close() }()
		if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
			var zero T
			yield(zero, fmt.Errorf("expected text/event-stream"))
			return
		}
		reader := bufio.NewReader(resp.Body)
		var data []string
		emit := func() bool {
			if len(data) == 0 {
				return true
			}
			payload := strings.Join(data, "\n")
			data = nil
			if payload == "[DONE]" {
				return false
			}
			var value T
			if err := json.Unmarshal([]byte(payload), &value); err != nil {
				yield(value, err)
				return false
			}
			if nilValue(value) && !r.NullableResponse {
				yield(value, fmt.Errorf("event null is not allowed"))
				return false
			}
			return yield(value, nil)
		}
		for {
			line, e := reader.ReadString('\n')
			line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
			if line == "" {
				if !emit() {
					return
				}
			} else if strings.HasPrefix(line, "data:") {
				v := strings.TrimPrefix(line, "data:")
				data = append(data, strings.TrimPrefix(v, " "))
			}
			if e != nil {
				if e == io.EOF {
					emit()
				} else {
					var zero T
					yield(zero, e)
				}
				return
			}
		}
	}
}

// Query encodes primitive and repeated values using wire field names.
func Query(values map[string]any) map[string][]string {
	out := map[string][]string{}
	for key, value := range values {
		if o, ok := value.(presence); ok && !o.IsSet() {
			continue
		}
		b, err := json.Marshal(value)
		if err != nil || string(b) == "null" {
			continue
		}
		var v any
		decoder := json.NewDecoder(strings.NewReader(string(b)))
		decoder.UseNumber()
		if decoder.Decode(&v) != nil {
			continue
		}
		switch x := v.(type) {
		case []any:
			for _, item := range x {
				out[key] = append(out[key], fmt.Sprint(item))
			}
		case map[string]any:
			out[key] = []string{string(b)}
		default:
			out[key] = []string{fmt.Sprint(v)}
		}
	}
	return out
}

// CloneHeaders copies caller headers before request options are applied.
func CloneHeaders(h http.Header) http.Header {
	if h == nil {
		return make(http.Header)
	}
	return h.Clone()
}

// PaginationOption configures lazy pagination without changing HTTP options.
type PaginationOption func(*paginationConfig)
type paginationConfig struct{ startToken string }

func WithStartToken(token string) PaginationOption {
	return func(c *paginationConfig) { c.startToken = token }
}

// PageCursor preserves an absent initial optional cursor.
func PageCursor(token string, original Optional[string]) Optional[string] {
	if token == "" && !original.IsSet() {
		return original
	}
	return Some(token)
}
