package daemon

import (
	"bytes"
	"context"
	crand "crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coder/websocket"
	librespot "github.com/devgianlu/go-librespot"
)

// ticket 9.4 (daemon part 2) + issue #4 + issue #25: the Home Assistant
// connection endpoints for the settings UI (ticket 9.4, design section 4,
// tasks 9+10).
//
// POST /api/ha/login
//
// HA 2026.9 removed username/password from the WebSocket auth protocol -
// the auth schema only accepts an access token ({"type":"auth",
// "access_token":...}) or the api_password option (not used here), and it
// rejects unknown fields including "id". The login endpoint therefore has
// two mutually exclusive modes (token mode wins when both are given):
//
// Token mode - Body {"url","token"}: the user pastes a long-lived access
// token (HA UI: Profile -> Security -> Long-Lived Access Tokens) and the
// daemon validates it by speaking the real WebSocket protocol - which is
// exactly what the T1 acceptance needs, since the REST probe
// (/api/ha/test) alone cannot prove WS auth works. The daemon dials the HA
// websocket API at ws(s)://<host:port>/api/websocket (the url is
// normalized: trimmed, missing scheme defaults to http, trailing slash
// stripped, ws(s) mapped to http(s)). After dialing it consumes the
// server's opening frame, which real HA always sends as the first message:
// {"type":"auth_required"} (anything else on that slot is a protocol
// failure -> unreachable). Only then does it send the token VERBATIM
// (same rule as ParseHaConfig):
//
//	{"type":"auth","access_token":T}
//	    -> {"type":"auth_ok"}      (-> 200)
//	    -> {"type":"auth_invalid"} (-> invalid_credentials)
//	    -> anything else           (protocol failure -> unreachable)
//
// No "id" field on the auth message (HA 2026.9 rejects it, issue #4 section
// 3). No token re-issue: the user-provided token stays in effect.
//
// Credential mode - Body {"url","username","password"} (issue #25): HA
// 2026.9 has no credential-based WS login, so the daemon bootstraps its own
// long-lived access token (LLA) through the REST login flow + WS minting:
//
//	1. POST <url>/auth/login_flow, JSON {"client_id":C,"handler":
//	   ["homeassistant",null],"redirect_uri":R} -> 200 form step "init"
//	   (flow_id; the local provider asks username AND password together in
//	   this one step - there is no username-only split). C = <url>/mira and
//	   R = <url>/mira/callback: deterministic, well-formed http(s) URLs with
//	   a path sharing scheme+netloc, so HA's IndieAuth client verification
//	   passes without fetching anything.
//	2. POST <url>/auth/login_flow/<flow_id>, JSON {"client_id":C,
//	   "username":U,"password":P} -> 200:
//	    - {"type":"create_entry","result":<code>} on success (single-use
//	      auth code);
//	    - the form step again with errors.base="invalid_auth" on wrong
//	      credentials (HA answers 200 - inspect the body, not the status)
//	      -> invalid_credentials;
//	    - an unexpected intermediate flow step (MFA: select_mfa_module/mfa)
//	      -> invalid_credentials with the HA message surfaced (the bootstrap
//	      cannot complete without the MFA code);
//	    - 403 "Login blocked: ..." (user inactive / not allowed to
//	      authenticate remotely) -> invalid_credentials with the HA message.
//	3. POST <url>/auth/token, form-urlencoded grant_type=authorization_code
//	   &client_id=C&code=<code> -> {"access_token":<30-min JWT>, ...}.
//	4. WS to ws(s)://<host:port>/api/websocket: consume the opening
//	   auth_required frame (issue #23), then {"type":"auth",
//	   "access_token":<30-min JWT>} (no "id" field) -> auth_ok; a rejection
//	   of the HA-issued token is a protocol failure -> unreachable (the
//	   credentials themselves were just accepted in step 2).
//	5a. WS {"id":1,"type":"auth/refresh_tokens"} -> result list of the
//	   user's refresh tokens; if a long-lived token with client_name "Mira"
//	   already exists, step 5b mints straight under a unique name. The
//	   pre-check is required because HA 2026.9 sanitizes the duplicate-name
//	   mint error to code "unknown_error" + message "Unknown error", so the
//	   conflict cannot be detected from the mint answer itself. A list-check
//	   failure is non-fatal: proceed with the stable name (the 5b retry
//	   still covers it).
//	5b. WS {"id":2,"type":"auth/long_lived_access_token","client_name":
//	   <name>,"lifespan":3650} -> {"id":2,"type":"result","success":true,
//	   "result":<LLA JWT>}. HA keeps one LLA per (user, client_name):
//	   <name> is the stable "Mira" unless 5a found it taken, in which case
//	   "Mira <UTC YYYYMMDD-HHMMSS>". Any mint failure triggers ONE retry
//	   with a fresh unique name; a second failure -> unreachable. HA's WS
//	   API requires strictly increasing message ids per connection (ERR_ID
//	   REUSE), so the token list, the first mint and the retry carry ids 1,
//	   2 and 3.
//
// Response: 200 {"ok":true,"token":"<T>"} where T is the minted LLA in
// credential mode or the validated token (echoed) in token mode - same
// shape so the UI's store {url,token} does not change - or
// {"ok":false,"error":<class>}: 400 bad_request (missing/invalid url;
// neither a token nor username+password), 401 invalid_credentials (see
// above; optional "message" carries HA's own message, never credentials),
// 502 unreachable (dial error, any timeout, or a protocol failure - see
// haAuthWithToken / haAuthWithCredentials).
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
// the url, token, username and password must NEVER appear in a log line or
// in a response body other than the login endpoint's own. Log lines carry
// at most the host of the URL and the error class; the invalid_credentials
// "message" carries at most HA's own error text (never credentials). The
// token appears only in the JSON response body of the login endpoint.

