package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestValidateUserDataReturnsHostname(t *testing.T) {
	data := []byte(`#cloud-config
autoinstall:
  version: 1
  identity:
    hostname: test-host
`)

	hostname, err := validateUserData(data)
	if err != nil {
		t.Fatalf("validateUserData returned error: %v", err)
	}
	if hostname != "test-host" {
		t.Fatalf("hostname = %q, want %q", hostname, "test-host")
	}
}

func TestValidateUserDataRequiresAutoinstall(t *testing.T) {
	_, err := validateUserData([]byte("not_autoinstall: true\n"))
	if err == nil {
		t.Fatal("validateUserData returned nil error for missing autoinstall")
	}
}

func TestParseDiskSize(t *testing.T) {
	tests := map[string]int64{
		"1024": 1024,
		"1K":   1024,
		"2M":   2 * 1024 * 1024,
		"3G":   3 * 1024 * 1024 * 1024,
		"1GiB": 1024 * 1024 * 1024,
		"1gib": 1024 * 1024 * 1024,
		"1GB":  1024 * 1024 * 1024,
	}

	for input, want := range tests {
		got, err := parseDiskSize(input)
		if err != nil {
			t.Fatalf("parseDiskSize(%q) returned error: %v", input, err)
		}
		if got != want {
			t.Fatalf("parseDiskSize(%q) = %d, want %d", input, got, want)
		}
	}

	if _, err := parseDiskSize("bad"); err == nil {
		t.Fatal("parseDiskSize returned nil error for invalid size")
	}
}

func TestCheckOutputPathRejectsExistingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "existing.img")
	if err := os.WriteFile(path, []byte("exists"), 0o644); err != nil {
		t.Fatalf("create temp file: %v", err)
	}

	if err := checkOutputPath(path); err == nil {
		t.Fatal("checkOutputPath returned nil error for existing output file")
	}
}

func TestCheckOutputPathAcceptsWritableParent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "new.img")

	if err := checkOutputPath(path); err != nil {
		t.Fatalf("checkOutputPath returned error: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("checkOutputPath created output path or returned unexpected stat error: %v", err)
	}
}
