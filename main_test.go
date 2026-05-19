package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ubuntu-vm-template-builder/internal/backend/qemu"
	"ubuntu-vm-template-builder/internal/common"
)

func TestRunRequiresBackendSubcommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"ubuntu-vm-template-builder", "--backend", "qemu"}, &stdout, &stderr)
	if code == 0 {
		t.Fatal("run returned success for old --backend syntax")
	}
	if !strings.Contains(stderr.String(), `unknown command "--backend"`) {
		t.Fatalf("stderr does not explain old syntax is unsupported:\n%s", stderr.String())
	}
}

func TestSubcommandHelpExitsSuccessfully(t *testing.T) {
	for _, command := range []string{"qemu", "vcenter"} {
		t.Run(command, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run([]string{"ubuntu-vm-template-builder", command, "--help"}, &stdout, &stderr)
			if code != 0 {
				t.Fatalf("%s --help exit code = %d, stderr:\n%s", command, code, stderr.String())
			}
			output := stdout.String()
			for _, want := range []string{commandBuild, commandHardwareConfigExample, commandPrerequisites} {
				if !strings.Contains(output, want) {
					t.Fatalf("%s --help output missing %q:\n%s", command, want, output)
				}
			}
			if strings.Contains(output, "--hardware-config") {
				t.Fatalf("%s backend help should not show build flags:\n%s", command, output)
			}
		})
	}
}

func TestVCenterHelpMentionsUploadCommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"ubuntu-vm-template-builder", "vcenter", "--help"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("vcenter --help exit code = %d, stderr:\n%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), commandUpload) {
		t.Fatalf("vcenter help missing upload command:\n%s", stdout.String())
	}
}

func TestBuildCommandHelpExitsSuccessfully(t *testing.T) {
	for _, command := range []string{"qemu", "vcenter"} {
		t.Run(command, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run([]string{"ubuntu-vm-template-builder", command, commandBuild, "--help"}, &stdout, &stderr)
			if code != 0 {
				t.Fatalf("%s %s --help exit code = %d, stderr:\n%s", command, commandBuild, code, stderr.String())
			}
			if !strings.Contains(stderr.String(), "--hardware-config") {
				t.Fatalf("%s %s --help output missing --hardware-config:\n%s", command, commandBuild, stderr.String())
			}
			if !strings.Contains(stderr.String(), "Usage: ubuntu-vm-template-builder "+command+" "+commandBuild) {
				t.Fatalf("%s %s help output has unexpected usage:\n%s", command, commandBuild, stderr.String())
			}
		})
	}
}

func TestVCenterUploadHelpExitsSuccessfully(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"ubuntu-vm-template-builder", "vcenter", commandUpload, "--help"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("vcenter upload --help exit code = %d, stderr:\n%s", code, stderr.String())
	}
	output := stderr.String()
	for _, want := range []string{"Usage: ubuntu-vm-template-builder vcenter upload", "--source", "--destination", "--overwrite", "--vcenter-datastore"} {
		if !strings.Contains(output, want) {
			t.Fatalf("vcenter upload help missing %q:\n%s", want, output)
		}
	}
}

func TestHardwareConfigExampleCommands(t *testing.T) {
	tests := []struct {
		command string
		want    []string
		absent  []string
	}{
		{
			command: "qemu",
			want: []string{
				"boot_firmware: uefi",
				"disk_size: 20G",
				"vcpu: 2",
				"memory_mb: 2048",
				"qemu:",
				"cpu_model: host",
				"disk_interface: virtio",
				"iso_interface: virtio",
			},
			absent: []string{"vcenter:"},
		},
		{
			command: "vcenter",
			want: []string{
				"boot_firmware: uefi",
				"disk_size: 20G",
				"vcpu: 2",
				"memory_mb: 2048",
				"vcenter:",
				"scsi_controller: pvscsi",
				"network_adapter: vmxnet3",
				"network: VM Network",
				"disk_provisioning: thick_provision_lazy_zeroed",
				"compatibility: \"\"",
				"guest_os_id: ubuntu64Guest",
				"reserve_all_guest_memory: false",
				"output_type: template",
			},
			absent: []string{"qemu:"},
		},
	}

	for _, test := range tests {
		t.Run(test.command, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run([]string{"ubuntu-vm-template-builder", test.command, commandHardwareConfigExample}, &stdout, &stderr)
			if code != 0 {
				t.Fatalf("%s %s exit code = %d, stderr:\n%s", test.command, commandHardwareConfigExample, code, stderr.String())
			}
			if stderr.Len() != 0 {
				t.Fatalf("%s %s wrote stderr:\n%s", test.command, commandHardwareConfigExample, stderr.String())
			}

			output := stdout.String()
			for _, want := range test.want {
				if !strings.Contains(output, want) {
					t.Fatalf("%s example missing %q in:\n%s", test.command, want, output)
				}
			}
			for _, absent := range test.absent {
				if strings.Contains(output, absent) {
					t.Fatalf("%s example unexpectedly contains %q in:\n%s", test.command, absent, output)
				}
			}

			cfg := loadHardwareConfigExampleOutput(t, output)
			if cfg.BootFirmware != common.BootFirmwareUEFI || cfg.DiskSize != "20G" || cfg.VCPU != 2 || cfg.MemoryMB != 2048 {
				t.Fatalf("loaded example hardware config = %+v", cfg)
			}
		})
	}
}