// Error classes of the login endpoint (body {"ok":false,"error":<class>}).
const (
	haErrBadRequest   = "bad_request"
	haErrUnreachable  = "unreachable"
	haErrInvalidCreds = "invalid_credentials"

	// Credential-mode bootstrap wire constants (issue #25): HA's WS API
	// requires strictly increasing message ids per connection (a reused or
	// lower id is rejected with ERR_ID_REUSE "Identifier values have to
	// increase."), so the bootstrap takes them from a fixed sequence:
	// token-list check first, then the mint attempt(s). Also: the stable
	// LLA client_name (HA keeps one LLA per user per name), its token_type
	// as reported by auth/refresh_tokens, and the LLA lifespan in days.
	haListMsgID    = 1 // auth/refresh_tokens pre-check
	haMintMsgID    = 2 // first mint attempt; the retry takes haMintMsgID+1
	haLLAName      = "Mira"
	haLLADays      = 3650 // ~10 years (issue #25)
	haTokenTypeLLA = "long_lived_access_token"
)

// Timeouts are package-level vars (not consts) so tests can dial them down;
// each request re-reads the current values.
var (
	haLoginConnectTimeout = 5 * time.Second  // websocket dial (handshake)
	haLoginOverallTimeout = 15 * time.Second // dial + auth
	haLoginHTTPTimeout    = 10 * time.Second // one REST bootstrap request
	haTestTimeout         = 5 * time.Second  // GET <url>/api/ probe
)

