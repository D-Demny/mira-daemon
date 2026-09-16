package daemon

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	librespot "github.com/devgianlu/go-librespot"
)

// HTTP-handler contract tests, the real contract that the frontend's msw mocks reflect

// brings up a server on an OS-assigned port, closes at test end
func newTestApiServer(t *testing.T) (ApiServer, string) {
	t.Helper()
	srv, err := NewApiServer(&librespot.NullLogger{}, "127.0.0.1", 0, "*", "", "")
	if err != nil {
		t.Fatalf("NewApiServer: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	// We access the underlying listener via the concrete type to read the
	// OS-assigned port. Same-package test, so unexported fields are fine.
	addr := srv.(*ConcreteApiServer).listener.Addr().String()
	return srv, "http://" + addr
}

// testClient is a shared http.Client with a short timeout - keeps a hung
// server from making the test suite hang for minutes.
var testClient = &http.Client{Timeout: 5 * time.Second}

// reads one ApiRequest, replies with (data, err), yields the request for assertions
func drainOne(t *testing.T, srv ApiServer, data any, err error) <-chan ApiRequest {
	t.Helper()
	ch := make(chan ApiRequest, 1)
	go func() {
		select {
		case req := <-srv.Receive():
			ch <- req
			req.Reply(data, err)
		case <-time.After(2 * time.Second):
			// Test will time out anyway via testClient; closing the channel
			// signals "no request seen" so receivers don't block forever.
			close(ch)
		}
	}()
	return ch
}

// /observer/status

func TestObserverStatus_FastPathWhenPlayerNotReady(t *testing.T) {
	t.Parallel()

	// pre-session, observer/status intercepts and returns synthetic "starting up"
	_, base := newTestApiServer(t)

	resp, err := testClient.Get(base + "/observer/status")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status code: got %d want 200 (fast-path should serve synthetic JSON, not 503)", resp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if got, want := body["active"], false; got != want {
		t.Errorf("active: got %v want %v", got, want)
	}
	if got, want := body["message"], "starting up"; got != want {
		t.Errorf("message: got %q want %q", got, want)
	}
}

func TestObserverStatus_DispatchesToChannelWhenPlayerReady(t *testing.T) {
	t.Parallel()

	srv, base := newTestApiServer(t)
	srv.SetPlayerReady(true)

	// Fake consumer drains exactly one request and replies with a stub
	// observer status. The HTTP response body is then the JSON-encoded
	// stub, proving the channel round-trip wired up correctly.
	captured := drainOne(t, srv, map[string]any{
		"active":     true,
		"track_name": "Test Song",
	}, nil)

	resp, err := testClient.Get(base + "/observer/status")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status code: got %d want 200", resp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if got, want := body["track_name"], "Test Song"; got != want {
		t.Errorf("body forwarded from channel reply incorrectly: got %q want %q", got, want)
	}

	req, ok := <-captured
	if !ok {
		t.Fatal("consumer goroutine never received a request")
	}
	if got, want := req.Type, ApiRequestTypeObserverStatus; got != want {
		t.Errorf("ApiRequest.Type: got %q want %q", got, want)
	}
}

func TestObserverStatus_WrongMethodReturns405(t *testing.T) {
	t.Parallel()

	_, base := newTestApiServer(t)

	resp, err := testClient.Post(base+"/observer/status", "application/json", nil)
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST to /observer/status: got %d want 405", resp.StatusCode)
	}
}

func TestNonObserverChannelEndpointReturns503WhenPlayerNotReady(t *testing.T) {
	t.Parallel()

	// other channel-bound endpoints just 503 pre-session, no fast-path
	_, base := newTestApiServer(t)

	resp, err := testClient.Get(base + "/lyrics/abc?track=x&artist=y")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("channel endpoint with playerReady=false: got %d want 503", resp.StatusCode)
	}
}

// /lyrics/{id}

