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

func TestValidateUEFIPortableUserDataAcceptsDefaultStorageWithFallback(t *testing.T) {
	data := []byte(`#cloud-config
autoinstall:
  version: 1
  late-commands:
    - curtin in-target --target=/target -- grub-install --target=x86_64-efi --efi-directory=/boot/efi --bootloader-id=ubuntu --removable --recheck
`)

	if err := validateUEFIPortableUserData(data); err != nil {
		t.Fatalf("validateUEFIPortableUserData returned error: %v", err)
	}
}

func TestValidateUEFIPortableUserDataAcceptsCustomESPAndFallback(t *testing.T) {
	data := []byte(`#cloud-config
autoinstall:
  version: 1
  storage:
    config:
      - id: disk0
        type: disk
        ptable: gpt
      - id: part-efi
        type: partition
        device: disk0
        size: 512M
        flag: boot
      - id: fs-efi
        type: format
        volume: part-efi
        fstype: fat32
      - id: mount-efi
        type: mount
        device: fs-efi
        path: /boot/efi
  late-commands:
    - cp /target/boot/efi/EFI/ubuntu/grubx64.efi /target/boot/efi/EFI/BOOT/BOOTX64.EFI
`)

	if err := validateUEFIPortableUserData(data); err != nil {
		t.Fatalf("validateUEFIPortableUserData returned error: %v", err)
	}
}

func TestValidateUEFIPortableUserDataRejectsMissingFallback(t *testing.T) {
	data := []byte(`#cloud-config
autoinstall:
  version: 1
  storage:
    layout:
      name: direct
`)

	err := validateUEFIPortableUserData(data)
	if err == nil {
		t.Fatal("validateUEFIPortableUserData returned nil error without fallback command")
	}
	if !strings.Contains(err.Error(), "late-commands") || !strings.Contains(err.Error(), "--removable") {
		t.Fatalf("error %q does not explain missing fallback command", err.Error())
	}
}

func TestValidateUEFIPortableUserDataRejectsBIOSStorage(t *testing.T) {
	data := []byte(`#cloud-config
autoinstall:
  version: 1
  storage:
    config:
      - id: disk0
        type: disk
        ptable: gpt
      - id: part-bios
        type: partition
        device: disk0
        size: 1M
        flag: bios_grub
      - id: part-boot
        type: partition
        device: disk0
        size: 1G
        flag: boot
      - id: fs-boot
        type: format
        volume: part-boot
        fstype: ext4
      - id: mount-boot
        type: mount
        device: fs-boot
        path: /boot
  late-commands:
    - curtin in-target --target=/target -- grub-install --target=x86_64-efi --efi-directory=/boot/efi --bootloader-id=ubuntu --removable --recheck
`)

	err := validateUEFIPortableUserData(data)
	if err == nil {
		t.Fatal("validateUEFIPortableUserData returned nil error for BIOS-style storage")
	}
	if !strings.Contains(err.Error(), "EFI System Partition") || !strings.Contains(err.Error(), "/boot/efi") {
		t.Fatalf("error %q does not explain missing ESP", err.Error())
	}
}

