package qemulog

import (
	"bytes"
	"testing"
)

func TestCompactingWriterCollapsesBlankRuns(t *testing.T) {
	var out bytes.Buffer
	writer := NewCompactingWriter(&out)

	if _, err := writer.Write([]byte("first\n\n\nsecond\n")); err != nil {
		t.Fatalf("Write returned error: %v", err)
	}
	if err := writer.Flush(); err != nil {
		t.Fatalf("Flush returned error: %v", err)
	}

	want := "first\n\nsecond\n"
	if got := out.String(); got != want {
		t.Fatalf("compacted output = %q, want %q", got, want)
	}
}

func TestCompactingWriterCollapsesLeadingBlankRun(t *testing.T) {
	var out bytes.Buffer
	writer := NewCompactingWriter(&out)

	if _, err := writer.Write([]byte("\n\n\n[    0.000000] Linux version\n")); err != nil {
		t.Fatalf("Write returned error: %v", err)
	}

	want := "\n[    0.000000] Linux version\n"
	if got := out.String(); got != want {
		t.Fatalf("compacted output = %q, want %q", got, want)
	}
}

func TestCompactingWriterTreatsWhitespaceAndTerminalControlAsBlank(t *testing.T) {
	var out bytes.Buffer
	writer := NewCompactingWriter(&out)

	input := []byte("start\n  \t \n\x1b[2J\x1b[H\n\n\x1b[31merror\x1b[0m\n")
	if _, err := writer.Write(input); err != nil {
		t.Fatalf("Write returned error: %v", err)
	}

	want := "start\n\nerror\n"
	if got := out.String(); got != want {
		t.Fatalf("compacted output = %q, want %q", got, want)
	}
}

func TestCompactingWriterStripsTerminalControlsBeforeVisibleText(t *testing.T) {
	var out bytes.Buffer
	writer := NewCompactingWriter(&out)

	if _, err := writer.Write([]byte("\x1b[2J\x1b[H[    0.000000] Linux version\n")); err != nil {
		t.Fatalf("Write returned error: %v", err)
	}

	want := "[    0.000000] Linux version\n"
	if got := out.String(); got != want {
		t.Fatalf("compacted output = %q, want %q", got, want)
	}
	if bytes.Contains(out.Bytes(), []byte{0x1b}) {
		t.Fatalf("compacted output still contains escape bytes: %q", out.String())
	}
}

func TestCompactingWriterStripsTerminalReset(t *testing.T) {
	var out bytes.Buffer
	writer := NewCompactingWriter(&out)

	if _, err := writer.Write([]byte("before\n\x1bcafter\n")); err != nil {
		t.Fatalf("Write returned error: %v", err)
	}

	want := "before\nafter\n"
	if got := out.String(); got != want {
		t.Fatalf("compacted output = %q, want %q", got, want)
	}
}

func TestCompactingWriterStripsColorSequences(t *testing.T) {
	var out bytes.Buffer
	writer := NewCompactingWriter(&out)

	if _, err := writer.Write([]byte("\x1b[31merror\x1b[0m\n")); err != nil {
		t.Fatalf("Write returned error: %v", err)
	}

	want := "error\n"
	if got := out.String(); got != want {
		t.Fatalf("compacted output = %q, want %q", got, want)
	}
	if bytes.Contains(out.Bytes(), []byte{0x1b}) {
		t.Fatalf("compacted output still contains escape bytes: %q", out.String())
	}
}

func TestCompactingWriterNormalizesCarriageReturns(t *testing.T) {
	var out bytes.Buffer
	writer := NewCompactingWriter(&out)

	if _, err := writer.Write([]byte("first\r\r\r\nsecond\rthird")); err != nil {
		t.Fatalf("Write returned error: %v", err)
	}
	if err := writer.Flush(); err != nil {
		t.Fatalf("Flush returned error: %v", err)
	}

	want := "first\n\nsecond\nthird\n"
	if got := out.String(); got != want {
		t.Fatalf("compacted output = %q, want %q", got, want)
	}
}
