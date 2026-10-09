package main

import (
	"strings"
	"sync"
)

// tailSize bounds the output kept for the journal: mariadb-backup writes a
// line per table, the whole output goes to the log file of the copy instead
const tailSize = 16 << 10

// tailBuffer keeps the last bytes written to it
type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func newTailBuffer() *tailBuffer {
	return &tailBuffer{}
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.buf = append(t.buf, p...)
	if len(t.buf) > tailSize {
		t.buf = append([]byte(nil), t.buf[len(t.buf)-tailSize:]...)
	}
	return len(p), nil
}

// lines returns up to n last non-empty lines
func (t *tailBuffer) lines(n int) []string {
	t.mu.Lock()
	defer t.mu.Unlock()

	var lines []string
	for _, line := range strings.Split(string(t.buf), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines
}