func TestHardwareConfigExampleHelpExitsSuccessfully(t *testing.T) {
	for _, command := range []string{"qemu", "vcenter"} {
		t.Run(command, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run([]string{"ubuntu-vm-template-builder", command, commandHardwareConfigExample, "--help"}, &stdout, &stderr)
			if code != 0 {
				t.Fatalf("%s %s --help exit code = %d, stderr:\n%s", command, commandHardwareConfigExample, code, stderr.String())
			}
			if !strings.Contains(stdout.String(), "Usage: ubuntu-vm-template-builder "+command+" "+commandHardwareConfigExample) {
				t.Fatalf("nested help output is unexpected:\n%s", stdout.String())
			}
		})
	}
}

func TestPrerequisiteCommandsAreNestedUnderBackends(t *testing.T) {
	for _, command := range []string{"qemu", "vcenter"} {
		t.Run(command, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run([]string{"ubuntu-vm-template-builder", command, commandPrerequisites, "--help"}, &stdout, &stderr)
			if code != 0 {
				t.Fatalf("%s %s --help exit code = %d, stderr:\n%s", command, commandPrerequisites, code, stderr.String())
			}
			if !strings.Contains(stdout.String(), "Usage: ubuntu-vm-template-builder "+command+" "+commandPrerequisites) {
				t.Fatalf("nested prerequisites help output is unexpected:\n%s", stdout.String())
			}
		})
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"ubuntu-vm-template-builder", commandPrerequisites}, &stdout, &stderr)
	if code == 0 {
		t.Fatal("top-level prerequisites command returned success")
	}
	if !strings.Contains(stderr.String(), `unknown command "prerequisites"`) {
		t.Fatalf("top-level prerequisites error is unexpected:\n%s", stderr.String())
	}
}

func TestPrerequisiteCollectorsAreBackendSpecific(t *testing.T) {
	qemuReport := collectQEMUPrerequisites()
	if !prerequisiteNamesContain(qemuReport, "qemu-system-x86_64") {
		t.Fatalf("QEMU prerequisites missing qemu-system-x86_64: %+v", qemuReport.Items)
	}
	if prerequisiteNamesContain(qemuReport, "xorriso") {
		t.Fatalf("QEMU prerequisites unexpectedly include xorriso: %+v", qemuReport.Items)
	}
	for _, input := range qemuReport.InputPrerequisites {
		if strings.Contains(strings.ToLower(input), "vcenter") {
			t.Fatalf("QEMU input prerequisite mentions vCenter: %q", input)
		}
	}

	vcenterReport := collectVCenterPrerequisites()
	if !prerequisiteNamesContain(vcenterReport, "xorriso") {
		t.Fatalf("vCenter prerequisites missing xorriso: %+v", vcenterReport.Items)
	}
	for _, disallowed := range []string{"qemu-system-x86_64", "qemu-img", "OVMF UEFI firmware", "/dev/kvm access"} {
		if prerequisiteNamesContain(vcenterReport, disallowed) {
			t.Fatalf("vCenter prerequisites unexpectedly include %s: %+v", disallowed, vcenterReport.Items)
		}
	}
}

func prerequisiteNamesContain(report prerequisiteReport, name string) bool {
	for _, item := range report.Items {
		if item.Name == name {
			return true
		}
	}
	return false
}