// Request/response shapes of the two endpoints.
type haLoginRequest struct {
	URL   string `json:"url"`
	Token string `json:"token"`

	// Credential-mode bootstrap (issue #25): used only when Token is empty;
	// both Username and Password must be present together.
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
	// HA's own message for invalid_credentials (issue #25: e.g. "Login
	// blocked: ..." or the unexpected flow step) - never credentials.
	Message string `json:"message,omitempty"`
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

// haWsDialAndAuth dials the HA websocket API at u, consumes the server's
// opening frame - real HA always sends {"type":"auth_required"} as the
// first server->client message (anything else on that slot is a protocol
// failure, issue #23) - and then sends the access-token auth request
// VERBATIM ({"type":"auth","access_token":T}, no "id" field: HA 2026.9's
// auth schema rejects unknown fields). On success it returns the open
// connection plus the context that bounds the rest of the conversation and
// its cancel func; on failure it returns class != "" and has already closed
// the connection (on a ctx timeout coder/websocket self-closes, so the
// Close there is immediate).
//
// SECURITY: u and the token never reach a log line; only the host and the
// error class are logged (ticket 9.4 mandatory lesson).
func haWsDialAndAuth(log librespot.Logger, u, token string) (*websocket.Conn, context.Context, context.CancelFunc, string) {
	host := haHostForLog(u)

	// The overall timeout bounds the whole conversation; the connect
	// timeout bounds only the dial. Deriving the dial ctx from the overall
	// ctx keeps both limits independent but cumulative.
	overallCtx, cancel := context.WithTimeout(context.Background(), haLoginOverallTimeout)
	dialCtx, dialCancel := context.WithTimeout(overallCtx, haLoginConnectTimeout)
	conn, _, err := websocket.Dial(dialCtx, haWebSocketURL(u), nil)
	dialCancel()
	if err != nil {
		log.Warnf("ha login: %s: dial failed (%s)", host, haErrUnreachable)
		cancel()
		return nil, nil, nil, haErrUnreachable
	}
	closeFailed := func(class string) (*websocket.Conn, context.Context, context.CancelFunc, string) {
		_ = conn.Close(websocket.StatusNormalClosure, "")
		cancel()
		return nil, nil, nil, class
	}

	openType, openData, err := conn.Read(overallCtx)
	if err != nil || openType != websocket.MessageText {
		log.Warnf("ha login: %s: opening frame read failed (%s)", host, haErrUnreachable)
		return closeFailed(haErrUnreachable)
	}
	var opening struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(openData, &opening); err != nil || opening.Type != "auth_required" {
		log.Warnf("ha login: %s: unexpected opening frame (%s)", host, haErrUnreachable)
		return closeFailed(haErrUnreachable)
	}

	authMsg, err := json.Marshal(haWsAuthRequest{Type: "auth", AccessToken: token})
	if err != nil {
		return closeFailed(haErrUnreachable)
	}
	if err := conn.Write(overallCtx, websocket.MessageText, authMsg); err != nil {
		log.Warnf("ha login: %s: auth write failed (%s)", host, haErrUnreachable)
		return closeFailed(haErrUnreachable)
	}
	return conn, overallCtx, cancel, ""
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

	conn, overallCtx, cancel, class := haWsDialAndAuth(log, u, token)
	if class != "" {
		return "", class
	}
	defer cancel()
	defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()

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

// haLoginFlowResult is one step's answer of HA's REST login flow (POST
// /auth/login_flow or POST /auth/login_flow/{flow_id}); see the file header
// for the full credential-mode contract.
type haLoginFlowResult struct {
	Type   string `json:"type"` // "form" (a step) or "create_entry" (done)
	FlowID string `json:"flow_id"`
	StepID string `json:"step_id"` // e.g. "init", "select_mfa_module", "mfa"
	// Result is the single-use auth code when Type="create_entry"; Errors
	// holds the per-field errors (e.g. {"base":"invalid_auth"}). HA answers
	// wrong credentials with HTTP 200, so callers must inspect these
	// fields, not the status code.
	Result string            `json:"result"`
	Errors map[string]string `json:"errors"`
}

// haBootstrapRequest runs one HTTP step of the credential bootstrap (issue
// #25) against u with a per-request client and the current (test-overridable)
// haLoginHTTPTimeout - the same pattern as the /api/ha/test probe. It returns
// the status code and body on transport success, or class != "" on dial
// error, timeout, or unreadable response (unreachable). Non-2xx statuses are
// NOT an error here: HA answers wrong credentials with 200 and protocol
// deviations (bad client_id, unknown handler, IP change) with 400/403/404 -
// the callers classify status+body.
func haBootstrapRequest(ctx context.Context, method, endpoint string, contentType string, payload []byte) (int, []byte, string) {
	client := &http.Client{Timeout: haLoginHTTPTimeout}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(payload))
	if err != nil {
		return 0, nil, haErrUnreachable
	}
	req.Header.Set("Content-Type", contentType)
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, haErrUnreachable
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, haErrUnreachable
	}
	return resp.StatusCode, body, ""
}

// haHTTPErrorMessage extracts the human-readable message of an HA error
// response ({"message":"..."} or {"error":...,"error_description":"..."}) -
// the only part of an error body that may be surfaced to the UI (SECURITY:
// never credentials). "" when the body carries no message.
func haHTTPErrorMessage(body []byte) string {
	var m struct {
		Message          string `json:"message"`
		ErrorDescription string `json:"error_description"`
	}
	if err := json.Unmarshal(body, &m); err != nil {
		return ""
	}
	if m.Message != "" {
		return m.Message
	}
	return m.ErrorDescription
}