func TestLyrics_ValidResultReturns200WithJSON(t *testing.T) {
	t.Parallel()

	srv, base := newTestApiServer(t)
	srv.SetPlayerReady(true)
	captured := drainOne(t, srv, &LyricsResult{
		SyncType: "LINE_SYNCED",
		Lines:    []LyricsLine{{StartTimeMs: "0", Words: "Hello"}},
	}, nil)

	resp, err := testClient.Get(base + "/lyrics/abc?track=Song&artist=Artist")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status: got %d want 200", resp.StatusCode)
	}
	var body LyricsResult
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.SyncType != "LINE_SYNCED" || len(body.Lines) != 1 {
		t.Errorf("response body mismatch: %+v", body)
	}

	// Verify the request reached the handler with the right shape.
	req := <-captured
	if got, want := req.Type, ApiRequestTypeLyrics; got != want {
		t.Errorf("ApiRequest.Type: got %q want %q", got, want)
	}
	data, ok := req.Data.(ApiRequestDataLyrics)
	if !ok {
		t.Fatalf("ApiRequest.Data type: got %T want ApiRequestDataLyrics", req.Data)
	}
	if data.TrackId != "abc" || data.TrackName != "Song" || data.ArtistName != "Artist" {
		t.Errorf("ApiRequestDataLyrics: got %+v want trackId=abc, name=Song, artist=Artist", data)
	}
}

func TestLyrics_ErrNotFoundMapsTo404(t *testing.T) {
	t.Parallel()

	// 404 = "looked and found nothing" (silent empty state), 500 = lookup failed
	srv, base := newTestApiServer(t)
	srv.SetPlayerReady(true)
	drainOne(t, srv, nil, ErrNotFound)

	resp, err := testClient.Get(base + "/lyrics/abc?track=x&artist=y")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("ErrNotFound should map to 404, got %d", resp.StatusCode)
	}
}

func TestLyrics_GenericErrorMapsTo500(t *testing.T) {
	t.Parallel()

	srv, base := newTestApiServer(t)
	srv.SetPlayerReady(true)
	drainOne(t, srv, nil, errors.New("primary source is on fire"))

	resp, err := testClient.Get(base + "/lyrics/abc?track=x&artist=y")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("generic error should map to 500, got %d", resp.StatusCode)
	}
}

// /auth/status

func TestAuthStatus_NoHandlerRegisteredReturns503(t *testing.T) {
	t.Parallel()

	// /auth/status bypasses the request channel, needs its own handler
	_, base := newTestApiServer(t)

	resp, err := testClient.Get(base + "/auth/status")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("no auth handler: got %d want 503", resp.StatusCode)
	}
}

func TestAuthStatus_NotRequiredKnownYieldsLoadingFalse(t *testing.T) {
	t.Parallel()

	srv, base := newTestApiServer(t)
	srv.SetAuthHandler(func() (required bool, url string, known bool) {
		return false, "", true
	})

	resp, err := testClient.Get(base + "/auth/status")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status: got %d want 200", resp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got, want := body["required"], false; got != want {
		t.Errorf("required: got %v want %v", got, want)
	}
	if got, want := body["loading"], false; got != want {
		// known=true loading should be false
		t.Errorf("loading: got %v want %v", got, want)
	}
	// `url` is omitted when not required
	if _, exists := body["url"]; exists {
		t.Errorf("url should be omitted when required=false; got %v", body["url"])
	}
}

func TestAuthStatus_RequiredWithURLExposesQRTarget(t *testing.T) {
	t.Parallel()

	srv, base := newTestApiServer(t)
	const want = "https://accounts.spotify.com/?code=ABCD"
	srv.SetAuthHandler(func() (required bool, url string, known bool) {
		return true, want, true
	})

	resp, err := testClient.Get(base + "/auth/status")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()

	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["required"] != true {
		t.Errorf("required: got %v want true", body["required"])
	}
	if body["url"] != want {
		t.Errorf("url: got %v want %q", body["url"], want)
	}
}

// /system/reset

type fakeSystemHandler struct{ called atomic.Bool }

func (f *fakeSystemHandler) PerformReset()   { f.called.Store(true) }
func (f *fakeSystemHandler) PerformRestart() { f.called.Store(true) }
func (f *fakeSystemHandler) PerformSuspend() { f.called.Store(true) }

