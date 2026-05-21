package common

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateUserDataReturnsHostname(t *testing.T) {
	data := []byte(`#cloud-config
autoinstall:
  version: 1
  identity:
    hostname: test-host
`)

	hostname, err := ValidateUserData(data)
	if err != nil {
		t.Fatalf("ValidateUserData returned error: %v", err)
	}
	if hostname != "test-host" {
		t.Fatalf("hostname = %q, want %q", hostname, "test-host")
	}
}

func TestValidateUserDataRequiresAutoinstall(t *testing.T) {
	_, err := ValidateUserData([]byte("not_autoinstall: true\n"))
	if err == nil {
		t.Fatal("ValidateUserData returned nil error for missing autoinstall")
	}
}

func TestValidateUEFIPortableUserDataAcceptsDefaultStorageWithFallback(t *testing.T) {
	data := []byte(`#cloud-config
autoinstall:
  version: 1
  late-commands:
    - curtin in-target --target=/target -- grub-install --target=x86_64-efi --efi-directory=/boot/efi --bootloader-id=ubuntu --removable --recheck
`)

	if err := ValidateUEFIPortableUserData(data); err != nil {
		t.Fatalf("ValidateUEFIPortableUserData returned error: %v", err)
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

	if err := ValidateUEFIPortableUserData(data); err != nil {
		t.Fatalf("ValidateUEFIPortableUserData returned error: %v", err)
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

	err := ValidateUEFIPortableUserData(data)
	if err == nil {
		t.Fatal("ValidateUEFIPortableUserData returned nil error without fallback command")
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

	err := ValidateUEFIPortableUserData(data)
	if err == nil {
		t.Fatal("ValidateUEFIPortableUserData returned nil error for BIOS-style storage")
	}
	if !strings.Contains(err.Error(), "EFI System Partition") || !strings.Contains(err.Error(), "/boot/efi") {
		t.Fatalf("error %q does not explain missing ESP", err.Error())
	}
}

func TestExampleUserDataFiles(t *testing.T) {
	uefiData, err := os.ReadFile(filepath.Join("..", "..", "autoinstall.uefi.example.yaml"))
	if err != nil {
		t.Fatalf("read UEFI example: %v", err)
	}
	if _, err := ValidateUserData(uefiData); err != nil {
		t.Fatalf("UEFI example failed user-data validation: %v", err)
	}
	if err := ValidateUEFIPortableUserData(uefiData); err != nil {
		t.Fatalf("UEFI example failed UEFI portability validation: %v", err)
	}

	biosData, err := os.ReadFile(filepath.Join("..", "..", "autoinstall.bios.example.yaml"))
	if err != nil {
		t.Fatalf("read BIOS example: %v", err)
	}
	if _, err := ValidateUserData(biosData); err != nil {
		t.Fatalf("BIOS example failed user-data validation: %v", err)
	}
	if err := ValidateUEFIPortableUserData(biosData); err == nil {
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
		got, err := ParseDiskSize(input)
		if err != nil {
			t.Fatalf("ParseDiskSize(%q) returned error: %v", input, err)
		}
		if got != want {
			t.Fatalf("ParseDiskSize(%q) = %d, want %d", input, got, want)
		}
	}

	if _, err := ParseDiskSize("bad"); err == nil {
		t.Fatal("ParseDiskSize returned nil error for invalid size")
	}
}

func TestEnsureWritableNewFileRejectsExistingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "existing.img")
	if err := os.WriteFile(path, []byte("exists"), 0o644); err != nil {
		t.Fatalf("create temp file: %v", err)
	}

	if err := EnsureWritableNewFile(path); err == nil {
		t.Fatal("EnsureWritableNewFile returned nil error for existing output file")
	}
}

func TestEnsureWritableNewFileAcceptsWritableParent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "new.img")

	if err := EnsureWritableNewFile(path); err != nil {
		t.Fatalf("EnsureWritableNewFile returned error: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("EnsureWritableNewFile created output path or returned unexpected stat error: %v", err)
	}
}