func TestExampleUserDataFiles(t *testing.T) {
	uefiData, err := os.ReadFile("autoinstall.uefi.example.yaml")
	if err != nil {
		t.Fatalf("read UEFI example: %v", err)
	}
	if _, err := validateUserData(uefiData); err != nil {
		t.Fatalf("UEFI example failed user-data validation: %v", err)
	}
	if err := validateUEFIPortableUserData(uefiData); err != nil {
		t.Fatalf("UEFI example failed UEFI portability validation: %v", err)
	}

	biosData, err := os.ReadFile("autoinstall.bios.example.yaml")
	if err != nil {
		t.Fatalf("read BIOS example: %v", err)
	}
	if _, err := validateUserData(biosData); err != nil {
		t.Fatalf("BIOS example failed user-data validation: %v", err)
	}
	if err := validateUEFIPortableUserData(biosData); err == nil {
		t.Fatal("BIOS example unexpectedly passed UEFI portability validation")
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

func TestNormalizeBootModeDefaultsToUEFI(t *testing.T) {
	if got := normalizeBootMode(""); got != bootModeUEFI {
		t.Fatalf("normalizeBootMode empty = %q, want %q", got, bootModeUEFI)
	}
	if got := normalizeBootMode(" BIOS "); got != bootModeBIOS {
		t.Fatalf("normalizeBootMode trims and lowercases to %q, want %q", got, bootModeBIOS)
	}
}

func TestValidateBootMode(t *testing.T) {
	for _, mode := range []string{"", "uefi", "UEFI", "bios", " BIOS "} {
		if !validateBootMode(mode) {
			t.Fatalf("validateBootMode(%q) = false, want true", mode)
		}
	}
	if validateBootMode("legacy") {
		t.Fatal("validateBootMode returned true for unsupported mode")
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
		bootMode:  bootModeBIOS,
	}

	args, err := installer.qemuArgs("seed.iso", "vmlinuz", "initrd")
	if err != nil {
		t.Fatalf("qemuArgs returned error: %v", err)
	}
	if !hasArg(args, "-nographic") {
		t.Fatalf("qemu args %v do not contain -nographic", args)
	}
	if hasArg(args, "if=pflash,format=raw,readonly=on,file=OVMF_CODE.fd") {
		t.Fatalf("bios qemu args %v unexpectedly contain OVMF pflash", args)
	}
	if hasArgPair(args, "-display", "none") {
		t.Fatalf("qemu args %v still contain -display none", args)
	}
	appendValue := valueAfterArg(args, "-append")
	if !strings.Contains(appendValue, "console=ttyS0,115200n8") {
		t.Fatalf("qemu -append value %q does not enable ttyS0 console", appendValue)
	}
	if !hasArgPair(args, "-drive", "file=ubuntu.iso,format=raw,readonly=on,if=virtio") {
		t.Fatalf("qemu args %v do not attach Ubuntu ISO as virtio block", args)
	}
	if !hasArgPair(args, "-drive", "file=seed.iso,format=raw,readonly=on,if=virtio") {
		t.Fatalf("qemu args %v do not attach seed ISO as virtio block", args)
	}
	for _, arg := range args {
		if strings.Contains(arg, "media=cdrom") {
			t.Fatalf("qemu args %v still attach ISOs with media=cdrom", args)
		}
	}
}

func TestQemuArgsUEFIUsesOVMFPFlash(t *testing.T) {
	dir := t.TempDir()
	codePath := filepath.Join(dir, "OVMF_CODE.fd")
	varsTemplatePath := filepath.Join(dir, "OVMF_VARS_TEMPLATE.fd")
	if err := os.WriteFile(codePath, []byte("code"), 0o644); err != nil {
		t.Fatalf("write code file: %v", err)
	}
	if err := os.WriteFile(varsTemplatePath, []byte("vars"), 0o644); err != nil {
		t.Fatalf("write vars template: %v", err)
	}

	installer := &Installer{
		ubuntuISO:            "ubuntu.iso",
		imagePath:            "output.img",
		diskFmt:              "raw",
		bootMode:             bootModeUEFI,
		ovmfCodePath:         codePath,
		ovmfVarsTemplatePath: varsTemplatePath,
		tempDir:              dir,
	}

	args, err := installer.qemuArgs("seed.iso", "vmlinuz", "initrd")
	if err != nil {
		t.Fatalf("qemuArgs returned error: %v", err)
	}

	if !hasArgPair(args, "-drive", "if=pflash,format=raw,readonly=on,file="+codePath) {
		t.Fatalf("uefi qemu args %v do not contain readonly OVMF code pflash", args)
	}
	varsPath := filepath.Join(dir, "OVMF_VARS.fd")
	if !hasArgPair(args, "-drive", "if=pflash,format=raw,file="+varsPath) {
		t.Fatalf("uefi qemu args %v do not contain writable OVMF vars pflash", args)
	}
	copied, err := os.ReadFile(varsPath)
	if err != nil {
		t.Fatalf("read copied vars file: %v", err)
	}
	if string(copied) != "vars" {
		t.Fatalf("copied vars = %q, want vars", copied)
	}
}

func TestFindOVMFFirmwareInUsesFirstCompletePair(t *testing.T) {
	dir := t.TempDir()
	incompleteCode := filepath.Join(dir, "incomplete-code.fd")
	completeCode := filepath.Join(dir, "complete-code.fd")
	completeVars := filepath.Join(dir, "complete-vars.fd")
	if err := os.WriteFile(incompleteCode, []byte("code"), 0o644); err != nil {
		t.Fatalf("write incomplete code: %v", err)
	}
	if err := os.WriteFile(completeCode, []byte("code"), 0o644); err != nil {
		t.Fatalf("write complete code: %v", err)
	}
	if err := os.WriteFile(completeVars, []byte("vars"), 0o644); err != nil {
		t.Fatalf("write complete vars: %v", err)
	}

	got, err := findOVMFFirmwareIn([]ovmfFirmware{
		{CodePath: incompleteCode, VarsPath: filepath.Join(dir, "missing-vars.fd")},
		{CodePath: completeCode, VarsPath: completeVars},
	})
	if err != nil {
		t.Fatalf("findOVMFFirmwareIn returned error: %v", err)
	}
	if got.CodePath != completeCode || got.VarsPath != completeVars {
		t.Fatalf("findOVMFFirmwareIn = %+v, want complete pair", got)
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

func TestOVMFInstallSuggestionDebianFamily(t *testing.T) {
	osInfo := OSInfo{
		GOOS:   "linux",
		ID:     "debian",
		IDLike: []string{"debian"},
	}

	got := ovmfInstallSuggestion(osInfo)
	if !strings.Contains(got, "apt install") || !strings.Contains(got, "ovmf") || !strings.Contains(got, "--boot-mode bios") {
		t.Fatalf("ovmfInstallSuggestion returned %q, want apt ovmf package and bios fallback", got)
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
	if !strings.Contains(output, "--boot-mode string") {
		t.Fatalf("usage does not mention boot mode flag:\n%s", output)
	}
}