func TestSystemReset_POSTCallsPerformReset(t *testing.T) {
	t.Parallel()

	srv, base := newTestApiServer(t)
	h := &fakeSystemHandler{}
	srv.SetSystemHandler(h)

	resp, err := testClient.Post(base+"/system/reset", "", nil)
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("POST /system/reset: got %d want 200", resp.StatusCode)
	}

	// PerformReset is called from a goroutine (so the daemon can respond
	// 200 before it kills itself)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if h.called.Load() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("PerformReset was not called within 1s of the POST response")
}

func TestSystemReset_NoHandlerReturns503(t *testing.T) {
	t.Parallel()

	// No handler set to 503
	_, base := newTestApiServer(t)

	resp, err := testClient.Post(base+"/system/reset", "", nil)
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("no system handler: got %d want 503", resp.StatusCode)
	}
}

// /player/*

func TestPlayerPlayPause_POSTDispatchesPlayPauseRequest(t *testing.T) {
	t.Parallel()

	srv, base := newTestApiServer(t)
	srv.SetPlayerReady(true)
	captured := drainOne(t, srv, nil, nil)

	resp, err := testClient.Post(base+"/player/playpause", "", nil)
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status: got %d want 200", resp.StatusCode)
	}
	req := <-captured
	if got, want := req.Type, ApiRequestTypePlayPause; got != want {
		t.Errorf("Type: got %q want %q", got, want)
	}
}

func TestPlayerPlay_DecodesOffsetFromBody(t *testing.T) {
	t.Parallel()

	srv, base := newTestApiServer(t)
	srv.SetPlayerReady(true)
	captured := drainOne(t, srv, nil, nil)

	body := strings.NewReader(`{"uri":"spotify:playlist:abc","offset":{"position":3}}`)
	resp, err := testClient.Post(base+"/player/play", "application/json", body)
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status: got %d want 200", resp.StatusCode)
	}
	req := <-captured
	if got, want := req.Type, ApiRequestTypePlay; got != want {
		t.Fatalf("Type: got %q want %q", got, want)
	}
	data, ok := req.Data.(ApiRequestDataPlay)
	if !ok {
		t.Fatalf("Data type: got %T want ApiRequestDataPlay", req.Data)
	}
	if data.Uri != "spotify:playlist:abc" {
		t.Errorf("Uri: got %q want %q", data.Uri, "spotify:playlist:abc")
	}
	if data.Offset == nil {
		t.Fatal("Offset: got nil want non-nil")
	}
	if data.Offset.Position != 3 {
		t.Errorf("Offset.Position: got %d want 3", data.Offset.Position)
	}
	if data.Offset.Uri != "" {
		t.Errorf("Offset.Uri: got %q want empty", data.Offset.Uri)
	}
}

func TestPlayerPlay_OffsetMarshalsToConnectOptions(t *testing.T) {
	t.Parallel()

	// the play command's options envelope must serialize the offset with the
	// web-player wire field names (uri / position)
	cmd := connectCommand{
		Endpoint: "play",
		Context:  &connectContext{Uri: "spotify:playlist:abc", Url: "context://spotify:playlist:abc"},
		Options: &connectOptions{
			License: "tft",
			Offset:  &connectOffset{Position: 3},
		},
	}
	b, err := json.Marshal(cmd)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var envelope map[string]any
	if err := json.Unmarshal(b, &envelope); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	opts, _ := envelope["options"].(map[string]any)
	if opts == nil {
		t.Fatal("options missing from envelope")
	}
	offset, _ := opts["offset"].(map[string]any)
	if offset == nil {
		t.Fatal("offset missing from options")
	}
	if offset["position"] != float64(3) {
		t.Errorf("offset.position: got %v want 3", offset["position"])
	}
	if _, has := offset["uri"]; has {
		t.Errorf("offset.uri should be omitted when empty")
	}
}

