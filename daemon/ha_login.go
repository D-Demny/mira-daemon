package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coder/websocket"
	librespot "github.com/devgianlu/go-librespot"
)

// ticket 9.4 (daemon part 2) + issue #4: the Home Assistant connection
// endpoints for the settings UI (ticket 9.4, design section 4, tasks 9+10).
//
// POST /api/ha/login
//
// Background (issue #4): HA 2026.9 removed username/password login from the
// WebSocket auth protocol - the auth schema only accepts a long-lived access
// token ({"type":"auth","access_token":...}) or the api_password option (not
// used here), and it rejects unknown fields including "id". The REST
// /api/login path never existed, so there is no credential-based login left:
// the user pastes a long-lived access token (HA UI: Profile -> Security ->
// Long-Lived Access Tokens) and the daemon validates it by speaking the real
// WebSocket protocol - which is exactly what the T1 acceptance needs, since
// the REST probe (/api/ha/test) alone cannot prove WS auth works.
//
//	Body {"url","token"}. The daemon dials the HA websocket API at
//	ws(s)://<host:port>/api/websocket (the url is normalized: trimmed, missing
//	scheme defaults to http, trailing slash stripped, ws(s) mapped to http(s))
//	and sends the token VERBATIM (same rule as ParseHaConfig):
//
//	{"type":"auth","access_token":T}
//	    -> {"type":"auth_ok"}      (-> 200)
//	    -> {"type":"auth_invalid"} (-> invalid_credentials)
//	    -> anything else           (protocol failure -> unreachable)
//
// No "id" field on the auth message (HA 2026.9 rejects it, issue #4 section
// 3). No token re-issue: the user-provided token stays in effect - the old
// client-issued token flow is gone with the username/password path.
// Response: {"ok":true,"token":"<token>"} (200; the validated token echoed so
// the UI's store shape does not change) or {"ok":false,"error":<class>}:
// 400 bad_request (missing/invalid url, missing token), 401
// invalid_credentials, 502 unreachable (dial error, any timeout, or a
// protocol failure - see haAuthWithToken).
// POST /api/ha/test
//
//	Body {"url","token"} (token optional). Probes GET <url>/api/ (the HA
//	REST root) with the Bearer token if one was given. The response is
//	always 200 with {"reachable":bool,"authenticated":bool,
//	"defaults":{"url":<config default>}}, except 400 {"ok":false,
//	"error":"bad_request"} for a non-empty but invalid url (unknown
//	scheme, unparseable, no host). An empty or missing url is legal and
//	just skips the probe (reachable+authenticated stay false): the UI
//	needs defaults.url in the unconfigured state (no ha entry saved in
//	the settings blob) and this endpoint is the only source for the
//	build-time default URL (ticket 9.4, task 7):
//	  200 from HA            -> reachable+authenticated
//	  401/403                -> reachable, unauthenticated
//	  any other HTTP status  -> reachable, unauthenticated (HA answered the
//	                           probe; /api/ is expected to accept the
//                           credentials)
//	  dial error/timeout     -> unreachable
//	defaults.url is the config.yml default URL resolved through
//	getHomeAssistantConfig (runtime blob wins if the user already saved HA
//	settings, build-time value otherwise; empty when nothing is configured).
//	The UI pre-fills the URL field with it (ticket 9.4 task 7).
//
// Both are cross-service handlers like /api/pi/*: no player session needed,
// registered without the playerReady gate (serve, api_server.go).
//
// SECURITY (ticket 9.4 mandatory lesson, hard acceptance criterion):
// the url and token must NEVER appear in a log line. Log lines carry at most
// the host of the URL and the error class. The token appears only in the
// JSON response body of the login endpoint.

// Error classes of the login endpoint (body {"ok":false,"error":<class>}).
const (
	haErrBadRequest   = "bad_request"
	haErrUnreachable  = "unreachable"
	haErrInvalidCreds = "invalid_credentials"
)

// Timeouts are package-level vars (not consts) so tests can dial them down;
// each request re-reads the current values.
var (
	haLoginConnectTimeout = 5 * time.Second  // websocket dial (handshake)
	haLoginOverallTimeout = 15 * time.Second // dial + auth
	haTestTimeout         = 5 * time.Second  // GET <url>/api/ probe
)

// Request/response shapes of the two endpoints.
type haLoginRequest struct {
	URL   string `json:"url"`
	Token string `json:"token"`
}

type haTestRequest struct {
	URL   string `json:"url"`
	Token string `json:"token"`
}

type haLoginSuccess struct {
	OK    bool   `json:"ok"`
	Token string `json:"token"`
}

type haLoginErrorBody struct {
	OK    bool   `json:"ok"`
	Error string `json:"error"`
}

type haTestDefaults struct {
	URL string `json:"url"`
}

type haTestResponse struct {
	Reachable     bool           `json:"reachable"`
	Authenticated bool           `json:"authenticated"`
	Defaults      haTestDefaults `json:"defaults"`
}

