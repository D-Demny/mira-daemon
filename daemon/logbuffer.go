package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"

	"github.com/sirupsen/logrus"
)

// most recent WARN/ERROR/FATAL log lines
type LogBuffer struct {
	mu    sync.Mutex
	lines []string
	max   int
	path  string
}

func NewLogBuffer(max int) *LogBuffer {
	if max <= 0 {
		max = 20
	}
	return &LogBuffer{max: max, lines: make([]string, 0, max)}
}

var (
	globalLogBuffer  *LogBuffer
	previousProblems []string
)

const problemsPersistPath = "/var/local/mira/problems.json"

func InstallLogBuffer(logger *logrus.Logger) {
	previousProblems = loadPersistedProblems(problemsPersistPath)
	globalLogBuffer = NewLogBuffer(20)
	globalLogBuffer.path = problemsPersistPath
	logger.AddHook(globalLogBuffer)
}

func RecentProblems(n int) []string {
	if globalLogBuffer == nil {
		return nil
	}
	return globalLogBuffer.Recent(n)
}

func PreviousProblems(n int) []string {
	if n <= 0 || n > len(previousProblems) {
		n = len(previousProblems)
	}
	out := make([]string, 0, n)
	for i := len(previousProblems) - 1; i >= 0 && len(out) < n; i-- {
		out = append(out, previousProblems[i])
	}
	return out
}

func loadPersistedProblems(path string) []string {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var lines []string
	if json.Unmarshal(b, &lines) != nil {
		return nil
	}
	return lines
}

func (b *LogBuffer) Levels() []logrus.Level {
	return []logrus.Level{logrus.WarnLevel, logrus.ErrorLevel, logrus.FatalLevel, logrus.PanicLevel}
}

func (b *LogBuffer) Fire(e *logrus.Entry) error {
	lvl := e.Level.String()
	msg := e.Message
	if err, ok := e.Data[logrus.ErrorKey]; ok && err != nil {
		msg = msg + ": " + toStr(err)
	}
	line := e.Time.Format("15:04:05") + " " + lvl + " " + msg
	b.mu.Lock()
	b.lines = append(b.lines, line)
	if len(b.lines) > b.max {
		b.lines = b.lines[len(b.lines)-b.max:]
	}
	snapshot := append([]string(nil), b.lines...)
	path := b.path
	b.mu.Unlock()
	if path != "" {
		if data, err := json.Marshal(snapshot); err == nil {
			_ = os.MkdirAll(filepath.Dir(path), 0o755)
			tmp := path + ".tmp"
			if os.WriteFile(tmp, data, 0o644) == nil {
				_ = os.Rename(tmp, path)
			}
		}
	}
	return nil
}

func (b *LogBuffer) Recent(n int) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if n <= 0 || n > len(b.lines) {
		n = len(b.lines)
	}
	out := make([]string, 0, n)
	for i := len(b.lines) - 1; i >= 0 && len(out) < n; i-- {
		out = append(out, b.lines[i])
	}
	return out
}

func toStr(v any) string {
	if s, ok := v.(interface{ Error() string }); ok {
		return s.Error()
	}
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}