func TestPlayerPlayCommand_OffsetUriBecomesSkipTo(t *testing.T) {
	t.Parallel()

	// bug29: connect receivers (go-librespot, Spotify web player) ignore
	// options.offset and only start a context at options.skip_to.track_uri,
	// so a play offset carrying a track uri must be mirrored into skip_to
	data := ApiRequestDataPlay{
		Uri:    "spotify:playlist:abc",
		Offset: &ApiRequestPlayOffset{Uri: "spotify:track:xyz", Position: 2},
	}
	cmd := buildPlayCommand(data)

	if got := cmd.Options.SkipTo.TrackUri; got != "spotify:track:xyz" {
		t.Errorf("options.skip_to.track_uri: got %q want %q", got, "spotify:track:xyz")
	}
	if cmd.Options.Offset == nil {
		t.Fatal("options.offset: got nil want non-nil")
	}
	if got := cmd.Options.Offset.Position; got != 2 {
		t.Errorf("options.offset.position: got %d want 2", got)
	}
	if got := cmd.Options.Offset.Uri; got != "spotify:track:xyz" {
		t.Errorf("options.offset.uri: got %q want %q", got, "spotify:track:xyz")
	}

	// an explicit skip_to_uri still wins over the offset mirror
	dataExplicit := ApiRequestDataPlay{
		Uri:       "spotify:playlist:abc",
		SkipToUri: "spotify:track:explicit",
		Offset:    &ApiRequestPlayOffset{Uri: "spotify:track:xyz", Position: 2},
	}
	if got := buildPlayCommand(dataExplicit).Options.SkipTo.TrackUri; got != "spotify:track:explicit" {
		t.Errorf("explicit skip_to_uri not honored: got %q", got)
	}

	// an offset without a track uri (position-only) must not fabricate a skip_to
	dataPosOnly := ApiRequestDataPlay{
		Uri:    "spotify:playlist:abc",
		Offset: &ApiRequestPlayOffset{Position: 5},
	}
	if got := buildPlayCommand(dataPosOnly).Options.SkipTo.TrackUri; got != "" {
		t.Errorf("skip_to.track_uri without offset uri: got %q want empty", got)
	}
}

// fakeJwt builds a three-segment JWT with the given payload claims (no real
// signature needed — jwtSubClaim only reads the payload segment)
func fakeJwt(claims map[string]any) string {
	b, _ := json.Marshal(claims)
	return "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString(b) + ".c2ln"
}

