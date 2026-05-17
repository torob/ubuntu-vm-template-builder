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

	want := "start\n\n\x1b[31merror\x1b[0m\n"
	if got := out.String(); got != want {
		t.Fatalf("compacted output = %q, want %q", got, want)
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
