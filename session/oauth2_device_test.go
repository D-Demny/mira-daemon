package session

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func tokenPollClient(fn roundTripFunc) *http.Client {
	return &http.Client{Transport: fn}
}

func tokenPollResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Status:     http.StatusText(status),
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
	}
}

func TestPollDeviceTokenTransportErrorIsTransient(t *testing.T) {
	client := tokenPollClient(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("dial failed")
	})

	_, err := pollDeviceToken(context.Background(), client, "device-code")
	if !errors.Is(err, errDeviceTokenPollTransient) {
		t.Fatalf("pollDeviceToken error = %v, want transient", err)
	}
}

func TestPollDeviceTokenServerErrorIsTransient(t *testing.T) {
	client := tokenPollClient(func(*http.Request) (*http.Response, error) {
		return tokenPollResponse(http.StatusBadGateway, "bad gateway"), nil
	})

	_, err := pollDeviceToken(context.Background(), client, "device-code")
	if !errors.Is(err, errDeviceTokenPollTransient) {
		t.Fatalf("pollDeviceToken error = %v, want transient", err)
	}
}

func TestPollDeviceTokenPendingContinues(t *testing.T) {
	client := tokenPollClient(func(*http.Request) (*http.Response, error) {
		return tokenPollResponse(http.StatusBadRequest, `{"error":"authorization_pending"}`), nil
	})

	tok, err := pollDeviceToken(context.Background(), client, "device-code")
	if err != nil {
		t.Fatalf("pollDeviceToken returned error for pending auth: %v", err)
	}
	if tok != nil {
		t.Fatalf("pollDeviceToken returned token for pending auth: %+v", tok)
	}
}

func TestPollDeviceTokenAccessDeniedIsTerminal(t *testing.T) {
	client := tokenPollClient(func(*http.Request) (*http.Response, error) {
		return tokenPollResponse(http.StatusBadRequest, `{"error":"access_denied"}`), nil
	})

	_, err := pollDeviceToken(context.Background(), client, "device-code")
	if err == nil {
		t.Fatal("pollDeviceToken returned nil error for access_denied")
	}
	if errors.Is(err, errDeviceTokenPollTransient) {
		t.Fatalf("pollDeviceToken marked access_denied transient: %v", err)
	}
}