func TestJwtSubClaim(t *testing.T) {
	t.Parallel()

	if got := jwtSubClaim(fakeJwt(map[string]any{"sub": "abcdef123", "exp": 1893456000})); got != "abcdef123" {
		t.Errorf("sub claim: got %q want abcdef123", got)
	}
	if got := jwtSubClaim(fakeJwt(map[string]any{"exp": 1893456000})); got != "" {
		t.Errorf("missing sub: got %q want empty", got)
	}
	if got := jwtSubClaim("not-a-jwt"); got != "" {
		t.Errorf("non-jwt: got %q want empty", got)
	}
	bad := "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString([]byte("{nope")) + ".c2ln"
	if got := jwtSubClaim(bad); got != "" {
		t.Errorf("undecodable payload: got %q want empty", got)
	}
}

// recordingStateStore counts Save calls so tests can assert that a resolved
// account id actually went through the persistence path
type recordingStateStore struct {
	saves atomic.Int32
}

func (s *recordingStateStore) Load() (*librespot.AppState, error) { return nil, nil }
func (s *recordingStateStore) Save(*librespot.AppState) error     { s.saves.Add(1); return nil }
func (s *recordingStateStore) Wipe() error                        { return nil }

// fakeMeFn is a swappable webMeAccountFn that counts calls and returns the
// given id ("" simulates a failed /v1/me lookup)
type fakeMeFn struct {
	id    string
	calls atomic.Int32
}

func (f *fakeMeFn) fn(ctx context.Context) string { f.calls.Add(1); return f.id }

func TestResolvePlayContextUri_CachedAccountIDIsFastPath(t *testing.T) {
	t.Parallel()

	me := &fakeMeFn{} // must NOT be called — the cached id short-circuits
	state := &librespot.AppState{
		OAuth:     librespot.OAuthState{AccessToken: "opaque-token-no-dots"},
		AccountID: "cached-user",
	}
	p := &AppPlayer{app: &App{log: &librespot.NullLogger{}, state: state, stateStore: nopStateStore{}}, webMeAccountFn: me.fn}

	got := p.resolvePlayContextUri(likedCollectionUri)
	if want := "spotify:user:cached-user:collection:tracks"; got != want {
		t.Fatalf("cached fast path: got %q want %q", got, want)
	}
	if n := me.calls.Load(); n != 0 {
		t.Errorf("web api /v1/me looked up %d times with a cached id, want 0", n)
	}
}

func TestResolvePlayContextUri_WebApiMeFetchCachesAndPersists(t *testing.T) {
	t.Parallel()

	store := &recordingStateStore{}
	me := &fakeMeFn{id: "webapi-user"} // the live device token is opaque — no JWT sub
	state := &librespot.AppState{OAuth: librespot.OAuthState{AccessToken: "opaque-token-no-dots"}}
	p := &AppPlayer{app: &App{log: &librespot.NullLogger{}, state: state, stateStore: store}, webMeAccountFn: me.fn}

	// first play: fetches /v1/me, resolves, caches + persists
	got := p.resolvePlayContextUri(likedCollectionUri)
	if want := "spotify:user:webapi-user:collection:tracks"; got != want {
		t.Fatalf("first resolve: got %q want %q", got, want)
	}
	if state.AccountID != "webapi-user" {
		t.Errorf("state.AccountID after resolve: got %q want webapi-user", state.AccountID)
	}
	if n := store.saves.Load(); n != 1 {
		t.Errorf("persistence saves after first resolve: got %d want 1", n)
	}

	// second play: cached id, no further /v1/me traffic
	got = p.resolvePlayContextUri(likedCollectionUri)
	if want := "spotify:user:webapi-user:collection:tracks"; got != want {
		t.Fatalf("second resolve: got %q want %q", got, want)
	}
	if n := me.calls.Load(); n != 1 {
		t.Errorf("/v1/me looked up %d times over two resolves, want exactly 1", n)
	}
	if n := store.saves.Load(); n != 1 {
		t.Errorf("persistence saves after second resolve: got %d want 1 (already persisted)", n)
	}

	// the play envelope carries the resolved context as its connect uri
	cmd := buildPlayCommand(ApiRequestDataPlay{Uri: got, SkipToUri: "spotify:track:abc"})
	if cmd.Context.Uri != got || cmd.Context.Url != "context://"+got {
		t.Errorf("envelope context: got uri=%q url=%q, want %q", cmd.Context.Uri, cmd.Context.Url, got)
	}
}

func TestResolvePlayContextUri_WebApiFailureFallsBackToJwtSub(t *testing.T) {
	t.Parallel()

	me := &fakeMeFn{id: ""} // /v1/me unreachable (rate limit, offline, ...)
	state := &librespot.AppState{OAuth: librespot.OAuthState{AccessToken: fakeJwt(map[string]any{"sub": "user123"})}}
	p := &AppPlayer{app: &App{log: &librespot.NullLogger{}, state: state, stateStore: nopStateStore{}}, webMeAccountFn: me.fn}

	// issue #56 fix #2 resolution order (3): a failed /v1/me must fall back
	// to the JWT sub claim of tokens that are still JWTs ...
	got := p.resolvePlayContextUri(likedCollectionUri)
	if want := "spotify:user:user123:collection:tracks"; got != want {
		t.Fatalf("jwt fallback: got %q want %q", got, want)
	}
	if n := me.calls.Load(); n != 1 {
		t.Errorf("/v1/me looked up %d times before the jwt fallback, want 1", n)
	}
	if state.AccountID != "" {
		t.Errorf("state.AccountID must stay empty when only the jwt fallback resolved: got %q", state.AccountID)
	}

	// every other context passes through unchanged (byte-for-byte envelope
	// for real playlists + all other contexts, invariant of the #56 fix)
	for _, uri := range []string{
		"spotify:playlist:37i9dQZF1DXcBWIGoYBM5M",
		"spotify:album:abc123",
		"spotify:track:xyz789",
		"spotify:artist:abc456",
	} {
		if res := p.resolvePlayContextUri(uri); res != uri {
			t.Errorf("passthrough %q: got %q", uri, res)
		}
	}
}

func TestResolvePlayContextUri_OpaqueTokenWithoutIdKeepsLegacyBareContext(t *testing.T) {
	t.Parallel()

	me := &fakeMeFn{id: ""} // /v1/me failed AND the token is opaque (live device shape)
	state := &librespot.AppState{OAuth: librespot.OAuthState{AccessToken: "opaque-token-no-dots"}}
	p := &AppPlayer{app: &App{log: &librespot.NullLogger{}, state: state, stateStore: nopStateStore{}}, webMeAccountFn: me.fn}

	// issue #56 fix #2 resolution order (4): nothing resolvable keeps the
	// legacy behavior — the bare pseudo context goes out as before (that
	// mode cannot start liked songs either way; no regression)
	if got := p.resolvePlayContextUri(likedCollectionUri); got != likedCollectionUri {
		t.Errorf("no resolvable id: got %q want the bare pseudo context (legacy)", got)
	}

	// no OAuth token at all (e.g. static-token mode) stays legacy as well
	pNoAuth := &AppPlayer{app: &App{log: &librespot.NullLogger{}, state: &librespot.AppState{}, stateStore: nopStateStore{}}, webMeAccountFn: me.fn}
	if got := pNoAuth.resolvePlayContextUri(likedCollectionUri); got != likedCollectionUri {
		t.Errorf("no token: got %q want the bare pseudo context (legacy)", got)
	}
	if n := me.calls.Load(); n != 2 {
		t.Errorf("/v1/me looked up %d times, want one per unresolvable play", n)
	}
}

// TestWebApiMeAccountID_GuardsNilSession pins the nil-session guard that lets
// unit tests (and static-token setups without a live session) run the full
// resolution chain without touching the network.
func TestWebApiMeAccountID_GuardsNilSession(t *testing.T) {
	t.Parallel()

	state := &librespot.AppState{OAuth: librespot.OAuthState{AccessToken: "opaque-token"}}
	p := &AppPlayer{app: &App{log: &librespot.NullLogger{}, state: state, stateStore: nopStateStore{}}}

	if got := p.webApiMeAccountID(context.Background()); got != "" {
		t.Errorf("nil session: got %q want empty", got)
	}
}

func TestPlayerSeek_DecodesAbsolutePositionFromBody(t *testing.T) {
	t.Parallel()

	srv, base := newTestApiServer(t)
	srv.SetPlayerReady(true)
	captured := drainOne(t, srv, nil, nil)

	body := strings.NewReader(`{"position": 42000, "relative": false}`)
	resp, err := testClient.Post(base+"/player/seek", "application/json", body)
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status: got %d want 200", resp.StatusCode)
	}
	req := <-captured
	if got, want := req.Type, ApiRequestTypeSeek; got != want {
		t.Errorf("Type: got %q want %q", got, want)
	}
	data, ok := req.Data.(ApiRequestDataSeek)
	if !ok {
		t.Fatalf("Data type: got %T want ApiRequestDataSeek", req.Data)
	}
	if data.Position != 42_000 {
		t.Errorf("Position: got %d want 42000", data.Position)
	}
	if data.Relative {
		t.Errorf("Relative: got true want false")
	}
}

