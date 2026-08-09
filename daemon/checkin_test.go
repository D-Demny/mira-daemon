package daemon

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	librespot "github.com/devgianlu/go-librespot"
)

func TestCheckinWaitJitter(t *testing.T) {
	t.Parallel()
	lo, hi := checkinInterval-checkinJitter, checkinInterval+checkinJitter
	seen := map[time.Duration]bool{}
	for i := 0; i < 200; i++ {
		w := checkinWait()
		if w < lo || w > hi {
			t.Fatalf("wait %v outside [%v, %v]", w, lo, hi)
		}
		seen[w] = true
	}
	if len(seen) < 2 {
		t.Fatal("wait must vary between calls or the fleet stays phase locked")
	}
}

func TestNormalizeVersion(t *testing.T) {
	t.Parallel()
	tests := []struct{ in, want string }{
		{"v1.0.0", "1.0.0"},
		{"1.0.0", "1.0.0"},
		{" v1.2.3 ", "1.2.3"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := normalizeVersion(tt.in); got != tt.want {
			t.Errorf("normalizeVersion(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestVersionLess(t *testing.T) {
	t.Parallel()
	tests := []struct {
		a, b string
		want bool
	}{
		{"1.0.0", "1.1.0", true},
		{"1.1.0", "1.0.0", false},
		{"1.0.0", "1.0.0", false},
		{"1.0.0", "1.0.1", true},
		{"1.9.0", "1.10.0", true},
		{"2.0.0", "1.9.9", false},
		{"1.0", "1.0.1", true},
		{"1.0.0", "1.0.1-rc1", true},
		{"unknown", "1.1.0", false},
		{"1.0.0", "garbage", false},
		{"", "1.0.0", false},
	}
	for _, tt := range tests {
		if got := versionLess(tt.a, tt.b); got != tt.want {
			t.Errorf("versionLess(%q, %q) = %v, want %v", tt.a, tt.b, got, tt.want)
		}
	}
}

type nopStateStore struct{}

func (nopStateStore) Load() (*librespot.AppState, error) { return nil, nil }
func (nopStateStore) Save(*librespot.AppState) error     { return nil }
func (nopStateStore) Wipe() error                        { return nil }

func checkinTestApp(serverURL string) *App {
	return &App{
		log:        &librespot.NullLogger{},
		cfg:        &Config{Checkin: true, CheckinURL: serverURL},
		client:     &http.Client{},
		state:      &librespot.AppState{},
		stateStore: nopStateStore{},
	}
}

func TestDoCheckinStoresOffsetAndLatest(t *testing.T) {
	t.Parallel()
	var gotQuery url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		_, _ = w.Write([]byte(`{"utc_offset_min":-240,"latest_version":"1.1.0"}`))
	}))
	defer srv.Close()

	app := checkinTestApp(srv.URL)
	if err := app.doCheckin(context.Background(), "1.0.0"); err != nil {
		t.Fatal(err)
	}
	if gotQuery.Get("version") != "1.0.0" {
		t.Fatalf("unexpected query: %v", gotQuery)
	}
	if off := app.utcOffsetMin(); off == nil || *off != -240 {
		t.Fatalf("offset not persisted: %v", off)
	}
	if app.latestVersion() != "1.1.0" {
		t.Fatalf("latest_version not persisted: %q", app.latestVersion())
	}
	if !app.updateAvailable() {
		if !versionLess("1.0.0", "1.1.0") {
			t.Fatal("1.1.0 should read as newer than 1.0.0")
		}
	}
}

func TestDoCheckinMissingOffset(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	app := checkinTestApp(srv.URL)
	if err := app.doCheckin(context.Background(), "1.0.0"); err == nil {
		t.Fatal("expected error when the response carries no offset")
	}
	if app.utcOffsetMin() != nil {
		t.Fatal("a response without an offset must not persist one")
	}
}

func TestDoCheckinServerError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	app := checkinTestApp(srv.URL)
	if err := app.doCheckin(context.Background(), "1.0.0"); err == nil {
		t.Fatal("expected error on 500")
	}
	if app.utcOffsetMin() != nil {
		t.Fatal("failed request must not persist an offset")
	}
	if app.hasCheckedInEver() {
		t.Fatal("failed request must not count as a success")
	}
}
