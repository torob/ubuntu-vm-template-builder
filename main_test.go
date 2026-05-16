package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
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

func TestRunInterruptibleCommandStopsProcessOnInterrupt(t *testing.T) {
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("sleep command is not available")
	}

	cmd := exec.Command("sleep", "30")
	configureCommandProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep command: %v", err)
	}

	signals := make(chan os.Signal, 1)
	errCh := make(chan error, 1)
	go func() {
		errCh <- waitInterruptibleCommand(cmd, signals, 2*time.Second)
	}()

	signals <- os.Interrupt

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("runInterruptibleCommand returned nil after interrupt")
		}
		if !strings.Contains(err.Error(), "interrupt") {
			t.Fatalf("interrupt error = %q, want mention of interrupt", err.Error())
		}
	case <-time.After(3 * time.Second):
		_ = signalCommandProcessGroup(cmd, syscall.SIGKILL)
		t.Fatal("command did not stop after interrupt")
	}
}

func TestQemuArgsUseNographicSerialConsole(t *testing.T) {
	installer := &Installer{
		ubuntuISO: "ubuntu.iso",
		imagePath: "output.img",
		diskFmt:   "raw",
	}

	args := installer.qemuArgs("seed.iso", "vmlinuz", "initrd")
	if !hasArg(args, "-nographic") {
		t.Fatalf("qemu args %v do not contain -nographic", args)
	}
	if hasArgPair(args, "-display", "none") {
		t.Fatalf("qemu args %v still contain -display none", args)
	}
	appendValue := valueAfterArg(args, "-append")
	if !strings.Contains(appendValue, "console=ttyS0,115200n8") {
		t.Fatalf("qemu -append value %q does not enable ttyS0 console", appendValue)
	}
}

func hasArg(args []string, name string) bool {
	for _, arg := range args {
		if arg == name {
			return true
		}
	}
	return false
}

func hasArgPair(args []string, name, value string) bool {
	for idx := 0; idx+1 < len(args); idx++ {
		if args[idx] == name && args[idx+1] == value {
			return true
		}
	}
	return false
}

func valueAfterArg(args []string, name string) string {
	for idx := 0; idx+1 < len(args); idx++ {
		if args[idx] == name {
			return args[idx+1]
		}
	}
	return ""
}

func TestIsPrerequisitesCommandAliases(t *testing.T) {
	for _, command := range []string{"prerequisites", "prereqs", "prerequests", "prequests"} {
		if !isPrerequisitesCommand(command) {
			t.Fatalf("isPrerequisitesCommand(%q) = false, want true", command)
		}
	}
	if isPrerequisitesCommand("install") {
		t.Fatal("isPrerequisitesCommand returned true for install")
	}
}

func TestParseOSRelease(t *testing.T) {
	values := parseOSRelease([]byte(`NAME="Ubuntu"
ID=ubuntu
ID_LIKE="debian"
VERSION_ID="26.04"
`))

	if values["NAME"] != "Ubuntu" {
		t.Fatalf("NAME = %q, want Ubuntu", values["NAME"])
	}
	if values["ID"] != "ubuntu" {
		t.Fatalf("ID = %q, want ubuntu", values["ID"])
	}
	if values["ID_LIKE"] != "debian" {
		t.Fatalf("ID_LIKE = %q, want debian", values["ID_LIKE"])
	}
}

func TestQemuInstallSuggestionDebianFamily(t *testing.T) {
	osInfo := OSInfo{
		GOOS:   "linux",
		ID:     "ubuntu",
		IDLike: []string{"debian"},
	}

	got := qemuInstallSuggestion(osInfo)
	if !strings.Contains(got, "apt install") || !strings.Contains(got, "qemu-system-x86") || !strings.Contains(got, "qemu-utils") {
		t.Fatalf("qemuInstallSuggestion returned %q, want apt qemu packages", got)
	}
}

func TestGoVersionAtLeast(t *testing.T) {
	tests := []struct {
		output string
		want   bool
	}{
		{output: "go version go1.26.2 linux/amd64", want: true},
		{output: "go version go1.27.0 linux/amd64", want: true},
		{output: "go version go1.25.9 linux/amd64", want: false},
		{output: "not go output", want: false},
	}

	for _, test := range tests {
		if got := goVersionAtLeast(test.output, 1, 26); got != test.want {
			t.Fatalf("goVersionAtLeast(%q) = %v, want %v", test.output, got, test.want)
		}
	}
}

func TestPrerequisiteReportRequiredOK(t *testing.T) {
	report := prerequisiteReport{
		Items: []prerequisiteItem{
			{Name: "required-ok", Required: true, OK: true},
			{Name: "optional-missing", Required: false, OK: false},
		},
	}
	if !report.RequiredOK() {
		t.Fatal("RequiredOK returned false when only optional prerequisite is missing")
	}

	report.Items = append(report.Items, prerequisiteItem{Name: "required-missing", Required: true, OK: false})
	if report.RequiredOK() {
		t.Fatal("RequiredOK returned true with a missing required prerequisite")
	}
}

func TestPrintPrerequisiteReportIncludesSuggestions(t *testing.T) {
	report := prerequisiteReport{
		OS: OSInfo{GOOS: "linux", ID: "debian", Name: "Debian"},
		Items: []prerequisiteItem{
			{
				Name:        "qemu-system-x86_64",
				Description: "QEMU system emulator.",
				Required:    true,
				OK:          false,
				Detail:      "not found in PATH",
				Suggestion:  "sudo apt install qemu-system-x86",
			},
		},
	}

	var out bytes.Buffer
	printPrerequisiteReport(&out, report)
	output := out.String()

	for _, want := range []string{
		"[MISSING] qemu-system-x86_64 (required)",
		"Fix: sudo apt install qemu-system-x86",
		"Install input prerequisites checked during normal install runs:",
		"One or more required host prerequisites are missing.",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("prerequisite report missing %q in:\n%s", want, output)
		}
	}
}

func TestPrintUsageMentionsPrerequisitesCommand(t *testing.T) {
	var out bytes.Buffer
	printUsage(&out, "install-ubuntu")
	output := out.String()

	if !strings.Contains(output, "install-ubuntu prerequisites") {
		t.Fatalf("usage does not mention prerequisites command:\n%s", output)
	}
	if !strings.Contains(output, "Aliases: prereqs, prerequests, prequests") {
		t.Fatalf("usage does not mention prerequisites aliases:\n%s", output)
	}
}