// haLoginClientIDs derives the deterministic IndieAuth client pair from the
// canonical base url (issue #25, file header step 1): a well-formed http(s)
// URL with a path and a redirect_uri sharing its scheme+netloc, so HA's
// verify_client_id/verify_redirect_uri pass without fetching anything.
func haLoginClientIDs(canonical string) (clientID, redirectURI string) {
	return canonical + "/mira", canonical + "/mira/callback"
}

// haStartLoginFlow is bootstrap step 1: POST <u>/auth/login_flow and expect
// the form step "init" asking username+password together (the local provider
// has no username-only split). Returns the flow result (carrying the
// flow_id) on success; any deviation from the documented shape (non-200,
// unexpected type/step) is a protocol failure -> unreachable.
func haStartLoginFlow(log librespot.Logger, u string) (*haLoginFlowResult, string) {
	host := haHostForLog(u)
	clientID, redirectURI := haLoginClientIDs(u)

	payload, err := json.Marshal(map[string]any{
		"client_id":    clientID,
		"handler":      []any{"homeassistant", nil}, // the length-1 handler ["homeassistant"] is rejected by HA
		"redirect_uri": redirectURI,
	})
	if err != nil {
		return nil, haErrUnreachable
	}
	ctx, cancel := context.WithTimeout(context.Background(), haLoginHTTPTimeout)
	defer cancel()
	status, body, class := haBootstrapRequest(ctx, http.MethodPost, u+"/auth/login_flow", "application/json", payload)
	if class != "" || status != http.StatusOK {
		log.Warnf("ha login: %s: login flow start failed (%s)", host, haErrUnreachable)
		return nil, haErrUnreachable
	}
	var res haLoginFlowResult
	if err := json.Unmarshal(body, &res); err != nil || res.Type != "form" || res.StepID != "init" {
		log.Warnf("ha login: %s: login flow start unexpected answer (%s)", host, haErrUnreachable)
		return nil, haErrUnreachable
	}
	return &res, ""
}

// haFlowSubmit is bootstrap step 2: POST the username+password to the flow's
// step endpoint. Returns the single-use auth code on success. Wrong
// credentials arrive as HTTP 200 + errors.base="invalid_auth"; an unexpected
// intermediate step (MFA) or a "Login blocked" 403 (the account was accepted
// by the provider but cannot be issued tokens) both map to
// invalid_credentials with HA's own message; anything else is a protocol
// failure -> unreachable.
func haFlowSubmit(log librespot.Logger, u, clientID, flowID, username, password string) (string, string, string) {
	host := haHostForLog(u)

	payload, err := json.Marshal(map[string]any{
		"client_id": clientID,
		"username":  username,
		"password":  password,
	})
	if err != nil {
		return "", haErrUnreachable, ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), haLoginHTTPTimeout)
	defer cancel()
	status, body, class := haBootstrapRequest(ctx, http.MethodPost, u+"/auth/login_flow/"+flowID, "application/json", payload)
	if class != "" {
		log.Warnf("ha login: %s: credential submit failed (%s)", host, haErrUnreachable)
		return "", haErrUnreachable, ""
	}
	var res haLoginFlowResult
	if err := json.Unmarshal(body, &res); err != nil {
		log.Warnf("ha login: %s: credential submit unexpected answer (%s)", host, haErrUnreachable)
		return "", haErrUnreachable, ""
	}
	if status == http.StatusForbidden {
		// "Login blocked: <reason>" - the credentials are accepted but the
		// user cannot be issued tokens (inactive, or not allowed to
		// authenticate from this IP). A credential problem, surfaced with HA's
		// own message.
		log.Warnf("ha login: %s: login blocked (%s)", host, haErrInvalidCreds)
		return "", haErrInvalidCreds, haHTTPErrorMessage(body)
	}
	if status != http.StatusOK {
		// 400 (bad client_id, IP change), 404 (unknown flow): a deviation
		// from the documented flow -> protocol failure.
		log.Warnf("ha login: %s: credential submit unexpected status (%s)", host, haErrUnreachable)
		return "", haErrUnreachable, ""
	}
	switch res.Type {
	case "create_entry":
		if res.Result == "" {
			log.Warnf("ha login: %s: flow finished without an auth code (%s)", host, haErrUnreachable)
			return "", haErrUnreachable, ""
		}
		return res.Result, "", ""
	case "form":
		// Wrong credentials: HA answers HTTP 200 with errors.base set.
		if res.Errors["base"] == "invalid_auth" {
			log.Warnf("ha login: %s: credentials rejected (%s)", host, haErrInvalidCreds)
			return "", haErrInvalidCreds, "invalid_auth"
		}
		// An unexpected intermediate step (MFA: select_mfa_module/mfa, or an
		// invalid_code error there): the bootstrap cannot complete without
		// the MFA code. A credential problem, with HA's step surfaced.
		log.Warnf("ha login: %s: unexpected flow step (%s)", host, haErrInvalidCreds)
		return "", haErrInvalidCreds, fmt.Sprintf("unexpected flow step %q", res.StepID)
	default:
		log.Warnf("ha login: %s: credential submit unexpected answer type (%s)", host, haErrUnreachable)
		return "", haErrUnreachable, ""
	}
}