// normalizeHaURL trims the user input and canonicalizes it to an http(s)
// URL without a trailing slash: a missing scheme defaults to http, ws(s)
// schemes map back to http(s). Empty input, unparseable URLs, hosts without
// a port are NOT rejected (HA's default port 8123 is a valid target) - but
// unknown schemes are, they can only yield a confusing dial error.
// Returns "" when the input is unusable.
func normalizeHaURL(raw string) string {
	u := strings.TrimSpace(raw)
	if u == "" {
		return ""
	}
	if !strings.Contains(u, "://") {
		u = "http://" + u
	}
	parsed, err := url.Parse(u)
	if err != nil || parsed.Host == "" {
		return ""
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "ws":
		parsed.Scheme = "http"
	case "https", "wss":
		parsed.Scheme = "https"
	default:
		return ""
	}
	return strings.TrimSuffix(parsed.String(), "/")
}

// haWebSocketURL maps a canonical http(s) URL to the HA websocket endpoint
// URL (http->ws, https->wss): http://10.10.1.104:8123 ->
// ws://10.10.1.104:8123/api/websocket.
func haWebSocketURL(canonical string) string {
	if strings.HasPrefix(canonical, "https://") {
		return "wss://" + strings.TrimPrefix(canonical, "https://") + "/api/websocket"
	}
	return "ws://" + strings.TrimPrefix(canonical, "http://") + "/api/websocket"
}

// haHostForLog extracts the host:port part of a canonical URL - the only
// piece of the URL a log line may carry (see the file header, SECURITY).
func haHostForLog(canonical string) string {
	if parsed, err := url.Parse(canonical); err == nil && parsed.Host != "" {
		return parsed.Host
	}
	return canonical
}

// haStatusForError maps an error class to the HTTP status the endpoint
// answers with (see the file header).
func haStatusForError(class string) int {
	switch class {
	case haErrBadRequest:
		return http.StatusBadRequest
	case haErrInvalidCreds:
		return http.StatusUnauthorized
	default: // haErrUnreachable
		return http.StatusBadGateway
	}
}

func writeHaJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// haAuthWithToken dials the HA websocket API at u and validates the
// long-lived access token by sending it as the auth request. It returns the
// validated token (echoed) and "" on success, or "" and the error class on
// failure.
//
// Error mapping (second return value; "" = success):
//   - dial error, any timeout, a non-text read, or an unexpected answer to
//     the auth request (anything that is not auth_ok/auth_invalid) ->
//     haErrUnreachable (502). A protocol failure is indistinguishable from a
//     transport problem on the device side and is definitely not a
//     credential problem. Token auth cannot trigger an MFA reprompt, so no
//     separate class exists for it (issue #4 removed the username/password
//     path that used to reach the reprompt).
//   - auth_invalid -> haErrInvalidCreds (401): this HA instance does not
//     accept the token.
//
// SECURITY: u and the token never reach a log line; only the host and the
// error class are logged (ticket 9.4 mandatory lesson).
func haAuthWithToken(log librespot.Logger, u, token string) (string, string) {
	host := haHostForLog(u)

	// The overall timeout bounds the whole conversation; the connect
	// timeout bounds only the dial. Deriving the dial ctx from the overall
	// ctx keeps both limits independent but cumulative.
	overallCtx, cancel := context.WithTimeout(context.Background(), haLoginOverallTimeout)
	defer cancel()

	dialCtx, dialCancel := context.WithTimeout(overallCtx, haLoginConnectTimeout)
	conn, _, err := websocket.Dial(dialCtx, haWebSocketURL(u), nil)
	dialCancel()
	if err != nil {
		log.Warnf("ha login: %s: dial failed (%s)", host, haErrUnreachable)
		return "", haErrUnreachable
	}
	// On a timeout the ctx cancellation already closed the conn (coder/
	// websocket self-closes), so this Close returns immediately there.
	defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()

	authMsg, err := json.Marshal(haWsAuthRequest{Type: "auth", AccessToken: token})
	if err != nil {
		return "", haErrUnreachable
	}
	if err := conn.Write(overallCtx, websocket.MessageText, authMsg); err != nil {
		log.Warnf("ha login: %s: auth write failed (%s)", host, haErrUnreachable)
		return "", haErrUnreachable
	}

	msgType, data, err := conn.Read(overallCtx)
	if err != nil || msgType != websocket.MessageText {
		log.Warnf("ha login: %s: auth response failed (%s)", host, haErrUnreachable)
		return "", haErrUnreachable
	}
	var authResp struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &authResp); err != nil {
		log.Warnf("ha login: %s: malformed auth response (%s)", host, haErrUnreachable)
		return "", haErrUnreachable
	}
	switch authResp.Type {
	case "auth_ok":
		log.Infof("ha login: access token accepted by %s", host)
		return token, ""
	case "auth_invalid":
		log.Warnf("ha login: %s: token rejected (%s)", host, haErrInvalidCreds)
		return "", haErrInvalidCreds
	default:
		// An unexpected answer to the auth request (see func docs):
		// protocol failure, not a credential problem.
		log.Warnf("ha login: %s: unexpected auth response type %q (%s)", host, authResp.Type, haErrUnreachable)
		return "", haErrUnreachable
	}
}