func TestNormalizeBootFirmwareDefaultsToUEFI(t *testing.T) {
	if got := NormalizeBootFirmware(""); got != BootFirmwareUEFI {
		t.Fatalf("NormalizeBootFirmware empty = %q, want %q", got, BootFirmwareUEFI)
	}
	if got := NormalizeBootFirmware(" BIOS "); got != BootFirmwareBIOS {
		t.Fatalf("NormalizeBootFirmware trims and lowercases to %q, want %q", got, BootFirmwareBIOS)
	}
}

func TestValidateBootFirmware(t *testing.T) {
	for _, firmware := range []string{"", "uefi", "UEFI", "bios", " BIOS "} {
		if !ValidateBootFirmware(firmware) {
			t.Fatalf("ValidateBootFirmware(%q) = false, want true", firmware)
		}
	}
	if ValidateBootFirmware("legacy") {
		t.Fatal("ValidateBootFirmware returned true for unsupported firmware")
	}
}

func TestLoadHardwareConfigDefaults(t *testing.T) {
	cfg, err := LoadHardwareConfig("")
	if err != nil {
		t.Fatalf("LoadHardwareConfig returned error: %v", err)
	}
	if cfg.BootFirmware != BootFirmwareUEFI || cfg.VCPU != 2 || cfg.MemoryMB != 2048 {
		t.Fatalf("default hardware config = %+v", cfg)
	}
	if cfg.DiskSize != "" {
		t.Fatalf("default DiskSize = %q, want empty", cfg.DiskSize)
	}
	if cfg.QEMU.CPUModel != "host" || cfg.QEMU.DiskInterface != "virtio" || cfg.QEMU.ISOInterface != "virtio" {
		t.Fatalf("default qemu hardware config = %+v", cfg.QEMU)
	}
	if cfg.VCenter.SCSIController != "pvscsi" || cfg.VCenter.NetworkAdapter != "vmxnet3" || cfg.VCenter.Network != "" || cfg.VCenter.DiskProvisioning != VCenterDiskProvisioningThickLazyZeroed || cfg.VCenter.Compatibility != "" || cfg.VCenter.GuestOSID != DefaultVCenterGuestOSID || cfg.VCenter.ReserveAllGuestMemory || cfg.VCenter.OutputType != VCenterOutputTypeTemplate {
		t.Fatalf("default vcenter hardware config = %+v", cfg.VCenter)
	}
	if cfg.Proxmox.Bridge != "" || cfg.Proxmox.NetworkAdapter != DefaultProxmoxNetworkAdapter || cfg.Proxmox.SCSIController != DefaultProxmoxSCSIController || cfg.Proxmox.DiskInterface != DefaultProxmoxDiskInterface || cfg.Proxmox.DiskFormat != DefaultProxmoxDiskFormat || cfg.Proxmox.CPUType != DefaultProxmoxCPUType || cfg.Proxmox.Machine != DefaultProxmoxMachine || cfg.Proxmox.OSType != DefaultProxmoxOSType || cfg.Proxmox.EFIType != DefaultProxmoxEFIType || cfg.Proxmox.PreEnrolledKeys || cfg.Proxmox.OutputType != ProxmoxOutputTypeTemplate {
		t.Fatalf("default proxmox hardware config = %+v", cfg.Proxmox)
	}
}

