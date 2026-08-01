package daemon

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"time"
)

// support bundle

const reportUploadTimeout = 30 * time.Second

func (app *App) DebugBundle() []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	add := func(name string, data []byte) {
		if len(data) == 0 {
			data = []byte("(empty)\n")
		}
		_ = tw.WriteHeader(&tar.Header{
			Name: name, Mode: 0o644, Size: int64(len(data)), ModTime: time.Now(),
		})
		_, _ = tw.Write(data)
	}
	run := func(timeout time.Duration, name string, args ...string) []byte {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
		if err != nil {
			return append(out, []byte("\n(error: "+err.Error()+")\n")...)
		}
		return out
	}
	file := func(path string) []byte {
		b, err := os.ReadFile(path)
		if err != nil {
			return []byte("(unreadable: " + err.Error() + ")\n")
		}
		return b
	}
	lines := func(ls []string) []byte {
		var b bytes.Buffer
		for _, l := range ls {
			b.WriteString(l)
			b.WriteByte('\n')
		}
		return b.Bytes()
	}

	if status, err := json.MarshalIndent(app.DebugStatus(), "", "  "); err == nil {
		add("status.json", status)
	}
	add("daemon-log.txt", lines(tailFile(daemonLogPath, 500)))
	add("chromium-log.txt", lines(tailFile("/var/log/chromium/current", 200)))
	add("weston-log.txt", lines(tailFile("/var/log/weston/current", 100)))
	add("dmesg.txt", run(5*time.Second, "sh", "-c", "dmesg | tail -n 300"))
	add("ip-addr.txt", run(3*time.Second, "ip", "addr"))
	add("ip-route.txt", run(3*time.Second, "ip", "route"))
	add("resolv.conf", file("/var/local/etc/resolv.conf"))
	add("services.txt", run(5*time.Second, "sh", "-c", "sv status /var/service/* 2>/dev/null || sv status /etc/service/* 2>/dev/null"))
	add("problems-previous-run.txt", lines(PreviousProblems(20)))

	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

// upload the bundle and return id
func (app *App) SendReport() (string, error) {
	if app.cfg.ReportURL == "" {
		return "", fmt.Errorf("reporting not configured")
	}
	bundle := app.DebugBundle()
	id, err := app.uploadBundle(bundle)
	if err != nil {
		return "", err
	}
	app.log.Infof("debug: support bundle uploaded, id %s (%d bytes)", id, len(bundle))
	return id, nil
}

func (app *App) uploadBundle(bundle []byte) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), reportUploadTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", app.cfg.ReportURL, bytes.NewReader(bundle))
	if err != nil {
		return "", fmt.Errorf("report upload: invalid report_url")
	}
	req.Header.Set("Content-Type", "application/gzip")
	resp, err := app.client.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return "", fmt.Errorf("report upload: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("report upload failed: %d", resp.StatusCode)
	}
	var out struct {
		Id string `json:"id"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&out); err != nil {
		return "", fmt.Errorf("report upload: bad response: %w", err)
	}
	if out.Id == "" {
		return "", fmt.Errorf("report upload: no id in response")
	}
	return out.Id, nil
}
