package bluetooth

// iAP2 sidecar supervisor, for ios volume controls

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"

	librespot "github.com/devgianlu/go-librespot"
)

const iap2ServiceUUID = "00000000-deca-fade-deca-deafdecacafe"

var iap2SidecarPaths = []string{
	"/usr/bin/iap2-sidecar",
	"/var/local/iap2-sidecar",
}

type iap2Volume struct {
	log  librespot.Logger
	path string

	mu      sync.Mutex
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	state   string
	addr    string
	want    string
	stopped bool
}

func newIap2Volume(log librespot.Logger) *iap2Volume {
	for _, p := range iap2SidecarPaths {
		if _, err := os.Stat(p); err == nil {
			log.Infof("bluetooth: iAP2 sidecar found at %s", p)
			return &iap2Volume{log: log, path: p}
		}
	}
	// missing sidecar
	log.Warn("bluetooth: no iap2-sidecar binary, iPhone volume DISABLED (firmware build issue)")
	return nil
}

// spawns the sidecar if needed
func (v *iap2Volume) ensureRunning() error {
	if v.cmd != nil {
		return nil
	}
	if v.stopped {
		return fmt.Errorf("iap2 supervisor closed")
	}
	cmd := exec.Command(v.path)
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("iap2 stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("iap2 stdout: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("iap2 spawn: %w", err)
	}
	v.cmd = cmd
	v.stdin = stdin
	v.log.Infof("bluetooth: iap2-sidecar started (pid %d)", cmd.Process.Pid)

	go v.pump(stdout, cmd)
	return nil
}

func (v *iap2Volume) pump(stdout io.Reader, cmd *exec.Cmd) {
	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		var ev struct {
			Event string `json:"event"`
			State string `json:"state"`
			Addr  string `json:"addr"`
			Error string `json:"error"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &ev); err != nil {
			v.log.Debugf("bluetooth: iap2 unparseable event: %s", scanner.Text())
			continue
		}
		switch ev.Event {
		case "state":
			v.mu.Lock()
			v.state = ev.State
			v.addr = ev.Addr
			v.mu.Unlock()
			v.log.Infof("bluetooth: iap2 session %s (%s)", ev.State, ev.Addr)
		case "error":
			v.log.Warnf("bluetooth: iap2 sidecar: %s", ev.Error)
		}
	}
	err := cmd.Wait()

	v.mu.Lock()
	v.cmd = nil
	v.stdin = nil
	v.state = ""
	want, stopped := v.want, v.stopped
	v.mu.Unlock()
	v.log.WithError(err).Warn("bluetooth: iap2-sidecar exited")

	if want != "" && !stopped {
		time.Sleep(3 * time.Second)
		v.EnsureSession(want)
	}
}

// writes one JSON command line
func (v *iap2Volume) send(cmd map[string]any) error {
	if v.stdin == nil {
		return fmt.Errorf("iap2 sidecar not running")
	}
	b, err := json.Marshal(cmd)
	if err != nil {
		return err
	}
	_, err = v.stdin.Write(append(b, '\n'))
	return err
}

func (v *iap2Volume) EnsureSession(addr string) {
	if v == nil {
		return
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	v.want = addr
	if err := v.ensureRunning(); err != nil {
		v.log.WithError(err).Warn("bluetooth: iap2 sidecar unavailable")
		return
	}
	if err := v.send(map[string]any{"cmd": "connect", "addr": addr, "channel": 1}); err != nil {
		v.log.WithError(err).Warn("bluetooth: iap2 connect command failed")
	}
}

func (v *iap2Volume) DropSession(addr string) {
	if v == nil {
		return
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if addr != "" && v.want != addr {
		return
	}
	v.want = ""
	if v.stdin != nil {
		_ = v.send(map[string]any{"cmd": "disconnect"})
	}
}

// delivers a signed volume signal over the iAP2 session
func (v *iap2Volume) SendVolumeSteps(steps int) bool {
	if v == nil {
		return false
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.state != "connected" {
		return false
	}
	if err := v.send(map[string]any{"cmd": "volume", "steps": steps}); err != nil {
		v.log.WithError(err).Warn("bluetooth: iap2 volume command failed")
		return false
	}
	return true
}

// reports whether an iAP2 session is up
func (v *iap2Volume) Connected() bool {
	if v == nil {
		return false
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.state == "connected"
}

func (v *iap2Volume) Close() {
	if v == nil {
		return
	}
	v.mu.Lock()
	v.stopped = true
	v.want = ""
	cmd := v.cmd
	stdin := v.stdin
	v.mu.Unlock()
	if stdin != nil {
		_ = stdin.Close()
	}
	if cmd == nil {
		return
	}
	for i := 0; i < 20; i++ {
		v.mu.Lock()
		gone := v.cmd == nil
		v.mu.Unlock()
		if gone {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	_ = cmd.Process.Kill()
}