func TestLoadHardwareConfigCustom(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hardware.yaml")
	data := []byte(`
boot_firmware: BIOS
disk_size: " 25G "
vcpu: 4
memory_mb: 8192
qemu:
  cpu_model: " max "
  disk_interface: SCSI
  iso_interface: SATA
vcenter:
  scsi_controller: LSILOGIC-SAS
  network_adapter: E1000E
  network: " VM Network "
  disk_provisioning: Thick Provision Eager Zeroed
  compatibility: VMX-21
  guest_os_id: " otherLinux64Guest "
  reserve_all_guest_memory: true
  output_type: VM
proxmox:
  bridge: " vmbr1 "
  network_adapter: E1000E
  scsi_controller: VIRTIO-SCSI-SINGLE
  disk_interface: VIRTIO
  disk_format: QCOW2
  cpu_type: " x86-64-v3 "
  machine: " q35 "
  ostype: " l26 "
  efi_type: 2M
  pre_enrolled_keys: true
  output_type: VM
`)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write hardware config: %v", err)
	}

	cfg, err := LoadHardwareConfig(path)
	if err != nil {
		t.Fatalf("LoadHardwareConfig returned error: %v", err)
	}
	if cfg.BootFirmware != BootFirmwareBIOS || cfg.DiskSize != "25G" || cfg.VCPU != 4 || cfg.MemoryMB != 8192 {
		t.Fatalf("custom hardware config = %+v", cfg)
	}
	if cfg.QEMU.CPUModel != "max" || cfg.QEMU.DiskInterface != "scsi" || cfg.QEMU.ISOInterface != "sata" {
		t.Fatalf("custom qemu hardware config = %+v", cfg.QEMU)
	}
	if cfg.VCenter.SCSIController != "lsilogic-sas" || cfg.VCenter.NetworkAdapter != "e1000e" || cfg.VCenter.Network != "VM Network" || cfg.VCenter.DiskProvisioning != VCenterDiskProvisioningThickEagerZeroed || cfg.VCenter.Compatibility != "vmx-21" || cfg.VCenter.GuestOSID != "otherLinux64Guest" || !cfg.VCenter.ReserveAllGuestMemory || cfg.VCenter.OutputType != VCenterOutputTypeVM {
		t.Fatalf("custom vcenter hardware config = %+v", cfg.VCenter)
	}
	if cfg.Proxmox.Bridge != "vmbr1" || cfg.Proxmox.NetworkAdapter != "e1000e" || cfg.Proxmox.SCSIController != "virtio-scsi-single" || cfg.Proxmox.DiskInterface != "virtio" || cfg.Proxmox.DiskFormat != "qcow2" || cfg.Proxmox.CPUType != "x86-64-v3" || cfg.Proxmox.Machine != "q35" || cfg.Proxmox.OSType != "l26" || cfg.Proxmox.EFIType != "2m" || !cfg.Proxmox.PreEnrolledKeys || cfg.Proxmox.OutputType != ProxmoxOutputTypeVM {
		t.Fatalf("custom proxmox hardware config = %+v", cfg.Proxmox)
	}
}

func TestLoadHardwareConfigPartialWithoutDiskSizeFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hardware.yaml")
	if err := os.WriteFile(path, []byte("boot_firmware: bios\n"), 0o644); err != nil {
		t.Fatalf("write hardware config: %v", err)
	}

	_, err := LoadHardwareConfig(path)
	if err == nil {
		t.Fatal("LoadHardwareConfig returned nil error without disk_size")
	}
	if !strings.Contains(err.Error(), "disk_size") {
		t.Fatalf("error %q does not mention disk_size", err.Error())
	}
}

func TestLoadHardwareConfigPartialKeepsDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hardware.yaml")
	if err := os.WriteFile(path, []byte("boot_firmware: bios\ndisk_size: 30G\n"), 0o644); err != nil {
		t.Fatalf("write hardware config: %v", err)
	}

	cfg, err := LoadHardwareConfig(path)
	if err != nil {
		t.Fatalf("LoadHardwareConfig returned error: %v", err)
	}
	if cfg.BootFirmware != BootFirmwareBIOS {
		t.Fatalf("BootFirmware = %q, want bios", cfg.BootFirmware)
	}
	if cfg.DiskSize != "30G" {
		t.Fatalf("DiskSize = %q, want 30G", cfg.DiskSize)
	}
	if cfg.VCPU != 2 || cfg.MemoryMB != 2048 {
		t.Fatalf("partial config did not keep CPU/memory defaults: %+v", cfg)
	}
	if cfg.QEMU.CPUModel != "host" || cfg.QEMU.DiskInterface != "virtio" || cfg.QEMU.ISOInterface != "virtio" {
		t.Fatalf("partial config did not keep qemu defaults: %+v", cfg.QEMU)
	}
	if cfg.VCenter.SCSIController != "pvscsi" || cfg.VCenter.NetworkAdapter != "vmxnet3" || cfg.VCenter.Network != "" || cfg.VCenter.DiskProvisioning != VCenterDiskProvisioningThickLazyZeroed || cfg.VCenter.Compatibility != "" || cfg.VCenter.GuestOSID != DefaultVCenterGuestOSID || cfg.VCenter.ReserveAllGuestMemory || cfg.VCenter.OutputType != VCenterOutputTypeTemplate {
		t.Fatalf("partial config did not keep vcenter defaults: %+v", cfg.VCenter)
	}
	if cfg.Proxmox.Bridge != "" || cfg.Proxmox.NetworkAdapter != DefaultProxmoxNetworkAdapter || cfg.Proxmox.SCSIController != DefaultProxmoxSCSIController || cfg.Proxmox.DiskInterface != DefaultProxmoxDiskInterface || cfg.Proxmox.DiskFormat != DefaultProxmoxDiskFormat || cfg.Proxmox.CPUType != DefaultProxmoxCPUType || cfg.Proxmox.Machine != DefaultProxmoxMachine || cfg.Proxmox.OSType != DefaultProxmoxOSType || cfg.Proxmox.EFIType != DefaultProxmoxEFIType || cfg.Proxmox.PreEnrolledKeys || cfg.Proxmox.OutputType != ProxmoxOutputTypeTemplate {
		t.Fatalf("partial config did not keep proxmox defaults: %+v", cfg.Proxmox)
	}
}

