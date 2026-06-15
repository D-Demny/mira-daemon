package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// backlightConfPath is the key=value file the firmware auto_brightness service reads
const backlightConfPath = "/var/local/etc/backlight.conf"

// renderBacklightConf extracts the brightness-related keys from the settings
func renderBacklightConf(blob []byte) (conf string, ok bool) {
	var s struct {
		Auto       *bool `json:"autoBrightness"`
		Brightness *int  `json:"brightness"`
	}
	if err := json.Unmarshal(blob, &s); err != nil {
		return "", false
	}
	if s.Auto == nil && s.Brightness == nil {
		return "", false
	}

	auto, brightness := 1, 8
	if s.Auto != nil && !*s.Auto {
		auto = 0
	}
	if s.Brightness != nil {
		brightness = min(max(*s.Brightness, 1), 10)
	}
	return fmt.Sprintf("AUTO=%d\nBRIGHTNESS=%d\n", auto, brightness), true
}

// mirrorBacklightConf writes the conf atomically
func (app *App) mirrorBacklightConf(blob []byte) {
	conf, ok := renderBacklightConf(blob)
	if !ok {
		return
	}
	if existing, err := os.ReadFile(backlightConfPath); err == nil && string(existing) == conf {
		return
	}
	dir := filepath.Dir(backlightConfPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		app.log.WithError(err).Debug("backlight conf dir unavailable (dev machine?)")
		return
	}
	tmp, err := os.CreateTemp(dir, "backlight-*.conf")
	if err != nil {
		app.log.WithError(err).Debug("backlight conf not writable (dev machine?)")
		return
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err = tmp.WriteString(conf); err == nil {
		err = tmp.Chmod(0o644)
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), backlightConfPath)
	}
	if err != nil {
		app.log.WithError(err).Warn("failed to write backlight conf")
		return
	}

	// the service picks stock-vs-manual at startup
	if err := exec.Command("sv", "restart", "auto_brightness").Run(); err != nil {
		app.log.WithError(err).Debug("auto_brightness restart unavailable (dev machine?)")
	}
}
