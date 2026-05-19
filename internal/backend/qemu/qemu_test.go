package qemu

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"ubuntu-vm-template-builder/internal/common"
)

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
			t.Fatal("waitInterruptibleCommand returned nil after interrupt")
		}
		if !strings.Contains(err.Error(), "interrupt") {
			t.Fatalf("interrupt error = %q, want mention of interrupt", err.Error())
		}
	case <-time.After(3 * time.Second):
		_ = signalCommandProcessGroup(cmd, syscall.SIGKILL)
		t.Fatal("command did not stop after interrupt")
	}
}

func TestQEMUArgsUseNographicSerialConsole(t *testing.T) {
	hardware := common.DefaultHardwareConfig()
	hardware.BootFirmware = common.BootFirmwareBIOS
	installer := &Installer{
		ubuntuISO: "ubuntu.iso",
		imagePath: "output.img",
		diskFmt:   "raw",
		hardware:  hardware,
	}

	args, err := installer.QEMUArgs("seed.iso", "vmlinuz", "initrd")
	if err != nil {
		t.Fatalf("QEMUArgs returned error: %v", err)
	}
	if !hasArg(args, "-nographic") {
		t.Fatalf("qemu args %v do not contain -nographic", args)
	}
	if !hasArgPair(args, "-cpu", "host") {
		t.Fatalf("qemu args %v do not use host CPU model", args)
	}
	if !hasArgPair(args, "-smp", "2") {
		t.Fatalf("qemu args %v do not use default vCPU count", args)
	}
	if !hasArgPair(args, "-m", "2048") {
		t.Fatalf("qemu args %v do not use default memory", args)
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

func TestQEMUArgsUseHardwareConfig(t *testing.T) {
	hardware := common.DefaultHardwareConfig()
	hardware.BootFirmware = common.BootFirmwareBIOS
	hardware.VCPU = 6
	hardware.MemoryMB = 12288
	hardware.QEMU.CPUModel = "max"
	hardware.QEMU.DiskInterface = "scsi"
	hardware.QEMU.ISOInterface = "sata"
	installer := &Installer{
		ubuntuISO: "ubuntu.iso",
		imagePath: "output.qcow2",
		diskFmt:   "qcow2",
		hardware:  hardware,
	}

	args, err := installer.QEMUArgs("seed.iso", "vmlinuz", "initrd")
	if err != nil {
		t.Fatalf("QEMUArgs returned error: %v", err)
	}

	for _, pair := range [][2]string{
		{"-cpu", "max"},
		{"-smp", "6"},
		{"-m", "12288"},
		{"-drive", "file=output.qcow2,format=qcow2,cache=none,if=scsi"},
		{"-drive", "file=ubuntu.iso,format=raw,readonly=on,if=sata"},
		{"-drive", "file=seed.iso,format=raw,readonly=on,if=sata"},
	} {
		if !hasArgPair(args, pair[0], pair[1]) {
			t.Fatalf("qemu args %v missing %s %s", args, pair[0], pair[1])
		}
	}
}

func TestQEMUArgsUEFIUsesOVMFPFlash(t *testing.T) {
	dir := t.TempDir()
	codePath := filepath.Join(dir, "OVMF_CODE.fd")
	varsTemplatePath := filepath.Join(dir, "OVMF_VARS_TEMPLATE.fd")
	if err := os.WriteFile(codePath, []byte("code"), 0o644); err != nil {
		t.Fatalf("write code file: %v", err)
	}
	if err := os.WriteFile(varsTemplatePath, []byte("vars"), 0o644); err != nil {
		t.Fatalf("write vars template: %v", err)
	}

	hardware := common.DefaultHardwareConfig()
	hardware.BootFirmware = common.BootFirmwareUEFI
	installer := &Installer{
		ubuntuISO:            "ubuntu.iso",
		imagePath:            "output.img",
		diskFmt:              "raw",
		hardware:             hardware,
		ovmfCodePath:         codePath,
		ovmfVarsTemplatePath: varsTemplatePath,
		tempDir:              dir,
	}

	args, err := installer.QEMUArgs("seed.iso", "vmlinuz", "initrd")
	if err != nil {
		t.Fatalf("QEMUArgs returned error: %v", err)
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

	got, err := FindOVMFFirmwareIn([]OVMFFirmware{
		{CodePath: incompleteCode, VarsPath: filepath.Join(dir, "missing-vars.fd")},
		{CodePath: completeCode, VarsPath: completeVars},
	})
	if err != nil {
		t.Fatalf("FindOVMFFirmwareIn returned error: %v", err)
	}
	if got.CodePath != completeCode || got.VarsPath != completeVars {
		t.Fatalf("FindOVMFFirmwareIn = %+v, want complete pair", got)
	}
}

func TestParseOSRelease(t *testing.T) {
	values := ParseOSRelease([]byte(`NAME="Ubuntu"
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

func TestQEMUInstallSuggestionDebianFamily(t *testing.T) {
	osInfo := OSInfo{
		GOOS:   "linux",
		ID:     "ubuntu",
		IDLike: []string{"debian"},
	}

	got := InstallSuggestion(osInfo)
	if !strings.Contains(got, "apt install") || !strings.Contains(got, "qemu-system-x86") || !strings.Contains(got, "qemu-utils") {
		t.Fatalf("InstallSuggestion returned %q, want apt qemu packages", got)
	}
}

func TestOVMFInstallSuggestionDebianFamily(t *testing.T) {
	osInfo := OSInfo{
		GOOS:   "linux",
		ID:     "debian",
		IDLike: []string{"debian"},
	}

	got := OVMFInstallSuggestion(osInfo)
	if !strings.Contains(got, "apt install") || !strings.Contains(got, "ovmf") || !strings.Contains(got, "boot_firmware: bios") {
		t.Fatalf("OVMFInstallSuggestion returned %q, want apt ovmf package and hardware config bios fallback", got)
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
		if got := GoVersionAtLeast(test.output, 1, 26); got != test.want {
			t.Fatalf("GoVersionAtLeast(%q) = %v, want %v", test.output, got, test.want)
		}
	}
}

func TestValidateDiskFormat(t *testing.T) {
	for _, format := range []string{"raw", "qcow2", "vmdk", " RAW "} {
		if !ValidateDiskFormat(format) {
			t.Fatalf("ValidateDiskFormat(%q) = false, want true", format)
		}
	}
	if ValidateDiskFormat("vdi") {
		t.Fatal("ValidateDiskFormat returned true for vdi")
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