func TestLoadHardwareConfigRejectsUnknownField(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hardware.yaml")
	if err := os.WriteFile(path, []byte("unknown: true\n"), 0o644); err != nil {
		t.Fatalf("write hardware config: %v", err)
	}

	if _, err := LoadHardwareConfig(path); err == nil {
		t.Fatal("LoadHardwareConfig returned nil error for unknown field")
	}
}

func TestHardwareConfigValidationRejectsInvalidValues(t *testing.T) {
	cfg := validHardwareConfig()
	cfg.BootFirmware = "legacy"
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate returned nil error for invalid boot firmware")
	}

	cfg = validHardwareConfig()
	cfg.DiskSize = ""
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate returned nil error for missing disk_size")
	}

	cfg = validHardwareConfig()
	cfg.DiskSize = "bad"
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate returned nil error for invalid disk_size")
	}

	cfg = validHardwareConfig()
	cfg.VCPU = 0
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate returned nil error for invalid vcpu")
	}

	cfg = validHardwareConfig()
	cfg.VCenter.NetworkAdapter = "rtl8139"
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate returned nil error for invalid vCenter NIC")
	}

	cfg = validHardwareConfig()
	cfg.VCenter.DiskProvisioning = "two_growable"
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate returned nil error for invalid vCenter disk provisioning")
	}

	cfg = validHardwareConfig()
	cfg.VCenter.Compatibility = "vmx-latest"
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate returned nil error for invalid vCenter compatibility")
	}

	cfg = validHardwareConfig()
	cfg.VCenter.GuestOSID = ""
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate returned nil error for empty vCenter guest OS ID")
	}

	cfg = validHardwareConfig()
	cfg.VCenter.OutputType = "snapshot"
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate returned nil error for invalid vCenter output type")
	}

	cfg = validHardwareConfig()
	cfg.Proxmox.NetworkAdapter = "ne2k"
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate returned nil error for invalid Proxmox NIC")
	}

	cfg = validHardwareConfig()
	cfg.Proxmox.SCSIController = "bad-scsi"
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate returned nil error for invalid Proxmox SCSI controller")
	}

	cfg = validHardwareConfig()
	cfg.Proxmox.DiskInterface = "nvme"
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate returned nil error for invalid Proxmox disk interface")
	}

	cfg = validHardwareConfig()
	cfg.Proxmox.DiskFormat = "vdi"
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate returned nil error for invalid Proxmox disk format")
	}

	cfg = validHardwareConfig()
	cfg.Proxmox.CPUType = ""
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate returned nil error for empty Proxmox CPU type")
	}

	cfg = validHardwareConfig()
	cfg.Proxmox.EFIType = "8m"
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate returned nil error for invalid Proxmox EFI type")
	}

	cfg = validHardwareConfig()
	cfg.Proxmox.OutputType = "snapshot"
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate returned nil error for invalid Proxmox output type")
	}
}

func validHardwareConfig() HardwareConfig {
	cfg := DefaultHardwareConfig()
	cfg.DiskSize = "20G"
	return cfg
}
