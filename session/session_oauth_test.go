package session

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	librespot "github.com/devgianlu/go-librespot"
)

func TestRefreshOAuthTokenSendsClientIdAndPersists(t *testing.T) {
	var gotBody, gotURL string
	client := tokenPollClient(func(req *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(req.Body)
		gotBody = string(b)
		gotURL = req.URL.String()
		return tokenPollResponse(http.StatusOK, `{"access_token":"new-access","refresh_token":"new-refresh","expires_in":3600}`), nil
	})

	var emitted *librespot.OAuthState
	s := &Session{
		client:        client,
		log:           &librespot.NullLogger{},
		tokenEndpoint: "https://token.example/api/token",
		oauthRefresh:  "old-refresh",
		oauthChanged:  func(st *librespot.OAuthState) { emitted = st },
	}

	if err := s.refreshOAuthToken(); err != nil {
		t.Fatalf("refreshOAuthToken: %v", err)
	}

	if gotURL != "https://token.example/api/token" {
		t.Fatalf("refresh posted to %q, want configured tokenEndpoint", gotURL)
	}
	for _, want := range []string{
		"grant_type=refresh_token",
		"client_id=" + librespot.ClientIdHex,
		"refresh_token=old-refresh",
	} {
		if !strings.Contains(gotBody, want) {
			t.Fatalf("refresh body %q missing %q", gotBody, want)
		}
	}
	if s.oauthToken != "new-access" || s.oauthRefresh != "new-refresh" {
		t.Fatalf("tokens not updated: %q/%q", s.oauthToken, s.oauthRefresh)
	}
	if until := time.Until(s.oauthExpiresAt); until <= 0 || until > 3610*time.Second {
		t.Fatalf("expiresAt not set from expires_in: %v (until %v)", s.oauthExpiresAt, until)
	}
	if emitted == nil {
		t.Fatal("oauthChanged not fired after successful refresh")
	}
	if emitted.AccessToken != "new-access" || emitted.RefreshToken != "new-refresh" {
		t.Fatalf("emitted state wrong: %+v", emitted)
	}
}

func TestRefreshOAuthTokenRequiresRefreshToken(t *testing.T) {
	s := &Session{log: &librespot.NullLogger{}}
	if err := s.refreshOAuthToken(); err == nil {
		t.Fatal("expected error without refresh token")
	}
}

func TestRefreshOAuthTokenInvalidClientKeepsOldTokens(t *testing.T) {
	client := tokenPollClient(func(*http.Request) (*http.Response, error) {
		return tokenPollResponse(http.StatusBadRequest, `{"error":"invalid_client"}`), nil
	})
	s := &Session{
		client:        client,
		log:           &librespot.NullLogger{},
		tokenEndpoint: deviceTokenURL,
		oauthToken:    "stale-access",
		oauthRefresh:  "old-refresh",
	}

	if err := s.refreshOAuthToken(); err == nil {
		t.Fatal("expected error for 400 invalid_client")
	}
	if s.oauthToken != "stale-access" || s.oauthRefresh != "old-refresh" {
		t.Fatalf("tokens changed despite failed refresh: %q/%q", s.oauthToken, s.oauthRefresh)
	}
}

func TestOAuthTokenFuncReturnsValidTokenWithoutNetwork(t *testing.T) {
	client := tokenPollClient(func(*http.Request) (*http.Response, error) {
		t.Fatal("unexpected token endpoint call for a valid token")
		return nil, nil
	})
	s := &Session{
		client:         client,
		log:            &librespot.NullLogger{},
		tokenEndpoint:  "https://token.example/api/token",
		oauthToken:     "valid",
		oauthRefresh:   "r",
		oauthExpiresAt: time.Now().Add(time.Hour),
	}

	tok, err := s.oauthTokenFunc()(context.Background(), false)
	if err != nil {
		t.Fatalf("oauthTokenFunc: %v", err)
	}
	if tok != "valid" {
		t.Fatalf("token = %q, want %q", tok, "valid")
	}
}

func TestOAuthTokenFuncRefreshesExpiredToken(t *testing.T) {
	client := tokenPollClient(func(*http.Request) (*http.Response, error) {
		return tokenPollResponse(http.StatusOK, `{"access_token":"fresh","expires_in":3600}`), nil
	})
	emitted := 0
	s := &Session{
		client:         client,
		log:            &librespot.NullLogger{},
		tokenEndpoint:  "https://token.example/api/token",
		oauthToken:     "stale",
		oauthRefresh:   "r",
		oauthExpiresAt: time.Now().Add(-time.Minute),
		oauthChanged:   func(*librespot.OAuthState) { emitted++ },
	}

	tok, err := s.oauthTokenFunc()(context.Background(), false)
	if err != nil {
		t.Fatalf("oauthTokenFunc: %v", err)
	}
	if tok != "fresh" {
		t.Fatalf("token = %q, want %q", tok, "fresh")
	}
	if emitted != 1 {
		t.Fatalf("oauthChanged fired %d times, want 1", emitted)
	}
}

func TestOAuthTokenFuncStaleFallbackWithoutRefresh(t *testing.T) {
	client := tokenPollClient(func(*http.Request) (*http.Response, error) {
		t.Fatal("unexpected token endpoint call without refresh token")
		return nil, nil
	})
	s := &Session{
		client:         client,
		log:            &librespot.NullLogger{},
		tokenEndpoint:  "https://token.example/api/token",
		oauthToken:     "stale",
		oauthExpiresAt: time.Now().Add(-time.Minute),
	}

	tok, err := s.oauthTokenFunc()(context.Background(), false)
	if err != nil {
		t.Fatalf("oauthTokenFunc: %v", err)
	}
	if tok != "stale" {
		t.Fatalf("token = %q, want stale fallback", tok)
	}
}

func TestOAuthTokenFuncNoTokenNoRefreshErrors(t *testing.T) {
	s := &Session{
		log:            &librespot.NullLogger{},
		tokenEndpoint:  "https://token.example/api/token",
		oauthExpiresAt: time.Now().Add(-time.Minute),
	}

	if _, err := s.oauthTokenFunc()(context.Background(), false); err == nil {
		t.Fatal("expected error when no token and no refresh token available")
	}
}

func TestEmitOAuthChangedNilCallbackIsNoop(t *testing.T) {
	s := &Session{log: &librespot.NullLogger{}}
	s.emitOAuthChanged()
}
