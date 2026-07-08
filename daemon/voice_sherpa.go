package daemon

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// manages a single long-lived sherpa-onnx ASR process (for the gigaspeech-Zipformer transducer to prevent long load times each time we want to run a command)
type sherpaSidecar struct {
	v *voiceService

	mu    sync.Mutex
	stdin io.WriteCloser
	proc  *os.Process
	lines chan string

	readyMu sync.Mutex
	ready   bool
	readyCh chan struct{}
}

const (
	sherpaThreads     = 4
	sherpaMethod      = "greedy_search"
	sherpaReadyWait   = 40 * time.Second
	sherpaDecodeMax   = 20 * time.Second
	sherpaRestartWait = 2 * time.Second
	sherpaMinFreeMB   = 30
)

func newSherpaSidecar(v *voiceService) *sherpaSidecar {
	return &sherpaSidecar{v: v, lines: make(chan string, 1), readyCh: make(chan struct{})}
}

// start launches the supervisor
func (s *sherpaSidecar) start() { go s.supervise() }

func (s *sherpaSidecar) setReady(r bool) {
	s.readyMu.Lock()
	switch {
	case r && !s.ready:
		close(s.readyCh)
	case !r && s.ready:
		s.readyCh = make(chan struct{})
	}
	s.ready = r
	s.readyMu.Unlock()
}

func (s *sherpaSidecar) waitReady(ctx context.Context) error {
	s.readyMu.Lock()
	if s.ready {
		s.readyMu.Unlock()
		return nil
	}
	ch := s.readyCh
	s.readyMu.Unlock()
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// spawns or respawns the sidecar
func (s *sherpaSidecar) supervise() {
	for s.v.ctx.Err() == nil {
		if err := s.runOnce(); err != nil && s.v.ctx.Err() == nil {
			s.v.app.log.WithError(err).Warn("voice: sherpa sidecar exited; restarting")
		}
		s.setReady(false)
		select {
		case <-time.After(sherpaRestartWait):
		case <-s.v.ctx.Done():
			return
		}
	}
}

func (s *sherpaSidecar) runOnce() error {
	md := s.v.cfg.SherpaModelDir
	enc, err := globOne(md, "encoder*int8.onnx")
	if err != nil {
		return err
	}
	dec, err := globOne(md, "decoder*int8.onnx")
	if err != nil {
		return err
	}
	joi, err := globOne(md, "joiner*int8.onnx")
	if err != nil {
		return err
	}
	tokens := filepath.Join(md, "tokens.txt")

	bin := filepath.Join(s.v.cfg.BinDir, s.v.cfg.SherpaBin)
	args := []string{"--library-path", s.v.cfg.LibDir, bin,
		enc, dec, joi, tokens, fmt.Sprintf("%d", sherpaThreads), sherpaMethod}
	cmd := exec.CommandContext(s.v.ctx, s.v.loader(), args...)
	// die with the daemon
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	s.v.app.log.Infof("voice: sherpa sidecar starting (model=%s, loading ~20s)", filepath.Base(enc))

	s.mu.Lock()
	s.stdin = stdin
	s.proc = cmd.Process
	s.mu.Unlock()

	go drainLog(s.v, stderr)

	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if strings.TrimSpace(line) == "READY" && !s.isReady() {
			s.setReady(true)
			s.v.app.log.Info("voice: sherpa sidecar READY (model loaded)")
			continue
		}
		select {
		case s.lines <- line:
		default: // no waiter
		}
	}
	_ = cmd.Wait()
	s.mu.Lock()
	s.stdin = nil
	s.proc = nil
	s.mu.Unlock()
	return fmt.Errorf("sherpa sidecar process ended")
}

func (s *sherpaSidecar) isReady() bool {
	s.readyMu.Lock()
	defer s.readyMu.Unlock()
	return s.ready
}

// forces the given sidecar process down so the supervisor respawns a fresh one
func (s *sherpaSidecar) killProc(p *os.Process) {
	if p == nil {
		return
	}
	s.mu.Lock()
	cur := s.proc
	s.mu.Unlock()
	if cur == p {
		_ = p.Kill()
	}
}

func drainLog(v *voiceService, r io.Reader) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		v.app.log.Debugf("voice: sherpa: %s", sc.Text())
	}
}

func globOne(dir, pat string) (string, error) {
	m, err := filepath.Glob(filepath.Join(dir, pat))
	if err != nil || len(m) == 0 {
		return "", fmt.Errorf("sherpa model %q not found in %s", pat, dir)
	}
	return m[0], nil
}

// returns the available memory
func availMemMB() int {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return -1
	}
	for _, line := range strings.Split(string(b), "\n") {
		if rest, ok := strings.CutPrefix(line, "MemAvailable:"); ok {
			if f := strings.Fields(rest); len(f) >= 1 {
				if kb, err := strconv.Atoi(f[0]); err == nil {
					return kb / 1024
				}
			}
		}
	}
	return -1
}

// transcribe sends clipPath to the recognizer and returns the transcript
func (s *sherpaSidecar) transcribe(ctx context.Context, clipPath string) (string, error) {
	rctx, cancel := context.WithTimeout(ctx, sherpaReadyWait)
	defer cancel()
	if err := s.waitReady(rctx); err != nil {
		return "", fmt.Errorf("sherpa not ready: %w", err)
	}
	// if mem is low (potential for leaks nott really tested extensively)
	if free := availMemMB(); free >= 0 && free < sherpaMinFreeMB {
		return "", fmt.Errorf("sherpa: low memory (%dMB free < %dMB floor), abstaining to avoid OOM", free, sherpaMinFreeMB)
	}

	s.mu.Lock()
	stdin := s.stdin
	proc := s.proc
	s.mu.Unlock()
	if stdin == nil {
		return "", fmt.Errorf("sherpa stdin unavailable")
	}

	select {
	case <-s.lines:
	default:
	}

	if _, err := io.WriteString(stdin, clipPath+"\n"); err != nil {
		return "", fmt.Errorf("sherpa write: %w", err)
	}

	dctx, dcancel := context.WithTimeout(ctx, sherpaDecodeMax)
	defer dcancel()
	select {
	case line, ok := <-s.lines:
		if !ok {
			return "", fmt.Errorf("sherpa sidecar closed")
		}
		return cleanTranscript(line), nil
	case <-dctx.Done():
		s.killProc(proc)
		return "", fmt.Errorf("sherpa decode timeout: %w", dctx.Err())
	}
}