// haExchangeToken is bootstrap step 3: POST <u>/auth/token (form-urlencoded)
// and exchange the single-use auth code for a 30-minute JWT. The identical
// client_id of steps 1+2 is required (the code is keyed by (client_id,
// code)).
func haExchangeToken(log librespot.Logger, u, clientID, code string) (string, string) {
	host := haHostForLog(u)

	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("client_id", clientID)
	form.Set("code", code)
	ctx, cancel := context.WithTimeout(context.Background(), haLoginHTTPTimeout)
	defer cancel()
	status, body, class := haBootstrapRequest(ctx, http.MethodPost, u+"/auth/token", "application/x-www-form-urlencoded", []byte(form.Encode()))
	if class != "" || status != http.StatusOK {
		log.Warnf("ha login: %s: token exchange failed (%s)", host, haErrUnreachable)
		return "", haErrUnreachable
	}
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &tok); err != nil || tok.AccessToken == "" {
		log.Warnf("ha login: %s: token exchange unexpected answer (%s)", host, haErrUnreachable)
		return "", haErrUnreachable
	}
	return tok.AccessToken, ""
}

// haWsMintRequest is the HA WebSocket command that mints a long-lived access
// token (file header step 5). Struct field order is part of the wire
// contract: lifespan is required by HA (there is no default) and client_name
// must be unique per user - one LLA per (user, client_name).
type haWsMintRequest struct {
	ID         int    `json:"id"`
	Type       string `json:"type"`
	ClientName string `json:"client_name"`
	Lifespan   int    `json:"lifespan"`
}

// haWsMintResponse is HA's answer to the mint command ({"id":2,"type":
// "result","success":...}); on success Result carries the LLA JWT. Error
// frames carry their text in the error payload - but note that HA 2026.9
// sanitizes unexpected exceptions (incl. the duplicate-name ValueError) to
// code "unknown_error" + message "Unknown error", so a name conflict must
// NOT be detected from this frame; it is pre-checked via
// auth/refresh_tokens instead. Message is kept for diagnostics on
// older/newer frame shapes. ID is a pointer so frames without an id (async
// events) can be skipped.
type haWsMintResponse struct {
	ID      *int          `json:"id"`
	Type    string        `json:"type"`
	Success bool          `json:"success"`
	Result  string        `json:"result"`
	Message string        `json:"message"`
	Error   haWsMintError `json:"error"`
}

// haWsMintError is the "error" payload of a failed WS command frame.
type haWsMintError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// haWsTokenEntry is one entry of the auth/refresh_tokens result list; only
// the two fields needed for the LLA name check are kept. Note that HA names
// the token-type field "type" in this answer (not "token_type").
type haWsTokenEntry struct {
	ClientName string `json:"client_name"`
	Type       string `json:"type"`
}

// haWsTokenListResponse is HA's answer to the auth/refresh_tokens command.
type haWsTokenListResponse struct {
	ID      *int             `json:"id"`
	Type    string           `json:"type"`
	Success bool             `json:"success"`
	Result  []haWsTokenEntry `json:"result"`
	Error   haWsMintError    `json:"error"`
}