func TestHardwareConfigExampleRejectsExtraArgs(t *testing.T) {
	for _, command := range []string{"qemu", "vcenter"} {
		t.Run(command, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run([]string{"ubuntu-vm-template-builder", command, commandHardwareConfigExample, "extra"}, &stdout, &stderr)
			if code == 0 {
				t.Fatalf("%s %s returned success with extra argument", command, commandHardwareConfigExample)
			}
			if !strings.Contains(stderr.String(), "does not accept arguments") {
				t.Fatalf("extra arg error is unexpected:\n%s", stderr.String())
			}
		})
	}
}

func TestRunVCenterUploadRequiresUploadAndPlacementFlags(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"ubuntu-vm-template-builder", "vcenter", commandUpload}, &stdout, &stderr)
	if code == 0 {
		t.Fatal("vcenter upload returned success without required flags")
	}
	errOutput := stderr.String()
	for _, want := range []string{"--destination", "--source", "--vcenter-datacenter", "--vcenter-datastore", "--vcenter-esxi-host", "--vcenter-host", "--vcenter-password", "--vcenter-username"} {
		if !strings.Contains(errOutput, want) {
			t.Fatalf("vcenter upload missing flag error does not mention %s:\n%s", want, errOutput)
		}
	}
	for _, absent := range []string{"--iso", "--user-data", "--hardware-config", "--vcenter-folder", "--vcenter-network"} {
		if strings.Contains(strings.SplitN(errOutput, "\n", 2)[0], absent) {
			t.Fatalf("vcenter upload missing flag error should not mention build-only flag %s:\n%s", absent, errOutput)
		}
	}
}

func TestRunVCenterUploadDoesNotRequireBuildOnlyFlagsAndAcceptsOverwrite(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{
		"ubuntu-vm-template-builder",
		"vcenter",
		commandUpload,
		"--source", filepath.Join(t.TempDir(), "missing.txt"),
		"--destination", "uploads/missing.txt",
		"--overwrite",
		"--vcenter-host", "vc.example.com",
		"--vcenter-username", "administrator@vsphere.local",
		"--vcenter-password", "secret",
		"--vcenter-datacenter", "DC0",
		"--vcenter-esxi-host", "esxi.example.com",
		"--vcenter-datastore", "datastore1",
	}, &stdout, &stderr)
	if code == 0 {
		t.Fatal("vcenter upload unexpectedly reached success with missing local source")
	}
	errOutput := stderr.String()
	if !strings.Contains(errOutput, "source file") {
		t.Fatalf("vcenter upload should validate source before connecting:\n%s", errOutput)
	}
	for _, absent := range []string{"--iso", "--user-data", "--hardware-config", "--vcenter-folder", "--vcenter-network", "flag provided but not defined"} {
		if strings.Contains(errOutput, absent) {
			t.Fatalf("vcenter upload error unexpectedly contains %q:\n%s", absent, errOutput)
		}
	}
}

func TestRunQEMURequiresImage(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{
		"ubuntu-vm-template-builder",
		"qemu",
		"build",
		"--iso", "ubuntu.iso",
		"--user-data", "autoinstall.yaml",
	}, &stdout, &stderr)
	if code == 0 {
		t.Fatal("qemu command returned success without --image")
	}
	if !strings.Contains(stderr.String(), "--image") {
		t.Fatalf("qemu missing flag error does not mention --image:\n%s", stderr.String())
	}
}

func TestRunQEMURequiresDiskSizeFromHardwareConfig(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{
		"ubuntu-vm-template-builder",
		"qemu",
		"build",
		"--iso", "ubuntu.iso",
		"--image", "output.img",
		"--user-data", "autoinstall.yaml",
	}, &stdout, &stderr)
	if code == 0 {
		t.Fatal("qemu command returned success without disk_size")
	}
	if !strings.Contains(stderr.String(), "disk_size") {
		t.Fatalf("qemu missing disk_size error does not mention disk_size:\n%s", stderr.String())
	}
}

func TestRunQEMURejectsDiskSizeFlag(t *testing.T) {
	for _, command := range []string{"qemu", "vcenter"} {
		t.Run(command, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run([]string{
				"ubuntu-vm-template-builder",
				command,
				"build",
				"--disk-size", "20G",
			}, &stdout, &stderr)
			if code == 0 {
				t.Fatalf("%s command returned success with removed --disk-size flag", command)
			}
			if !strings.Contains(stderr.String(), "flag provided but not defined") {
				t.Fatalf("%s --disk-size error is unexpected:\n%s", command, stderr.String())
			}
		})
	}
}

