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

// ticket 9.4 (daemon part 2): the Home Assistant connection endpoints for
// the settings UI (ticket 9.4, design section 4, tasks 9+10).
//
// POST /api/ha/login
//
//	Body {"url","username","password"} (all strings). The daemon dials the
//	HA websocket API at ws(s)://<host:port>/api/websocket (the url is
//	normalized: trimmed, missing scheme defaults to http, trailing slash
//	stripped, ws(s) mapped to http(s)), sends the credentials and asks HA
//	itself to issue a fresh long-lived access token:
//
//	{"id":1,"type":"auth","username":U,"password":P}
//	    -> {"type":"auth_ok"}                        (continue)
//	    -> {"type":"auth_invalid"}                   (-> invalid_credentials)
//	    -> anything else (MFA reprompt, any other
//	       non auth_ok/auth_invalid answer)          (-> mfa)
//	{"id":2,"type":"auth/long_lived_access_token","client_name":"Mira Thing"}
//	    -> {"id":2,"type":"auth/long_lived_access_token/result","access_token":"<jwt>"}
//
// Without the optional "lifespan" parameter HA issues its default 10-year
// long-lived token - the user never types a token (ticket 9.4, option (a)).
// Response: {"ok":true,"token":"<jwt>"} (200) or {"ok":false,"error":<class>}:
// 400 bad_request (missing/invalid url, username or password),
// 401 invalid_credentials | mfa, 502 unreachable (dial error, any timeout,
// or a protocol failure after a successful auth - see haLoginToken).
//
// POST /api/ha/test
//
//	Body {"url","token"} (token optional). Probes GET <url>/api/ (the HA
//	REST root) with the Bearer token if one was given. The response is
//	always 200 with {"reachable":bool,"authenticated":bool,
//	"defaults":{"url":<config default>}}, except 400 {"ok":false,
//	"error":"bad_request"} for a missing/invalid url:
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
// username, password and token must NEVER appear in a log line. Log lines
// carry at most the host of the URL and the error class. The token appears
// only in the JSON response body of the login endpoint.

// Error classes of the login endpoint (body {"ok":false,"error":<class>}).
const (
	haErrBadRequest   = "bad_request"
	haErrUnreachable  = "unreachable"
	haErrInvalidCreds = "invalid_credentials"
	haErrMfa          = "mfa"
)

// haLoginClientName is the client_name sent with the token request. Fixed
// on purpose: every token this device issues shows up under the same name
// in the HA profile, so it is identifiable and revocable (ticket 9.4,
// token-accumulation note).
const haLoginClientName = "Mira Thing"

// Timeouts are package-level vars (not consts) so tests can dial them down;
// each request re-reads the current values.
var (
	haLoginConnectTimeout = 5 * time.Second  // websocket dial (handshake)
	haLoginOverallTimeout = 15 * time.Second // dial + auth + token exchange
	haTestTimeout         = 5 * time.Second  // GET <url>/api/ probe
)

// Request/response shapes of the two endpoints.
type haLoginRequest struct {
	URL      string `json:"url"`
	Username string `json:"username"`
	Password string `json:"password"`
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
	case haErrInvalidCreds, haErrMfa:
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

// haLoginToken performs the HA websocket handshake described in the file
// header and returns the freshly issued long-lived access token.
//
// Error mapping (second return value; "" = success):
//   - dial error, any timeout, or a failure AFTER a successful auth_ok
//     (read error, unexpected message, empty access_token) ->
//     haErrUnreachable (502). A protocol failure after a successful auth is
//     indistinguishable from a transport problem on the device side and is
//     definitely not a credential problem, so it lands in the generic
//     "could not complete the conversation" class rather than
//     invalid_credentials.
//   - auth_invalid -> haErrInvalidCreds (401)
//   - any other answer to the auth request -> haErrMfa (401). HA answers an
//     MFA-enabled account with a reprompt (type "auth" carrying the
//     mfa_setup / mfa_setup_followup fields); the device cannot do TOTP, so
//     every non auth_ok/auth_invalid answer is classified as mfa (the UI
//     then offers manual token entry, ticket 9.4 phase 2).
//
// SECURITY: u, username and password never reach a log line; only the host
// and the error class are logged (ticket 9.4 mandatory lesson).
func haLoginToken(log librespot.Logger, u, username, password string) (string, string) {
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

	authMsg, err := json.Marshal(haWsAuthRequest{
		ID:       1,
		Type:     "auth",
		Username: username,
		Password: password,
	})
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
		// proceed to the token request
	case "auth_invalid":
		log.Warnf("ha login: %s: credentials rejected (%s)", host, haErrInvalidCreds)
		return "", haErrInvalidCreds
	default:
		// MFA reprompt (type "auth" with mfa_setup/mfa_setup_followup) or
		// any other answer to the auth request (see func docs).
		log.Warnf("ha login: %s: auth reprompt (%s)", host, haErrMfa)
		return "", haErrMfa
	}

	tokenMsg, err := json.Marshal(haWsTokenRequest{
		ID:         2,
		Type:       "auth/long_lived_access_token",
		ClientName: haLoginClientName,
	})
	if err != nil {
		return "", haErrUnreachable
	}
	if err := conn.Write(overallCtx, websocket.MessageText, tokenMsg); err != nil {
		log.Warnf("ha login: %s: token request failed (%s)", host, haErrUnreachable)
		return "", haErrUnreachable
	}

	_, data, err = conn.Read(overallCtx)
	if err != nil {
		log.Warnf("ha login: %s: token response failed (%s)", host, haErrUnreachable)
		return "", haErrUnreachable
	}
	// Documented HA shape: {"id":2,"type":"auth/long_lived_access_token/
	// result","access_token":"<jwt>"}. We accept any message carrying a
	// non-empty access_token; everything else (wrong type, empty token,
	// error result) is a protocol failure -> unreachable (see func docs).
	var tokenResp struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(data, &tokenResp); err != nil || tokenResp.AccessToken == "" {
		log.Warnf("ha login: %s: empty or malformed token result (%s)", host, haErrUnreachable)
		return "", haErrUnreachable
	}
	return tokenResp.AccessToken, ""
}

// The two websocket request messages. Struct field order is part of the
// wire contract: the tests assert the exact byte sequence (the HA protocol
// is JSON, key order is irrelevant to HA, but a stable shape keeps the
// traffic greppable and the tests strict).
type haWsAuthRequest struct {
	ID       int    `json:"id"`
	Type     string `json:"type"`
	Username string `json:"username"`
	Password string `json:"password"`
}

type haWsTokenRequest struct {
	ID         int    `json:"id"`
	Type       string `json:"type"`
	ClientName string `json:"client_name"`
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
	// username is trimmed (the UI trims it too, like ParseHaConfig); the
	// password is deliberately VERBATIM - a leading/trailing space is a
	// legitimate password character (same rule as ParseHaConfig).
	if canonical == "" || strings.TrimSpace(req.Username) == "" || req.Password == "" {
		writeHaJSON(w, http.StatusBadRequest, haLoginErrorBody{OK: false, Error: haErrBadRequest})
		return
	}

	token, class := haLoginToken(s.log, canonical, strings.TrimSpace(req.Username), req.Password)
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