// haUniqueLLAName builds the run-specific fallback client_name: the UTC
// timestamp to the second plus 4 hex chars of randomness, so two logins in
// the same second cannot mint under the same name (the duplicate-name error
// is sanitized by HA, so the collision must be prevented, not detected).
func haUniqueLLAName() string {
	base := time.Now().UTC().Format("20060102-150405")
	rnd := make([]byte, 2)
	if _, err := crand.Read(rnd); err != nil {
		return fmt.Sprintf("%s %s", haLLAName, base)
	}
	return fmt.Sprintf("%s %s-%x", haLLAName, base, rnd)
}

// haWsStableNameTaken asks HA which refresh tokens the current user already
// has (auth/refresh_tokens - the same command the frontend uses) and
// reports whether a long-lived token with the stable client_name exists.
// The check is required because HA 2026.9 sanitizes the duplicate-name
// mint error to "Unknown error", making it indistinguishable from any other
// server-side failure. Protocol/IO failures are returned as errors and are
// treated as non-fatal by the caller (proceed with the stable name; the
// mint retry still covers the conflict).
func haWsStableNameTaken(conn *websocket.Conn, overallCtx context.Context) (bool, error) {
	msg, err := json.Marshal(map[string]any{
		"id":   haListMsgID,
		"type": "auth/refresh_tokens",
	})
	if err != nil {
		return false, err
	}
	if err := conn.Write(overallCtx, websocket.MessageText, msg); err != nil {
		return false, err
	}
	for {
		msgType, data, err := conn.Read(overallCtx)
		if err != nil || msgType != websocket.MessageText {
			return false, fmt.Errorf("list response read failed")
		}
		var resp haWsTokenListResponse
		if err := json.Unmarshal(data, &resp); err != nil {
			return false, fmt.Errorf("malformed list response")
		}
		if resp.ID == nil || *resp.ID != haListMsgID {
			continue // an async event for another request
		}
		if !resp.Success {
			return false, fmt.Errorf("list failed: %s", resp.Error.Message)
		}
		for _, e := range resp.Result {
			if e.ClientName == haLLAName && e.Type == haTokenTypeLLA {
				return true, nil
			}
		}
		return false, nil
	}
}

// haMintLLA is bootstrap step 5: on an authenticated HA websocket connection
// (already auth_ok'd) it requests the long-lived access token and returns
// the minted JWT. HA keeps one LLA per (user, client_name), so before
// minting under the stable name it pre-checks via haWsStableNameTaken; a
// taken name goes straight to a unique "Mira <UTC YYYYMMDD-HHMMSS>". Any
// mint failure triggers ONE retry with a fresh unique name (covers the TOCTOU
// race and undiagnosable server errors); a second failure -> unreachable.
// Frames that do not carry the mint request's id (async events) are skipped.
func haMintLLA(log librespot.Logger, host string, conn *websocket.Conn, overallCtx context.Context) (string, string) {
	clientName := haLLAName
	if taken, err := haWsStableNameTaken(conn, overallCtx); err != nil {
		log.Warnf("ha login: %s: LLA name check failed, proceeding with the stable name (%s)", host, err)
	} else if taken {
		clientName = haUniqueLLAName()
		log.Infof("ha login: %s: stable LLA name exists, minting under a unique name", host)
	}
	for attempt := range 2 {
		if attempt > 0 {
			clientName = haUniqueLLAName() // fresh unique name for the retry
		}
		msgID := haMintMsgID + attempt // HA ids must strictly increase per connection
		msg, err := json.Marshal(haWsMintRequest{
			ID:         msgID,
			Type:       "auth/long_lived_access_token",
			ClientName: clientName,
			Lifespan:   haLLADays,
		})
		if err != nil {
			return "", haErrUnreachable
		}
		if err := conn.Write(overallCtx, websocket.MessageText, msg); err != nil {
			log.Warnf("ha login: %s: mint write failed (%s)", host, haErrUnreachable)
			return "", haErrUnreachable
		}

		for {
			msgType, data, err := conn.Read(overallCtx)
			if err != nil || msgType != websocket.MessageText {
				log.Warnf("ha login: %s: mint response failed (%s)", host, haErrUnreachable)
				return "", haErrUnreachable
			}
			var resp haWsMintResponse
			if err := json.Unmarshal(data, &resp); err != nil {
				log.Warnf("ha login: %s: malformed mint response (%s)", host, haErrUnreachable)
				return "", haErrUnreachable
			}
			if resp.ID == nil || *resp.ID != msgID {
				continue // an async event for another request
			}
			if !resp.Success {
				errMsg := resp.Error.Message
				if errMsg == "" {
					errMsg = resp.Message // HA protocol text only - never credentials
				}
				if attempt == 0 {
					log.Warnf("ha login: %s: LLA mint failed (%s): %s, retrying with a unique name", host, haErrUnreachable, errMsg)
					break // -> retry once with the fresh unique client_name
				}
				log.Warnf("ha login: %s: LLA mint failed twice (%s): %s", host, haErrUnreachable, errMsg)
				return "", haErrUnreachable
			}
			if resp.Result == "" {
				log.Warnf("ha login: %s: LLA mint returned no token (%s)", host, haErrUnreachable)
				return "", haErrUnreachable
			}
			log.Infof("ha login: long-lived access token minted by %s", host)
			return resp.Result, ""
		}
	}
	log.Warnf("ha login: %s: LLA mint failed twice (%s)", host, haErrUnreachable)
	return "", haErrUnreachable
}

