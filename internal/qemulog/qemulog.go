package qemulog

import (
	"bytes"
	"io"
	"sync"
)

var newline = []byte("\n")

type CompactingWriter struct {
	mu         sync.Mutex
	out        io.Writer
	line       []byte
	skipNextLF bool
	blank      bool
}

func NewCompactingWriter(out io.Writer) *CompactingWriter {
	return &CompactingWriter{out: out}
}

func (w *CompactingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	for idx, b := range p {
		if w.skipNextLF {
			w.skipNextLF = false
			if b == '\n' {
				continue
			}
		}

		switch b {
		case '\r':
			if err := w.emitBufferedLine(); err != nil {
				return idx, err
			}
			w.skipNextLF = true
		case '\n':
			if err := w.emitBufferedLine(); err != nil {
				return idx, err
			}
		default:
			w.line = append(w.line, b)
		}
	}

	return len(p), nil
}

func (w *CompactingWriter) Flush() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if len(w.line) == 0 {
		return nil
	}
	return w.emitBufferedLine()
}

func (w *CompactingWriter) emitBufferedLine() error {
	line := w.line
	w.line = w.line[:0]
	return w.emitLine(line)
}

func (w *CompactingWriter) emitLine(line []byte) error {
	if !hasVisibleContent(line) {
		if w.blank {
			return nil
		}
		w.blank = true
		_, err := w.out.Write(newline)
		return err
	}

	if _, err := w.out.Write(line); err != nil {
		return err
	}
	if _, err := w.out.Write(newline); err != nil {
		return err
	}
	w.blank = false
	return nil
}

func hasVisibleContent(line []byte) bool {
	for idx := 0; idx < len(line); {
		b := line[idx]
		switch {
		case b == 0x1b:
			idx = skipEscape(line, idx)
		case b <= ' ' || b == 0x7f:
			idx++
		default:
			return true
		}
	}
	return false
}

func skipEscape(line []byte, idx int) int {
	if idx+1 >= len(line) {
		return idx + 1
	}

	switch line[idx+1] {
	case '[':
		return skipCSI(line, idx+2)
	case ']':
		return skipOSC(line, idx+2)
	default:
		return idx + 2
	}
}

func skipCSI(line []byte, idx int) int {
	for idx < len(line) {
		if line[idx] >= 0x40 && line[idx] <= 0x7e {
			return idx + 1
		}
		idx++
	}
	return idx
}

func skipOSC(line []byte, idx int) int {
	for idx < len(line) {
		if line[idx] == 0x07 {
			return idx + 1
		}
		if bytes.HasPrefix(line[idx:], []byte{0x1b, '\\'}) {
			return idx + 2
		}
		idx++
	}
	return idx
}