func TestPlayerSeek_NegativeAbsolutePositionRejectedAsBadRequest(t *testing.T) {
	t.Parallel()

	srv, base := newTestApiServer(t)
	srv.SetPlayerReady(true)

	body := strings.NewReader(`{"position": -100, "relative": false}`)
	resp, err := testClient.Post(base+"/player/seek", "application/json", body)
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("negative absolute position: got %d want 400", resp.StatusCode)
	}
}

func TestPlayerShuffleContext_BodyShapeDispatchesBool(t *testing.T) {
	t.Parallel()

	srv, base := newTestApiServer(t)
	srv.SetPlayerReady(true)
	captured := drainOne(t, srv, nil, nil)

	body := strings.NewReader(`{"shuffle_context": true}`)
	resp, err := testClient.Post(base+"/player/shuffle_context", "application/json", body)
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status: got %d want 200", resp.StatusCode)
	}
	req := <-captured
	if got, want := req.Type, ApiRequestTypeSetShufflingContext; got != want {
		t.Errorf("Type: got %q want %q", got, want)
	}
	data, ok := req.Data.(ApiRequestDataShuffle)
	if !ok {
		t.Fatalf("Data type: got %T want ApiRequestDataShuffle", req.Data)
	}
	if got, want := data.Shuffle, true; got != want {
		t.Errorf("Shuffle: got %v want %v", got, want)
	}
	// absent smart_shuffle must stay nil so the handler keeps the legacy bare-bool wire shape
	if data.Smart != nil {
		t.Errorf("Smart: got %v want nil for absent field", *data.Smart)
	}
}