func TestBackendDirectBuildSyntaxIsRejected(t *testing.T) {
	tests := []struct {
		command string
		flag    string
	}{
		{command: "qemu", flag: "--iso"},
		{command: "vcenter", flag: "--iso"},
	}
	for _, test := range tests {
		t.Run(test.command, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run([]string{"ubuntu-vm-template-builder", test.command, test.flag, "ubuntu.iso"}, &stdout, &stderr)
			if code == 0 {
				t.Fatalf("%s direct build syntax returned success", test.command)
			}
			if !strings.Contains(stderr.String(), "unknown "+test.command+" command") {
				t.Fatalf("%s direct build error is unexpected:\n%s", test.command, stderr.String())
			}
		})
	}
}

func TestRunVCenterDoesNotRequireImage(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{
		"ubuntu-vm-template-builder",
		"vcenter",
		"build",
		"--iso", "ubuntu.iso",
		"--user-data", "autoinstall.yaml",
	}, &stdout, &stderr)
	if code == 0 {
		t.Fatal("vcenter command returned success without placement flags")
	}
	errOutput := stderr.String()
	for _, want := range []string{"--vcenter-datacenter", "--vcenter-datastore", "--vcenter-esxi-host", "--vcenter-folder", "--vcenter-host"} {
		if !strings.Contains(errOutput, want) {
			t.Fatalf("vcenter missing flag error does not mention %s:\n%s", want, errOutput)
		}
	}
	firstLine := strings.SplitN(errOutput, "\n", 2)[0]
	if strings.Contains(firstLine, "--vcenter-network") {
		t.Fatalf("vcenter missing flag error should allow network from hardware config:\n%s", errOutput)
	}
	if strings.Contains(errOutput, "--image") {
		t.Fatalf("vcenter missing flag error should not require --image:\n%s", errOutput)
	}
}

func TestRunVCenterRequiresNetworkFromFlagOrHardwareConfig(t *testing.T) {
	hardwareConfigPath := writeTempHardwareConfig(t, "disk_size: 20G\n")

	var stdout, stderr bytes.Buffer
	code := run([]string{
		"ubuntu-vm-template-builder",
		"vcenter",
		"build",
		"--iso", "ubuntu.iso",
		"--user-data", "autoinstall.yaml",
		"--hardware-config", hardwareConfigPath,
		"--vcenter-host", "vc.example.com",
		"--vcenter-username", "administrator@vsphere.local",
		"--vcenter-password", "secret",
		"--vcenter-datacenter", "DC0",
		"--vcenter-esxi-host", "esxi.example.com",
		"--vcenter-datastore", "datastore1",
		"--vcenter-folder", "vm",
	}, &stdout, &stderr)
	if code == 0 {
		t.Fatal("vcenter command returned success without network")
	}
	if !strings.Contains(stderr.String(), "pass --vcenter-network or set vcenter.network") {
		t.Fatalf("network error is unexpected:\n%s", stderr.String())
	}
}