// haWsAuthRequest is the HA WebSocket auth request for long-lived token
// login (issue #4). Struct field order is part of the wire contract: the
// tests assert the exact byte sequence (the HA protocol is JSON, key order
// is irrelevant to HA, but a stable shape keeps the traffic greppable and
// the tests strict). No "id": HA 2026.9's auth schema rejects unknown
// fields, including "id".
type haWsAuthRequest struct {
	Type        string `json:"type"`
	AccessToken string `json:"access_token"`
}

// handleHaLogin implements POST /api/ha/login (see the file header for the
// full contract).
func (s *ConcreteApiServer) handleHaLogin(w http.ResponseWriter, r *http.Request) {
	var req haLoginRequest
	if err := jsonDecode(r, &req); err != nil {
		writeHaJSON(w, http.StatusBadRequest, haLoginErrorBody{OK: false, Error: haErrBadRequest})
		return
	}
	canonical := normalizeHaURL(req.URL)
	// the token is deliberately VERBATIM - same rule as ParseHaConfig.
	if canonical == "" || req.Token == "" {
		writeHaJSON(w, http.StatusBadRequest, haLoginErrorBody{OK: false, Error: haErrBadRequest})
		return
	}

	token, class := haAuthWithToken(s.log, canonical, req.Token)
	if class != "" {
		writeHaJSON(w, haStatusForError(class), haLoginErrorBody{OK: false, Error: class})
		return
	}
	writeHaJSON(w, http.StatusOK, haLoginSuccess{OK: true, Token: token})
}

// handleHaTest implements POST /api/ha/test (see the file header for the
// full contract).
func (s *ConcreteApiServer) handleHaTest(w http.ResponseWriter, r *http.Request) {
	var req haTestRequest
	if err := jsonDecode(r, &req); err != nil {
		writeHaJSON(w, http.StatusBadRequest, haLoginErrorBody{OK: false, Error: haErrBadRequest})
		return
	}
	// An empty (or missing) url is NOT bad_request (ticket 9.4, task 7):
	// the settings UI needs defaults.url in the unconfigured state (no ha
	// entry in the settings blob) to pre-fill the URL field and to render
	// the "not configured (default: ...)" status line, and this endpoint
	// is the only source for the build-time default URL. So an empty url
	// skips the probe and answers with defaults only (reachable and
	// authenticated stay false). A non-empty url that does not normalize
	// (unknown scheme, unparseable, no host) is still bad_request.
	if strings.TrimSpace(req.URL) == "" {
		writeHaJSON(w, http.StatusOK, haTestResponse{
			Defaults: haTestDefaults{URL: s.getHomeAssistantConfig().URL},
		})
		return
	}
	canonical := normalizeHaURL(req.URL)
	if canonical == "" {
		writeHaJSON(w, http.StatusBadRequest, haLoginErrorBody{OK: false, Error: haErrBadRequest})
		return
	}

	host := haHostForLog(canonical)
	resp := haTestResponse{}
	resp.Defaults.URL = s.getHomeAssistantConfig().URL

	ctx, cancel := context.WithTimeout(r.Context(), haTestTimeout)
	defer cancel()

	probe, err := http.NewRequestWithContext(ctx, http.MethodGet, canonical+"/api/", nil)
	if err != nil {
		writeHaJSON(w, http.StatusBadRequest, haLoginErrorBody{OK: false, Error: haErrBadRequest})
		return
	}
	if req.Token != "" {
		probe.Header.Set("Authorization", "Bearer "+req.Token)
	}
	// Per-request client: the probe is a rare user-triggered action and the
	// timeout must be the current (test-overridable) haTestTimeout.
	client := &http.Client{Timeout: haTestTimeout}
	httpResp, err := client.Do(probe)
	if err != nil {
		// Dial error, refused, timeout, TLS failure: HA did not answer.
		// SECURITY: only the host lands in the log line.
		s.log.Warnf("ha test: %s: probe failed (unreachable)", host)
		writeHaJSON(w, http.StatusOK, resp) // reachable:false, authenticated:false
		return
	}
	defer func() { _ = httpResp.Body.Close() }()
	resp.Reachable = true
	// 200 = /api/ accepted the request (authenticated when a token was
	// sent); 401/403 and every other status = reachable but not
	// authenticated (see the file header).
	resp.Authenticated = httpResp.StatusCode == http.StatusOK
	writeHaJSON(w, http.StatusOK, resp)
}
