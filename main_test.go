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
	for _, command := range []string{"qemu", "vcenter", "proxmox"} {
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

func TestOnlyVCenterHelpMentionsUploadCommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"ubuntu-vm-template-builder", "vcenter", "--help"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("vcenter --help exit code = %d, stderr:\n%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), commandUpload) {
		t.Fatalf("vcenter help missing upload command:\n%s", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = run([]string{"ubuntu-vm-template-builder", "proxmox", "--help"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("proxmox --help exit code = %d, stderr:\n%s", code, stderr.String())
	}
	if strings.Contains(stdout.String(), commandUpload) {
		t.Fatalf("proxmox help should not include upload command:\n%s", stdout.String())
	}
}

func TestBuildCommandHelpExitsSuccessfully(t *testing.T) {
	for _, command := range []string{"qemu", "vcenter", "proxmox"} {
		t.Run(command, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run([]string{"ubuntu-vm-template-builder", command, commandBuild, "--help"}, &stdout, &stderr)
			if code != 0 {
				t.Fatalf("%s %s --help exit code = %d, stderr:\n%s", command, commandBuild, code, stderr.String())
			}
			if !strings.Contains(stderr.String(), "--hardware-config") {
				t.Fatalf("%s %s --help output missing --hardware-config:\n%s", command, commandBuild, stderr.String())
			}
			if !strings.Contains(stderr.String(), "--install-extra-packages") {
				t.Fatalf("%s %s --help output missing --install-extra-packages:\n%s", command, commandBuild, stderr.String())
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
		{
			command: "proxmox",
			want: []string{
				"boot_firmware: uefi",
				"disk_size: 20G",
				"vcpu: 2",
				"memory_mb: 2048",
				"proxmox:",
				"bridge: vmbr0",
				"network_adapter: virtio",
				"scsi_controller: virtio-scsi-pci",
				"disk_interface: scsi",
				"disk_format: raw",
				"cpu_type: host",
				"machine: q35",
				"ostype: l26",
				"efi_type: 4m",
				"pre_enrolled_keys: false",
				"output_type: template",
			},
			absent: []string{"qemu:", "vcenter:"},
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
	for _, command := range []string{"qemu", "vcenter", "proxmox"} {
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
	for _, command := range []string{"qemu", "vcenter", "proxmox"} {
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

	proxmoxReport := collectProxmoxPrerequisites()
	if !prerequisiteNamesContain(proxmoxReport, "xorriso") {
		t.Fatalf("Proxmox prerequisites missing xorriso: %+v", proxmoxReport.Items)
	}
	for _, input := range proxmoxReport.InputPrerequisites {
		if strings.Contains(strings.ToLower(input), "vcenter") {
			t.Fatalf("Proxmox input prerequisite mentions vCenter: %q", input)
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
	for _, command := range []string{"qemu", "vcenter", "proxmox"} {
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

func TestRunProxmoxUploadIsUnknownCommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"ubuntu-vm-template-builder", "proxmox", commandUpload}, &stdout, &stderr)
	if code == 0 {
		t.Fatal("proxmox upload returned success")
	}
	errOutput := stderr.String()
	if !strings.Contains(errOutput, `unknown proxmox command "upload"`) {
		t.Fatalf("proxmox upload error is unexpected:\n%s", errOutput)
	}
	if strings.Contains(errOutput, "--source") || strings.Contains(errOutput, "--destination") || strings.Contains(errOutput, "--proxmox-storage") {
		t.Fatalf("proxmox upload should not expose removed upload flags:\n%s", errOutput)
	}
}

func TestRunProxmoxBuildRejectsOldStorageFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{
		"ubuntu-vm-template-builder",
		"proxmox",
		commandBuild,
		"--iso", "ubuntu.iso",
		"--user-data", "autoinstall.yaml",
		"--proxmox-host", "pve.example.com",
		"--proxmox-token-id", "root@pam!builder",
		"--proxmox-token-secret", "secret",
		"--proxmox-node", "pve",
		"--proxmox-storage", "local",
	}, &stdout, &stderr)
	if code == 0 {
		t.Fatal("proxmox build accepted removed --proxmox-storage flag")
	}
	if !strings.Contains(stderr.String(), "flag provided but not defined: -proxmox-storage") {
		t.Fatalf("old storage flag error is unexpected:\n%s", stderr.String())
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
	for _, command := range []string{"qemu", "vcenter", "proxmox"} {
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
		{command: "proxmox", flag: "--iso"},
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

func TestRunProxmoxDoesNotRequireImage(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{
		"ubuntu-vm-template-builder",
		"proxmox",
		"build",
		"--iso", "ubuntu.iso",
		"--user-data", "autoinstall.yaml",
	}, &stdout, &stderr)
	if code == 0 {
		t.Fatal("proxmox command returned success without placement flags")
	}
	errOutput := stderr.String()
	for _, want := range []string{"--proxmox-disk-storage", "--proxmox-host", "--proxmox-iso-storage", "--proxmox-node", "--proxmox-token-id", "--proxmox-token-secret"} {
		if !strings.Contains(errOutput, want) {
			t.Fatalf("proxmox missing flag error does not mention %s:\n%s", want, errOutput)
		}
	}
	firstLine := strings.SplitN(errOutput, "\n", 2)[0]
	if strings.Contains(firstLine, "--proxmox-bridge") {
		t.Fatalf("proxmox missing flag error should allow bridge from hardware config:\n%s", errOutput)
	}
	if strings.Contains(errOutput, "--image") {
		t.Fatalf("proxmox missing flag error should not require --image:\n%s", errOutput)
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

func TestRunProxmoxRequiresBridgeFromFlagOrHardwareConfig(t *testing.T) {
	hardwareConfigPath := writeTempHardwareConfig(t, "disk_size: 20G\n")

	var stdout, stderr bytes.Buffer
	code := run([]string{
		"ubuntu-vm-template-builder",
		"proxmox",
		"build",
		"--iso", "ubuntu.iso",
		"--user-data", "autoinstall.yaml",
		"--hardware-config", hardwareConfigPath,
		"--proxmox-host", "pve.example.com",
		"--proxmox-token-id", "root@pam!builder",
		"--proxmox-token-secret", "secret",
		"--proxmox-node", "pve",
		"--proxmox-iso-storage", "local",
		"--proxmox-disk-storage", "vms",
	}, &stdout, &stderr)
	if code == 0 {
		t.Fatal("proxmox command returned success without bridge")
	}
	if !strings.Contains(stderr.String(), "pass --proxmox-bridge or set proxmox.bridge") {
		t.Fatalf("bridge error is unexpected:\n%s", stderr.String())
	}
}

func TestRunProxmoxAcceptsBridgeFromHardwareConfig(t *testing.T) {
	hardwareConfigPath := writeTempHardwareConfig(t, "disk_size: 20G\nproxmox:\n  bridge: vmbr0\n")

	var stdout, stderr bytes.Buffer
	code := run([]string{
		"ubuntu-vm-template-builder",
		"proxmox",
		"build",
		"--iso", "ubuntu.iso",
		"--user-data", "autoinstall.yaml",
		"--hardware-config", hardwareConfigPath,
		"--proxmox-host", "pve.example.com",
		"--proxmox-token-id", "root@pam!builder",
		"--proxmox-token-secret", "secret",
		"--proxmox-node", "pve",
		"--proxmox-iso-storage", "local",
		"--proxmox-disk-storage", "vms",
	}, &stdout, &stderr)
	if code == 0 {
		t.Fatal("proxmox command unexpectedly reached success with placeholder files")
	}
	if strings.Contains(stderr.String(), "pass --proxmox-bridge or set proxmox.bridge") {
		t.Fatalf("proxmox did not accept hardware config bridge:\n%s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "read user-data file") {
		t.Fatalf("proxmox should proceed past bridge validation to user-data loading:\n%s", stderr.String())
	}
}

func TestRunProxmoxBuildRejectsInvalidOptionsBeforeISOCheck(t *testing.T) {
	dir := t.TempDir()
	userDataPath := filepath.Join(dir, "user-data.yaml")
	if err := os.WriteFile(userDataPath, []byte(`#cloud-config
autoinstall:
  version: 1
`), 0o644); err != nil {
		t.Fatalf("write user-data: %v", err)
	}
	hardwareConfigPath := writeTempHardwareConfig(t, "disk_size: 20G\nproxmox:\n  bridge: vmbr0\n")
	optionsPath := filepath.Join(dir, "options.yaml")
	if err := os.WriteFile(optionsPath, []byte("boot: order=scsi0\n"), 0o644); err != nil {
		t.Fatalf("write options: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{
		"ubuntu-vm-template-builder",
		"proxmox",
		"build",
		"--iso", "missing.iso",
		"--user-data", userDataPath,
		"--hardware-config", hardwareConfigPath,
		"--proxmox-host", "pve.example.com",
		"--proxmox-token-id", "root@pam!builder",
		"--proxmox-token-secret", "secret",
		"--proxmox-node", "pve",
		"--proxmox-iso-storage", "local",
		"--proxmox-disk-storage", "vms",
		"--options", optionsPath,
	}, &stdout, &stderr)
	if code == 0 {
		t.Fatal("proxmox build accepted invalid options config")
	}
	errOutput := stderr.String()
	if !strings.Contains(errOutput, "Proxmox options") || !strings.Contains(errOutput, "field boot not found") {
		t.Fatalf("invalid options error is unexpected:\n%s", errOutput)
	}
	if strings.Contains(errOutput, "ubuntu ISO file") {
		t.Fatalf("proxmox should validate options before checking ISO:\n%s", errOutput)
	}
}

func TestRunProxmoxBuildAcceptsOptionFilesBeforeISOCheck(t *testing.T) {
	dir := t.TempDir()
	userDataPath := filepath.Join(dir, "user-data.yaml")
	if err := os.WriteFile(userDataPath, []byte(`#cloud-config
autoinstall:
  version: 1
`), 0o644); err != nil {
		t.Fatalf("write user-data: %v", err)
	}
	hardwareConfigPath := writeTempHardwareConfig(t, "disk_size: 20G\nproxmox:\n  bridge: vmbr0\n")
	optionsPath := filepath.Join(dir, "options.yaml")
	if err := os.WriteFile(optionsPath, []byte("start_at_boot: false\n"), 0o644); err != nil {
		t.Fatalf("write options: %v", err)
	}
	cloudInitPath := filepath.Join(dir, "cloud-init.yaml")
	if err := os.WriteFile(cloudInitPath, []byte("type: nocloud\nupgrade: false\n"), 0o644); err != nil {
		t.Fatalf("write cloud-init options: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{
		"ubuntu-vm-template-builder",
		"proxmox",
		"build",
		"--iso", "missing.iso",
		"--user-data", userDataPath,
		"--hardware-config", hardwareConfigPath,
		"--proxmox-host", "pve.example.com",
		"--proxmox-token-id", "root@pam!builder",
		"--proxmox-token-secret", "secret",
		"--proxmox-node", "pve",
		"--proxmox-iso-storage", "local",
		"--proxmox-disk-storage", "vms",
		"--options", optionsPath,
		"--cloud-init-options", cloudInitPath,
	}, &stdout, &stderr)
	if code == 0 {
		t.Fatal("proxmox build unexpectedly reached success with missing ISO")
	}
	errOutput := stderr.String()
	if !strings.Contains(errOutput, "ubuntu ISO file") {
		t.Fatalf("proxmox should proceed past option parsing to ISO validation:\n%s", errOutput)
	}
	if strings.Contains(errOutput, "Proxmox options") || strings.Contains(errOutput, "Proxmox cloud-init options") {
		t.Fatalf("valid option files were rejected:\n%s", errOutput)
	}
}

func TestEffectiveProxmoxBridgePrefersCLI(t *testing.T) {
	hardware := common.DefaultHardwareConfig()
	hardware.DiskSize = "20G"
	hardware.Proxmox.Bridge = "yaml-bridge"

	if got := effectiveProxmoxBridge(" cli-bridge ", hardware); got != "cli-bridge" {
		t.Fatalf("effectiveProxmoxBridge with CLI = %q, want cli-bridge", got)
	}
	if got := effectiveProxmoxBridge("", hardware); got != "yaml-bridge" {
		t.Fatalf("effectiveProxmoxBridge without CLI = %q, want yaml-bridge", got)
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

func TestBuildCommandsRejectInvalidInstallExtraPackagesConfig(t *testing.T) {
	dir := t.TempDir()
	userDataPath := filepath.Join(dir, "user-data.yaml")
	if err := os.WriteFile(userDataPath, []byte(`#cloud-config
autoinstall:
  version: 1
`), 0o644); err != nil {
		t.Fatalf("write user-data: %v", err)
	}
	qemuHardwarePath := writeTempHardwareConfig(t, "disk_size: 20G\nboot_firmware: bios\n")
	vcenterHardwarePath := writeTempHardwareConfig(t, "disk_size: 20G\nvcenter:\n  network: VM Network\n")
	proxmoxHardwarePath := writeTempHardwareConfig(t, "disk_size: 20G\nproxmox:\n  bridge: vmbr0\n")
	extraPath := filepath.Join(dir, "extra-packages.yaml")
	if err := os.WriteFile(extraPath, []byte("apt_url: http://archive.ubuntu.com/ubuntu\n"), 0o644); err != nil {
		t.Fatalf("write extra packages config: %v", err)
	}

	tests := []struct {
		name string
		args []string
	}{
		{
			name: "qemu",
			args: []string{
				"ubuntu-vm-template-builder",
				"qemu",
				"build",
				"--iso", "missing.iso",
				"--image", filepath.Join(dir, "output.img"),
				"--user-data", userDataPath,
				"--hardware-config", qemuHardwarePath,
				"--install-extra-packages", extraPath,
			},
		},
		{
			name: "vcenter",
			args: []string{
				"ubuntu-vm-template-builder",
				"vcenter",
				"build",
				"--iso", "missing.iso",
				"--user-data", userDataPath,
				"--hardware-config", vcenterHardwarePath,
				"--vcenter-host", "vc.example.com",
				"--vcenter-username", "administrator@vsphere.local",
				"--vcenter-password", "secret",
				"--vcenter-datacenter", "DC0",
				"--vcenter-esxi-host", "esxi.example.com",
				"--vcenter-datastore", "datastore1",
				"--vcenter-folder", "vm",
				"--install-extra-packages", extraPath,
			},
		},
		{
			name: "proxmox",
			args: []string{
				"ubuntu-vm-template-builder",
				"proxmox",
				"build",
				"--iso", "missing.iso",
				"--user-data", userDataPath,
				"--hardware-config", proxmoxHardwarePath,
				"--proxmox-host", "pve.example.com",
				"--proxmox-token-id", "root@pam!builder",
				"--proxmox-token-secret", "secret",
				"--proxmox-node", "pve",
				"--proxmox-iso-storage", "local",
				"--proxmox-disk-storage", "vms",
				"--install-extra-packages", extraPath,
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(test.args, &stdout, &stderr)
			if code == 0 {
				t.Fatalf("%s build returned success with invalid extra package config", test.name)
			}
			errOutput := stderr.String()
			if !strings.Contains(errOutput, "install extra packages config") || !strings.Contains(errOutput, "packages") {
				t.Fatalf("%s invalid extra package config error is unexpected:\n%s", test.name, errOutput)
			}
			if strings.Contains(errOutput, "ubuntu ISO file") {
				t.Fatalf("%s should validate extra package config before checking ISO:\n%s", test.name, errOutput)
			}
		})
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
		"proxmox",
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
	if output := out.String(); !strings.Contains(output, "--hardware-config") || !strings.Contains(output, "--install-extra-packages") || !strings.Contains(output, "qemu build") || strings.Contains(output, "--boot-mode") || strings.Contains(output, "--disk-size") {
		t.Fatalf("qemu build usage should mention --hardware-config and not removed flags:\n%s", output)
	}

	out.Reset()
	printVCenterUsage(&out, "ubuntu-vm-template-builder")
	if output := out.String(); !strings.Contains(output, commandBuild) || strings.Contains(output, "--hardware-config") || strings.Contains(output, "--vm-cpu") || strings.Contains(output, "--vm-memory-mb") {
		t.Fatalf("vcenter backend usage should mention build command and not build flags:\n%s", output)
	}

	out.Reset()
	printVCenterBuildUsage(&out, "ubuntu-vm-template-builder")
	if output := out.String(); !strings.Contains(output, "--hardware-config") || !strings.Contains(output, "--install-extra-packages") || !strings.Contains(output, "vcenter build") || strings.Contains(output, "--disk-size") || strings.Contains(output, "--vm-cpu") || strings.Contains(output, "--vm-memory-mb") {
		t.Fatalf("vcenter build usage should mention --hardware-config and not removed hardware flags:\n%s", output)
	}

	out.Reset()
	printProxmoxUsage(&out, "ubuntu-vm-template-builder")
	if output := out.String(); !strings.Contains(output, commandBuild) || strings.Contains(output, commandUpload) || strings.Contains(output, "--hardware-config") || strings.Contains(output, "--proxmox-host") {
		t.Fatalf("proxmox backend usage should mention commands and not build flags:\n%s", output)
	}

	out.Reset()
	printProxmoxBuildUsage(&out, "ubuntu-vm-template-builder")
	if output := out.String(); !strings.Contains(output, "--hardware-config") || !strings.Contains(output, "--install-extra-packages") || !strings.Contains(output, "--options") || !strings.Contains(output, "--cloud-init-options") || !strings.Contains(output, "proxmox build") || !strings.Contains(output, "--proxmox-host") || !strings.Contains(output, "--proxmox-iso-storage") || !strings.Contains(output, "--proxmox-disk-storage") || strings.Contains(output, "--proxmox-storage") || strings.Contains(output, "--disk-size") || strings.Contains(output, "--vm-cpu") || strings.Contains(output, "--vm-memory-mb") {
		t.Fatalf("proxmox build usage should mention --hardware-config and not removed hardware flags:\n%s", output)
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