func TestPlayerShuffleContext_SmartFlagDispatchVariants(t *testing.T) {
	t.Parallel()

	yes := true
	no := false
	cases := []struct {
		name    string
		body    string
		shuffle bool
		smart   *bool
	}{
		{"smart_true", `{"shuffle_context": true, "smart_shuffle": true}`, true, &yes},
		{"smart_false_explicit", `{"shuffle_context": false, "smart_shuffle": false}`, false, &no},
		{"smart_absent_stays_nil", `{"shuffle_context": false}`, false, nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			srv, base := newTestApiServer(t)
			srv.SetPlayerReady(true)
			captured := drainOne(t, srv, nil, nil)

			body := strings.NewReader(tc.body)
			resp, err := testClient.Post(base+"/player/shuffle_context", "application/json", body)
			if err != nil {
				t.Fatalf("Post: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				t.Errorf("status: got %d want 200", resp.StatusCode)
			}
			req := <-captured
			if got, want := req.Type, ApiRequestTypeSetShufflingContext; got != want {
				t.Errorf("Type: got %q want %q", got, want)
			}
			data, ok := req.Data.(ApiRequestDataShuffle)
			if !ok {
				t.Fatalf("Data type: got %T want ApiRequestDataShuffle", req.Data)
			}
			if data.Shuffle != tc.shuffle {
				t.Errorf("Shuffle: got %v want %v", data.Shuffle, tc.shuffle)
			}
			if (data.Smart == nil) != (tc.smart == nil) {
				t.Errorf("Smart presence: got %v want %v", data.Smart, tc.smart)
			} else if tc.smart != nil && *data.Smart != *tc.smart {
				t.Errorf("Smart: got %v want %v", *data.Smart, *tc.smart)
			}
		})
	}
}

func TestPlayerShuffleContext_MalformedBodyReturnsBadRequest(t *testing.T) {
	t.Parallel()

	srv, base := newTestApiServer(t)
	srv.SetPlayerReady(true)

	body := strings.NewReader(`{"shuffle_context":`)
	resp, err := testClient.Post(base+"/player/shuffle_context", "application/json", body)
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("malformed body: got %d want 400", resp.StatusCode)
	}
}

func TestBuildShuffleCommand_WireShape(t *testing.T) {
	t.Parallel()

	// issue #39 (verified wire protocol): set_options with shuffling_context
	// + modes as siblings of "endpoint" inside the command object; both fields
	// always present.
	boolTrue := true
	boolFalse := false
	tests := []struct {
		name        string
		data        ApiRequestDataShuffle
		shuffling   bool
		enhancement string
	}{
		{"off", ApiRequestDataShuffle{Shuffle: false}, false, "NONE"},
		{"plain_on_no_smart_flag", ApiRequestDataShuffle{Shuffle: true}, true, "NONE"},
		{"plain_on_explicit_smart_false", ApiRequestDataShuffle{Shuffle: true, Smart: &boolFalse}, true, "NONE"},
		{"smart_on", ApiRequestDataShuffle{Shuffle: true, Smart: &boolTrue}, true, "RECOMMENDATION"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := buildShuffleCommand(tt.data)
			if cmd.Endpoint != "set_options" {
				t.Fatalf("Endpoint: got %q want set_options", cmd.Endpoint)
			}
			envBody, err := json.Marshal(connectCommandEnvelope{Command: cmd})
			if err != nil {
				t.Fatalf("Marshal envelope: %v", err)
			}
			var decoded struct {
				Command struct {
					ShufflingContext *bool             `json:"shuffling_context"`
					Modes            map[string]string `json:"modes"`
				} `json:"command"`
			}
			if err := json.Unmarshal(envBody, &decoded); err != nil {
				t.Fatalf("Unmarshal envelope: %v", err)
			}
			var raw map[string]any
			if err := json.Unmarshal(envBody, &raw); err != nil {
				t.Fatalf("Unmarshal raw: %v", err)
			}
			cmdObj, _ := raw["command"].(map[string]any)
			if _, ok := cmdObj["shuffling_context"]; !ok {
				t.Errorf("envelope %s: shuffling_context missing (must always be present): %s", tt.name, envBody)
			}
			if _, ok := cmdObj["modes"]; !ok {
				t.Errorf("envelope %s: modes missing (must always be present): %s", tt.name, envBody)
			}
			if decoded.Command.ShufflingContext == nil || *decoded.Command.ShufflingContext != tt.shuffling {
				t.Errorf("shuffling_context: got %v want %v", decoded.Command.ShufflingContext, tt.shuffling)
			}
			if got := decoded.Command.Modes["context_enhancement"]; got != tt.enhancement {
				t.Errorf("modes.context_enhancement: got %q want %q", got, tt.enhancement)
			}
		})
	}
}

