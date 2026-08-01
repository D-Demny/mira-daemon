package daemon

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sirupsen/logrus"
)

// upload errors surface on the debug screen

func uploadApp(reportURL string) *App {
	return &App{
		cfg:    &Config{ReportURL: reportURL},
		client: &http.Client{},
	}
}

func TestUploadErrorHidesSubmitKey(t *testing.T) {
	app := uploadApp("http://127.0.0.1:1/?k=sekretsubmitkey")
	_, err := app.uploadBundle([]byte("x"))
	if err == nil {
		t.Fatal("expected an error")
	}
	msg := err.Error()
	if strings.Contains(msg, "sekretsubmitkey") || strings.Contains(msg, "?k=") {
		t.Fatalf("error leaks the submit key: %q", msg)
	}
	if !strings.HasPrefix(msg, "report upload") {
		t.Fatalf("unexpected error shape: %q", msg)
	}
}

func TestUploadBadURLHidesSubmitKey(t *testing.T) {
	app := uploadApp("http://bad url/?k=sekretsubmitkey")
	_, err := app.uploadBundle([]byte("x"))
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "sekretsubmitkey") {
		t.Fatalf("error leaks the submit key: %q", err.Error())
	}
}

func TestUploadStatusErrorHidesSubmitKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	app := uploadApp(srv.URL + "/?k=sekretsubmitkey")
	_, err := app.uploadBundle([]byte("x"))
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("want a 401 error, got %v", err)
	}
	if strings.Contains(err.Error(), "sekretsubmitkey") {
		t.Fatalf("error leaks the submit key: %q", err.Error())
	}
}

func TestUploadSuccessReturnsId(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("k") != "sekretsubmitkey" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"id":"AB12CD34"}`))
	}))
	defer srv.Close()
	app := uploadApp(srv.URL + "/?k=sekretsubmitkey")
	id, err := app.uploadBundle([]byte("x"))
	if err != nil || id != "AB12CD34" {
		t.Fatalf("want AB12CD34, got %q err %v", id, err)
	}
}

func TestProblemsRingCapturesUploadFailure(t *testing.T) {
	logger := logrus.New()
	buf := NewLogBuffer(20)
	logger.AddHook(buf)

	app := uploadApp("http://127.0.0.1:1/?k=sekretsubmitkey")
	_, err := app.uploadBundle([]byte("x"))
	logger.WithError(err).Warn("debug: fallback report upload failed")

	recent := buf.Recent(5)
	if len(recent) != 1 {
		t.Fatalf("want 1 problem line, got %d", len(recent))
	}
	line := recent[0]
	if !strings.Contains(line, "fallback report upload failed") {
		t.Fatalf("problem line missing message: %q", line)
	}
	if strings.Contains(line, "sekretsubmitkey") {
		t.Fatalf("problem line leaks the submit key: %q", line)
	}
}
