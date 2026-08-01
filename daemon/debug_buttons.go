package daemon

import (
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// for when chromium is dead

// evKey is shared with the uinput constants in app.go
const (
	keyPreset1    = 2 // KEY_1
	keyPreset4    = 5 // KEY_4
	chordHoldTime = 3 * time.Second
	chordCooldown = 60 * time.Second
)

func findGpioKeys() string {
	names, _ := filepath.Glob("/sys/class/input/event*/device/name")
	for _, p := range names {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		if strings.TrimSpace(string(b)) == "gpio-keys" {
			return "/dev/input/" + filepath.Base(filepath.Dir(filepath.Dir(p)))
		}
	}
	return ""
}

func (app *App) startButtonFallback() {
	if app.cfg.ReportURL == "" {
		return
	}
	dev := findGpioKeys()
	if dev == "" {
		app.log.Debugf("debug: no gpio-keys device, report chord fallback disabled")
		return
	}
	go app.watchReportChord(dev)
}

func (app *App) watchReportChord(dev string) {
	tvSize := int(unsafe.Sizeof(syscall.Timeval{}))
	buf := make([]byte, tvSize+8)

	var lastUpload time.Time
	for {
		f, err := os.Open(dev)
		if err != nil {
			app.log.WithError(err).Debugf("debug: cannot open %s, retrying", dev)
			time.Sleep(30 * time.Second)
			continue
		}
		app.log.Debugf("debug: watching %s for the 1+4 report chord", dev)

		var (
			mu        sync.Mutex
			p1, p4    bool
			holdTimer *time.Timer
		)
		fire := func() {
			if app.server.WSClients() > 0 {
				return // UI is alive
			}
			// claim the upload slot under the lock
			mu.Lock()
			ok := p1 && p4 && time.Since(lastUpload) >= chordCooldown
			if ok {
				lastUpload = time.Now()
			}
			mu.Unlock()
			if !ok {
				return
			}
			app.log.Warnf("debug: 1+4 held with no UI connected, uploading support bundle")
			if id, err := app.SendReport(); err != nil {
				app.log.WithError(err).Warnf("debug: fallback report upload failed")
			} else {
				app.log.Warnf("debug: fallback report uploaded, id %s", id)
			}
		}

		for {
			if _, err := io.ReadFull(f, buf); err != nil {
				break
			}
			typ := binary.LittleEndian.Uint16(buf[tvSize:])
			code := binary.LittleEndian.Uint16(buf[tvSize+2:])
			value := int32(binary.LittleEndian.Uint32(buf[tvSize+4:]))
			if typ != evKey || (code != keyPreset1 && code != keyPreset4) {
				continue
			}
			pressed := value != 0
			mu.Lock()
			if code == keyPreset1 {
				p1 = pressed
			} else {
				p4 = pressed
			}
			if p1 && p4 {
				if holdTimer == nil {
					holdTimer = time.AfterFunc(chordHoldTime, fire)
				}
			} else if holdTimer != nil {
				holdTimer.Stop()
				holdTimer = nil
			}
			mu.Unlock()
		}
		mu.Lock()
		if holdTimer != nil {
			holdTimer.Stop()
			holdTimer = nil
		}
		mu.Unlock()
		_ = f.Close()
		time.Sleep(5 * time.Second)
	}
}
