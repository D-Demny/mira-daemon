package daemon

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// grapheme->phoneme maps text to a flat IPA phoneme string for the phonetic rerank
type g2p struct {
	loader   string
	libDir   string
	bin      string
	dataPath string
	mu       sync.RWMutex
	memo     map[string]string
	probe    sync.Once
	ok       bool
	override func(string) string
}

const stressMarks = "ˈˌ"

func newG2P(loader, libDir, bin, dataPath string) *g2p {
	return &g2p{
		loader:   loader,
		libDir:   libDir,
		bin:      bin,
		dataPath: dataPath,
		memo:     make(map[string]string),
	}
}

// returns the cached or freshly computed flat IPA phoneme string for text
func (g *g2p) ipa(ctx context.Context, text string) string {
	key := strings.ToLower(strings.TrimSpace(text))
	if key == "" {
		return ""
	}
	g.mu.RLock()
	if v, ok := g.memo[key]; ok {
		g.mu.RUnlock()
		return v
	}
	g.mu.RUnlock()

	if g.override != nil {
		v := g.override(key)
		g.mu.Lock()
		g.memo[key] = v
		g.mu.Unlock()
		return v
	}

	out, err := g.runEspeak(ctx, key)
	if err != nil {
		return ""
	}
	v := cleanIPA(out)
	g.mu.Lock()
	g.memo[key] = v
	g.mu.Unlock()
	return v
}

// reduces a name to letters/digits/space/apostrophe, lowercased, single-spaced
func espeakNorm(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	b.Grow(len(s))
	lastSpace := false
	for _, r := range s {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '\'':
			b.WriteRune(r)
			lastSpace = false
		default:
			if !lastSpace {
				b.WriteByte(' ')
				lastSpace = true
			}
		}
	}
	out := strings.TrimSpace(b.String())
	if out == "" {
		return "x"
	}
	return out
}

func (g *g2p) runEspeak(ctx context.Context, text string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	args := []string{"--library-path", g.libDir, g.bin, "-q", "--ipa", "-v", "en-us"}
	if g.dataPath != "" {
		args = append(args, "--path="+g.dataPath)
	}
	args = append(args, espeakNorm(text))

	cmd := exec.CommandContext(cctx, g.loader, args...)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("espeak-ng: %w (%s)", err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

const g2pChunk = 512

// phonemizes many texts in batched espeak invocations
func (g *g2p) ipaMany(ctx context.Context, texts []string) map[string]string {
	out := make(map[string]string, len(texts))

	var pending []string
	seen := make(map[string]bool)
	g.mu.RLock()
	for _, t := range texts {
		k := strings.ToLower(strings.TrimSpace(t))
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		if v, ok := g.memo[k]; ok {
			out[k] = v
			continue
		}
		pending = append(pending, k)
	}
	g.mu.RUnlock()

	if g.override != nil {
		for _, k := range pending {
			v := g.override(k)
			out[k] = v
			g.mu.Lock()
			g.memo[k] = v
			g.mu.Unlock()
		}
		return out
	}

	for i := 0; i < len(pending); i += g2pChunk {
		if ctx.Err() != nil {
			break
		}
		end := i + g2pChunk
		if end > len(pending) {
			end = len(pending)
		}
		chunk := pending[i:end]
		phon, err := g.runEspeakBatch(ctx, chunk)
		if err != nil || len(phon) != len(chunk) {
			for _, k := range chunk {
				out[k] = g.ipa(ctx, k)
			}
			continue
		}
		g.mu.Lock()
		for j, k := range chunk {
			out[k] = phon[j]
			g.memo[k] = phon[j]
		}
		g.mu.Unlock()
	}
	return out
}

// phonemizes a chunk in one espeak invocation
func (g *g2p) runEspeakBatch(ctx context.Context, texts []string) ([]string, error) {
	cctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()

	var in bytes.Buffer
	for _, t := range texts {
		in.WriteString(espeakNorm(t))
		in.WriteByte('\n')
	}

	args := []string{"--library-path", g.libDir, g.bin, "-q", "--ipa", "-v", "en-us"}
	if g.dataPath != "" {
		args = append(args, "--path="+g.dataPath)
	}
	cmd := exec.CommandContext(cctx, g.loader, args...)
	cmd.Stdin = &in
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("espeak-ng batch: %w (%s)", err, strings.TrimSpace(errb.String()))
	}

	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != len(texts) {
		return nil, fmt.Errorf("espeak batch misaligned: %d lines for %d names", len(lines), len(texts))
	}
	res := make([]string, len(texts))
	for i, l := range lines {
		res[i] = cleanIPA(l)
	}
	return res, nil
}

// probes espeak-ng once and caches the result
func (g *g2p) available(ctx context.Context) bool {
	g.probe.Do(func() {
		s := g.ipa(ctx, "test")
		g.ok = s != ""
	})
	return g.ok
}

// flattens espeak output to a stress/whitespace-free phoneme string
func cleanIPA(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r == ' ' || r == '\n' || r == '\r' || r == '\t' {
			continue
		}
		if strings.ContainsRune(stressMarks, r) {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func defaultEspeakDataParent(binDir string) string {
	return filepath.Dir(strings.TrimRight(binDir, "/"))
}