func TestRunVCenterAcceptsNetworkFromHardwareConfig(t *testing.T) {
	hardwareConfigPath := writeTempHardwareConfig(t, "disk_size: 20G\nvcenter:\n  network: VM Network\n")

	var stdout, stderr bytes.Buffer
	code := run([]string{
		"ubuntu-vm-template-builder",
		"vcenter",
		"build",
		"--iso", "ubuntu.iso",
		"--user-data", "autoinstall.yaml",
		"--hardware-config", hardwareConfigPath,
		"--vcenter-host", "vc.example.com",
		"--vcenter-username", "administrator@vsphere.local",
		"--vcenter-password", "secret",
		"--vcenter-datacenter", "DC0",
		"--vcenter-esxi-host", "esxi.example.com",
		"--vcenter-datastore", "datastore1",
		"--vcenter-folder", "vm",
	}, &stdout, &stderr)
	if code == 0 {
		t.Fatal("vcenter command unexpectedly reached success with placeholder files")
	}
	if strings.Contains(stderr.String(), "pass --vcenter-network or set vcenter.network") {
		t.Fatalf("vcenter did not accept hardware config network:\n%s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "read user-data file") {
		t.Fatalf("vcenter should proceed past network validation to user-data loading:\n%s", stderr.String())
	}
}

func TestEffectiveVCenterNetworkPrefersCLI(t *testing.T) {
	hardware := common.DefaultHardwareConfig()
	hardware.DiskSize = "20G"
	hardware.VCenter.Network = "YAML Network"

	if got := effectiveVCenterNetwork(" CLI Network ", hardware); got != "CLI Network" {
		t.Fatalf("effectiveVCenterNetwork with CLI = %q, want CLI Network", got)
	}
	if got := effectiveVCenterNetwork("", hardware); got != "YAML Network" {
		t.Fatalf("effectiveVCenterNetwork without CLI = %q, want YAML Network", got)
	}
}

func TestRequiredFlagErrorsSorted(t *testing.T) {
	got := requiredFlagErrors(map[string]string{
		"zeta":  "",
		"alpha": "",
		"ok":    "value",
	})
	want := []string{"--alpha", "--zeta"}
	if len(got) != len(want) {
		t.Fatalf("requiredFlagErrors = %v, want %v", got, want)
	}
	for idx := range want {
		if got[idx] != want[idx] {
			t.Fatalf("requiredFlagErrors = %v, want %v", got, want)
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
		Backend: "QEMU",
		OS:      qemu.OSInfo{GOOS: "linux", ID: "debian", Name: "Debian"},
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
		InputPrerequisites: []string{"hardware config YAML with disk_size"},
	}

	var out bytes.Buffer
	printPrerequisiteReport(&out, report)
	output := out.String()

	for _, want := range []string{
		"QEMU prerequisites",
		"[MISSING] qemu-system-x86_64 (required)",
		"Fix: sudo apt install qemu-system-x86",
		"QEMU install input prerequisites checked during normal install runs:",
		"hardware config YAML with disk_size",
		"One or more required QEMU host prerequisites are missing.",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("prerequisite report missing %q in:\n%s", want, output)
		}
	}
}

func TestPrintUsageMentionsBackendCommands(t *testing.T) {
	var out bytes.Buffer
	printUsage(&out, "ubuntu-vm-template-builder")
	output := out.String()

	for _, want := range []string{
		"Usage: ubuntu-vm-template-builder <command>",
		"qemu",
		"vcenter",
		"Run ubuntu-vm-template-builder <command> --help",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("usage missing %q in:\n%s", want, output)
		}
	}
	if strings.Contains(output, "--backend") {
		t.Fatalf("usage still mentions removed --backend flag:\n%s", output)
	}
	if strings.Contains(output, "prerequisites") {
		t.Fatalf("top-level usage should not mention prerequisites:\n%s", output)
	}
}

func TestCommandUsageMentionsHardwareConfig(t *testing.T) {
	var out bytes.Buffer
	printQEMUUsage(&out, "ubuntu-vm-template-builder")
	if output := out.String(); !strings.Contains(output, commandBuild) || strings.Contains(output, "--hardware-config") || strings.Contains(output, "--boot-mode") {
		t.Fatalf("qemu backend usage should mention build command and not build flags:\n%s", output)
	}

	out.Reset()
	printQEMUBuildUsage(&out, "ubuntu-vm-template-builder")
	if output := out.String(); !strings.Contains(output, "--hardware-config") || !strings.Contains(output, "qemu build") || strings.Contains(output, "--boot-mode") || strings.Contains(output, "--disk-size") {
		t.Fatalf("qemu build usage should mention --hardware-config and not removed flags:\n%s", output)
	}

	out.Reset()
	printVCenterUsage(&out, "ubuntu-vm-template-builder")
	if output := out.String(); !strings.Contains(output, commandBuild) || strings.Contains(output, "--hardware-config") || strings.Contains(output, "--vm-cpu") || strings.Contains(output, "--vm-memory-mb") {
		t.Fatalf("vcenter backend usage should mention build command and not build flags:\n%s", output)
	}

	out.Reset()
	printVCenterBuildUsage(&out, "ubuntu-vm-template-builder")
	if output := out.String(); !strings.Contains(output, "--hardware-config") || !strings.Contains(output, "vcenter build") || strings.Contains(output, "--disk-size") || strings.Contains(output, "--vm-cpu") || strings.Contains(output, "--vm-memory-mb") {
		t.Fatalf("vcenter build usage should mention --hardware-config and not removed hardware flags:\n%s", output)
	}
}

func loadHardwareConfigExampleOutput(t *testing.T, output string) common.HardwareConfig {
	t.Helper()

	path := writeTempHardwareConfig(t, output)
	cfg, err := common.LoadHardwareConfig(path)
	if err != nil {
		t.Fatalf("LoadHardwareConfig rejected example output: %v\n%s", err, output)
	}
	return cfg
}

func writeTempHardwareConfig(t *testing.T, data string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "hardware.yaml")
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatalf("write hardware config: %v", err)
	}
	return path
}
