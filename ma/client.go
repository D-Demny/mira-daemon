// Package ma is a minimal client for the Music Assistant HTTP command
// API (issue #134, design issue #133). MA exposes its whole command
// surface through ONE endpoint: POST {base}/api with the JSON body
//
//	{"command": "<name>", "args": {...}, "message_id": "<id>"}
//
// and the header Authorization: Bearer <token>. The response body is the
// raw JSON result of the command - there is no JSON-RPC envelope.
// Failure looks like a non-2xx status, or a 200 whose body is a plain
// error string instead of JSON (both verified live against MA 2.10.7);
// HTTP 401 means the bearer token was rejected and surfaces as the
// distinct ErrUnauthenticated.
//
// The WebSocket event stream (ws://.../ws) is deliberately out of scope:
// the daemon polls players/get at ~1/s instead (issue #134, v1).
package ma

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
)

// defaultTimeout bounds every call. The observer polls once per second,
// so a hung connection must not pile up beyond one interval's worth of
// slack; control commands tolerate the same bound (the UI waits).
const defaultTimeout = 5 * time.Second

// maxBody guards body reads against a pathological response size.
const maxBody = 4 << 20

// ErrUnauthenticated is returned when the server rejects the bearer
// token (HTTP 401) - distinct so callers can log/configure it without
// it looking like a network blip.
var ErrUnauthenticated = errors.New("ma: unauthenticated")

// APIError describes an MA command failure: a non-2xx status, or a 200
// whose body was not JSON (MA's plain-text error answer). Message is
// the trimmed/truncated response body when it carries text.
type APIError struct {
	Command    string
	StatusCode int
	Message    string
}

func (e *APIError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("ma: command %q failed (http %d): %s", e.Command, e.StatusCode, e.Message)
	}
	return fmt.Sprintf("ma: command %q failed (http %d)", e.Command, e.StatusCode)
}

// Client talks to one MA server. It is immutable after construction and
// safe for concurrent use (net/http.Client guarantees).
type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

// NewClient builds a client for the given server base URL (already
// canonicalized by the daemon's config parser: http(s) scheme, no
// trailing slash) and bearer token.
func NewClient(baseURL, token string) *Client {
	return &Client{
		baseURL: strings.TrimSuffix(baseURL, "/"),
		token:   token,
		http:    &http.Client{Timeout: defaultTimeout},
	}
}

// randomMessageID builds the opaque per-request correlation id MA expects
// in the command envelope.
func randomMessageID() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// Call sends one command and returns its raw JSON result (null for
// commands with no return value, the typed payload otherwise). See the
// package header for the wire protocol.
func (c *Client) Call(ctx context.Context, command string, args any) (json.RawMessage, error) {
	payload := map[string]any{
		"command":    command,
		"message_id": randomMessageID(),
	}
	if args == nil {
		payload["args"] = map[string]any{}
	} else {
		payload["args"] = args
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("ma: marshalling %q: %w", command, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("ma: building request for %q: %w", command, err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ma: %s: %w", command, err)
	}
	defer func() { _ = resp.Body.Close() }()

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, fmt.Errorf("ma: %s reading response: %w", command, err)
	}

	if resp.StatusCode == http.StatusUnauthorized {
		return nil, ErrUnauthenticated
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, &APIError{Command: command, StatusCode: resp.StatusCode, Message: strings.TrimSpace(string(data))}
	}

	// an empty 2xx body means "command succeeded, nothing to return"
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, nil
	}
	var raw json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		// a 200 whose body is not JSON is how MA answers command
		// failures as plain error strings ("Unknown command: ...")
		return nil, &APIError{Command: command, StatusCode: resp.StatusCode, Message: strings.TrimSpace(string(data))}
	}
	return raw, nil
}

// playerArgs is the single shared args object of all players/cmd/* commands.
func playerArgs(playerID string) map[string]any {
	return map[string]any{"player_id": playerID}
}