// haAuthWithCredentials bootstraps a fresh long-lived access token against u
// from username+password (issue #25; see the file header for the full
// contract): REST login flow steps 1+2 -> single-use auth code, step 3 ->
// 30-minute JWT, then WS step 4 (auth with the short-lived JWT) and step 5
// (mint the LLA). It returns the minted LLA and "" on success, or "" and the
// error class (+ HA's message for invalid_credentials) on failure.
//
// SECURITY: u, username and password never reach a log line; only the host
// and the error class are logged (ticket 9.4 mandatory lesson). The minted
// token appears only in the login endpoint's JSON response.
func haAuthWithCredentials(log librespot.Logger, u, username, password string) (string, string, string) {
	host := haHostForLog(u)

	flow, class := haStartLoginFlow(log, u)
	if class != "" {
		return "", class, ""
	}
	clientID, _ := haLoginClientIDs(u)

	code, class, message := haFlowSubmit(log, u, clientID, flow.FlowID, username, password)
	if class != "" {
		return "", class, message
	}

	shortToken, class := haExchangeToken(log, u, clientID, code)
	if class != "" {
		return "", class, ""
	}

	conn, overallCtx, cancel, class := haWsDialAndAuth(log, u, shortToken)
	if class != "" {
		return "", class, ""
	}
	defer cancel()
	defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()

	msgType, data, err := conn.Read(overallCtx)
	if err != nil || msgType != websocket.MessageText {
		log.Warnf("ha login: %s: auth response failed (%s)", host, haErrUnreachable)
		return "", haErrUnreachable, ""
	}
	var authResp struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &authResp); err != nil || authResp.Type != "auth_ok" {
		// HA just issued this token in step 3: a rejection of it is a
		// protocol failure, not a credential problem.
		log.Warnf("ha login: %s: short-lived token rejected (%s)", host, haErrUnreachable)
		return "", haErrUnreachable, ""
	}

	token, class := haMintLLA(log, host, conn, overallCtx)
	return token, class, ""
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
	if canonical == "" {
		writeHaJSON(w, http.StatusBadRequest, haLoginErrorBody{OK: false, Error: haErrBadRequest})
		return
	}

	// Token mode wins when a token is given (the token is deliberately
	// VERBATIM - same rule as ParseHaConfig); credential mode needs BOTH
	// username and password (issue #25).
	var token, class, message string
	if req.Token != "" {
		token, class = haAuthWithToken(s.log, canonical, req.Token)
	} else if req.Username != "" && req.Password != "" {
		token, class, message = haAuthWithCredentials(s.log, canonical, req.Username, req.Password)
	} else {
		writeHaJSON(w, http.StatusBadRequest, haLoginErrorBody{OK: false, Error: haErrBadRequest})
		return
	}

	if class != "" {
		writeHaJSON(w, haStatusForError(class), haLoginErrorBody{OK: false, Error: class, Message: message})
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