func TestPlayerPlay_EmptyUriRejected(t *testing.T) {
	t.Parallel()

	// gate on empty URI before dispatch
	srv, base := newTestApiServer(t)
	srv.SetPlayerReady(true)

	body := strings.NewReader(`{}`)
	resp, err := testClient.Post(base+"/player/play", "application/json", body)
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("empty uri: got %d want 400", resp.StatusCode)
	}
}

func TestPlayerEndpoints_WrongMethodReturns405(t *testing.T) {
	t.Parallel()

	// GET on a // POST-only endpoint must return 405 across the board.
	_, base := newTestApiServer(t)

	for _, path := range []string{
		"/player/playpause",
		"/player/pause",
		"/player/resume",
		"/player/next",
		"/player/prev",
		"/player/seek",
		"/player/shuffle_context",
		"/player/repeat_context",
		"/player/repeat_track",
	} {
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			resp, err := testClient.Get(base + path)
			if err != nil {
				t.Fatalf("Get %s: %v", path, err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusMethodNotAllowed {
				body, _ := io.ReadAll(resp.Body)
				t.Errorf("GET %s: got %d want 405 (body=%s)", path, resp.StatusCode, bytes.TrimSpace(body))
			}
		})
	}
}

func TestPlayerRepeatContext_ParsesBoolBody(t *testing.T) {
	t.Parallel()

	srv, base := newTestApiServer(t)
	srv.SetPlayerReady(true)
	captured := drainOne(t, srv, nil, nil)

	body := strings.NewReader(`{"repeat_context": true}`)
	resp, err := testClient.Post(base+"/player/repeat_context", "application/json", body)
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status: got %d want 200", resp.StatusCode)
	}
	req := <-captured
	if got, want := req.Type, ApiRequestTypeSetRepeatingContext; got != want {
		t.Errorf("Type: got %q want %q", got, want)
	}
	if got := req.Data.(bool); !got {
		t.Errorf("Data: got %v want true", got)
	}
}

func TestOnPlayerReady_FiresOnTransitionAndImmediatelyWhenReady(t *testing.T) {
	t.Parallel()
	srv, _ := newTestApiServer(t)

	var a, b, c atomic.Int32

	srv.OnPlayerReady(func() { a.Add(1) })
	srv.OnPlayerReady(func() { b.Add(1) })
	if a.Load() != 0 || b.Load() != 0 {
		t.Fatalf("subscribers fired before ready: a=%d b=%d", a.Load(), b.Load())
	}

	srv.SetPlayerReady(true)
	if a.Load() != 1 || b.Load() != 1 {
		t.Fatalf("subscribers not fired on transition: a=%d b=%d", a.Load(), b.Load())
	}

	srv.SetPlayerReady(false)
	srv.SetPlayerReady(true)
	if a.Load() != 1 || b.Load() != 1 {
		t.Fatalf("subscribers re-fired on second transition: a=%d b=%d", a.Load(), b.Load())
	}

	srv.OnPlayerReady(func() { c.Add(1) })
	if c.Load() != 1 {
		t.Fatalf("late subscriber not fired immediately: c=%d", c.Load())
	}
}
