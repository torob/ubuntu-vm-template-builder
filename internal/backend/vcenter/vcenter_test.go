package vcenter

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vmware/govmomi"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
	"gopkg.in/yaml.v3"

	"ubuntu-vm-template-builder/internal/common"
)

func TestTransformUserDataAddsPoweroffShutdown(t *testing.T) {
	original := []byte(`#cloud-config
autoinstall:
  version: 1
  identity:
    hostname: vmware-template
`)
	originalCopy := append([]byte(nil), original...)

	transformed, err := TransformUserData(original)
	if err != nil {
		t.Fatalf("TransformUserData returned error: %v", err)
	}
	if !bytes.Equal(original, originalCopy) {
		t.Fatalf("TransformUserData modified input bytes")
	}
	if !bytes.HasPrefix(transformed, []byte("#cloud-config\n")) {
		t.Fatalf("transformed user-data missing cloud-config header:\n%s", transformed)
	}

	autoinstall, err := common.ParseAutoinstallMapping(transformed)
	if err != nil {
		t.Fatalf("parse transformed user-data: %v", err)
	}
	shutdown := common.MappingValue(autoinstall, "shutdown")
	if shutdown == nil || shutdown.Kind != yaml.ScalarNode || shutdown.Value != "poweroff" {
		t.Fatalf("transformed shutdown = %#v, want scalar poweroff", shutdown)
	}
}

func TestCreateNoCloudSeedDirDoesNotModifySourceFile(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "user-data")
	original := []byte(`#cloud-config
autoinstall:
  version: 1
  identity:
    hostname: seeded-template
`)
	if err := os.WriteFile(sourcePath, original, 0o644); err != nil {
		t.Fatalf("write source user-data: %v", err)
	}

	sourceBytes, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatalf("read source user-data: %v", err)
	}
	seedDir, err := CreateNoCloudSeedDir(dir, sourceBytes, "seeded-template")
	if err != nil {
		t.Fatalf("CreateNoCloudSeedDir returned error: %v", err)
	}

	after, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatalf("read source after seed generation: %v", err)
	}
	if !bytes.Equal(after, original) {
		t.Fatalf("source user-data changed on disk:\n%s", after)
	}

	generatedUserData, err := os.ReadFile(filepath.Join(seedDir, "user-data"))
	if err != nil {
		t.Fatalf("read generated user-data: %v", err)
	}
	if !strings.Contains(string(generatedUserData), "shutdown: poweroff") {
		t.Fatalf("generated user-data missing shutdown poweroff:\n%s", generatedUserData)
	}

	metaData, err := os.ReadFile(filepath.Join(seedDir, "meta-data"))
	if err != nil {
		t.Fatalf("read generated meta-data: %v", err)
	}
	for _, want := range []string{"instance-id: iid-seeded-template", "local-hostname: seeded-template"} {
		if !strings.Contains(string(metaData), want) {
			t.Fatalf("generated meta-data missing %q in:\n%s", want, metaData)
		}
	}
}

func TestAddAutoinstallKernelArgs(t *testing.T) {
	input := []byte("set timeout=15\nmenuentry 'Install Ubuntu' {\n  linux /casper/vmlinuz quiet ---\n}\n")

	got, changed := AddAutoinstallKernelArgs(input)
	if !changed {
		t.Fatal("AddAutoinstallKernelArgs did not report a change")
	}
	line := string(got)
	for _, want := range []string{
		grubTimeoutStyleSetting,
		grubTimeoutSetting,
		"autoinstall",
		GrubNoCloudKernelArg,
		ConsoleTTY0KernelArg,
		ConsoleTTYS0KernelArg,
		"autoinstall ds=nocloud\\;s=/cdrom/nocloud/ console=tty0 console=ttyS0,115200n8 ---",
	} {
		if !strings.Contains(line, want) {
			t.Fatalf("patched boot config missing %q in:\n%s", want, line)
		}
	}
	if strings.Contains(line, "set timeout=15") {
		t.Fatalf("patched GRUB config still has installer countdown:\n%s", line)
	}
	if strings.Contains(line, NoCloudKernelArg) {
		t.Fatalf("patched GRUB config contains unescaped NoCloud semicolon:\n%s", line)
	}
}

func TestAddSyslinuxAutoinstallKernelArgs(t *testing.T) {
	input := []byte("timeout 50\nprompt 1\nappend initrd=/casper/initrd quiet ---\n")

	got, changed := AddSyslinuxAutoinstallKernelArgs(input)
	if !changed {
		t.Fatal("AddSyslinuxAutoinstallKernelArgs did not report a change")
	}
	line := string(got)
	for _, want := range []string{
		syslinuxTimeoutSetting,
		syslinuxPromptSetting,
		"autoinstall",
		NoCloudKernelArg,
		ConsoleTTY0KernelArg,
		ConsoleTTYS0KernelArg,
		"autoinstall ds=nocloud;s=/cdrom/nocloud/ console=tty0 console=ttyS0,115200n8 ---",
	} {
		if !strings.Contains(line, want) {
			t.Fatalf("patched Syslinux config missing %q in:\n%s", want, line)
		}
	}
	if strings.Contains(line, "timeout 50") || strings.Contains(line, "prompt 1") {
		t.Fatalf("patched Syslinux config still has installer countdown:\n%s", line)
	}
}

func TestBuildURLAddsSDKPathAndCredentials(t *testing.T) {
	u, err := BuildURL(ConnectionConfig{
		Host:     "vc.example.com",
		Username: "administrator@vsphere.local",
		Password: "secret",
	})
	if err != nil {
		t.Fatalf("BuildURL returned error: %v", err)
	}
	if u.String() != "https://administrator%40vsphere.local:secret@vc.example.com/sdk" {
		t.Fatalf("BuildURL = %q", u.String())
	}
}

func TestMaybeMarkAsTemplateHonorsOutputType(t *testing.T) {
	ctx := context.Background()

	templateMarker := &fakeTemplateMarker{}
	if err := maybeMarkAsTemplate(ctx, templateMarker, common.VCenterOutputTypeTemplate); err != nil {
		t.Fatalf("maybeMarkAsTemplate template returned error: %v", err)
	}
	if templateMarker.calls != 1 {
		t.Fatalf("template MarkAsTemplate calls = %d, want 1", templateMarker.calls)
	}

	defaultMarker := &fakeTemplateMarker{}
	if err := maybeMarkAsTemplate(ctx, defaultMarker, ""); err != nil {
		t.Fatalf("maybeMarkAsTemplate default returned error: %v", err)
	}
	if defaultMarker.calls != 1 {
		t.Fatalf("default MarkAsTemplate calls = %d, want 1", defaultMarker.calls)
	}

	vmMarker := &fakeTemplateMarker{}
	if err := maybeMarkAsTemplate(ctx, vmMarker, common.VCenterOutputTypeVM); err != nil {
		t.Fatalf("maybeMarkAsTemplate vm returned error: %v", err)
	}
	if vmMarker.calls != 0 {
		t.Fatalf("vm MarkAsTemplate calls = %d, want 0", vmMarker.calls)
	}

	wantErr := errors.New("mark failed")
	errorMarker := &fakeTemplateMarker{err: wantErr}
	if err := maybeMarkAsTemplate(ctx, errorMarker, common.VCenterOutputTypeTemplate); !errors.Is(err, wantErr) {
		t.Fatalf("maybeMarkAsTemplate error = %v, want %v", err, wantErr)
	}
}

func TestNewInstallerDoesNotCreateTempDirBeforePreflight(t *testing.T) {
	installer, err := NewInstaller(simulatorVCenterConfig("deferred-temp"))
	if err != nil {
		t.Fatalf("NewInstaller returned error: %v", err)
	}
	if installer.tempDir != "" {
		t.Fatalf("tempDir = %q, want empty before ISO remastering", installer.tempDir)
	}
}

func TestConnectAndResolvePlacementValidatesSimulatorPlacement(t *testing.T) {
	runVPXSimulatorService(t, func(ctx context.Context, serverURL *url.URL) {
		cfg := simulatorVCenterConfig("preflight")
		cfg.VCenter.Host = serverURL.String()
		cfg.VCenter.Username = serverURL.User.Username()
		cfg.VCenter.Password, _ = serverURL.User.Password()
		cfg.VCenter.Insecure = true

		client, placement, err := connectAndResolvePlacement(ctx, cfg.VCenter)
		if err != nil {
			t.Fatalf("connectAndResolvePlacement returned error: %v", err)
		}
		defer client.Logout(ctx)
		if placement.Datacenter == nil || placement.Host == nil || placement.Datastore == nil || placement.Folder == nil || placement.Network == nil || placement.ResourcePool == nil {
			t.Fatalf("placement has nil fields: %#v", placement)
		}
	})
}

func TestValidateTargetNameAvailableRejectsExistingTargetInSelectedFolder(t *testing.T) {
	runVPXSimulator(t, func(ctx context.Context, client *vim25.Client) {
		cfg := simulatorVCenterConfig("duplicate-target")
		placement, err := ResolvePlacement(ctx, client, cfg.VCenter)
		if err != nil {
			t.Fatalf("ResolvePlacement returned error: %v", err)
		}
		if _, err := createSimulatorVM(ctx, client, cfg, placement); err != nil {
			t.Fatalf("create existing simulator VM: %v", err)
		}

		err = validateTargetNameAvailable(ctx, client, cfg.VCenter, placement)
		if err == nil {
			t.Fatal("validateTargetNameAvailable returned nil error for existing target")
		}
		for _, want := range []string{
			`target VM/template name "duplicate-target" already exists`,
			`/DC0/vm/duplicate-target`,
			"VirtualMachine:",
		} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("error %q missing %q", err.Error(), want)
			}
		}
	})
}

func TestValidateTargetNameAvailableAllowsSameNameInDifferentFolder(t *testing.T) {
	runVPXSimulator(t, func(ctx context.Context, client *vim25.Client) {
		cfg := simulatorVCenterConfig("same-name-different-folder")
		placement, err := ResolvePlacement(ctx, client, cfg.VCenter)
		if err != nil {
			t.Fatalf("ResolvePlacement returned error: %v", err)
		}

		baseFolderPath, err := resolvedFolderInventoryPath(ctx, client, placement.Folder)
		if err != nil {
			t.Fatalf("resolve base folder path: %v", err)
		}
		otherFolder, err := placement.Folder.CreateFolder(ctx, "OtherTemplates")
		if err != nil {
			t.Fatalf("create other folder: %v", err)
		}
		otherFolder.SetInventoryPath(path.Join(baseFolderPath, "OtherTemplates"))

		otherPlacement := *placement
		otherPlacement.Folder = otherFolder
		if _, err := createSimulatorVM(ctx, client, cfg, &otherPlacement); err != nil {
			t.Fatalf("create same-name VM in different folder: %v", err)
		}

		if err := validateTargetNameAvailable(ctx, client, cfg.VCenter, placement); err != nil {
			t.Fatalf("validateTargetNameAvailable returned error for same name in different folder: %v", err)
		}
	})
}

func TestUploadInstallerISOThroughSimulatorDatastore(t *testing.T) {
	runVPXSimulator(t, func(ctx context.Context, client *vim25.Client) {
		cfg := simulatorVCenterConfig("upload-test")
		placement, err := ResolvePlacement(ctx, client, cfg.VCenter)
		if err != nil {
			t.Fatalf("ResolvePlacement returned error: %v", err)
		}

		localISOPath := filepath.Join(t.TempDir(), "installer.iso")
		if err := os.WriteFile(localISOPath, []byte("test iso"), 0o644); err != nil {
			t.Fatalf("write test ISO: %v", err)
		}

		remotePath, datastorePath, err := uploadInstallerISO(ctx, placement, localISOPath, cfg.VCenter.Name)
		if err != nil {
			t.Fatalf("uploadInstallerISO returned error: %v", err)
		}
		if !strings.HasPrefix(remotePath, "ubuntu-vm-template-builder-upload-test-") || !strings.HasSuffix(remotePath, ".iso") {
			t.Fatalf("remotePath = %q, want generated upload path", remotePath)
		}
		if datastorePath != placement.Datastore.Path(remotePath) {
			t.Fatalf("datastorePath = %q, want %q", datastorePath, placement.Datastore.Path(remotePath))
		}

		if err := deleteDatastoreISO(ctx, placement, remotePath); err != nil {
			t.Fatalf("delete uploaded test ISO: %v", err)
		}
	})
}

func TestCleanupInterruptedVCenterBuildDestroysVMAndDeletesArtifacts(t *testing.T) {
	runVPXSimulator(t, func(ctx context.Context, client *vim25.Client) {
		cfg := simulatorVCenterConfig("interrupt-cleanup")
		placement, err := ResolvePlacement(ctx, client, cfg.VCenter)
		if err != nil {
			t.Fatalf("ResolvePlacement returned error: %v", err)
		}
		vm, err := createSimulatorVM(ctx, client, cfg, placement)
		if err != nil {
			t.Fatalf("create simulator VM: %v", err)
		}

		tempDir := t.TempDir()
		remoteISOPath := "interrupt-cleanup.iso"
		localISOPath := filepath.Join(tempDir, "installer.iso")
		if err := os.WriteFile(localISOPath, []byte("test iso"), 0o644); err != nil {
			t.Fatalf("write local installer ISO: %v", err)
		}
		if err := uploadDatastoreFile(ctx, placement, localISOPath, remoteISOPath); err != nil {
			t.Fatalf("upload installer ISO: %v", err)
		}

		remoteConsoleLogPath := "interrupt-cleanup-console.log"
		localConsoleLogPath := filepath.Join(tempDir, "console.log")
		if err := os.WriteFile(localConsoleLogPath, []byte("console output\n"), 0o644); err != nil {
			t.Fatalf("write local console log: %v", err)
		}
		if err := uploadDatastoreFile(ctx, placement, localConsoleLogPath, remoteConsoleLogPath); err != nil {
			t.Fatalf("upload console log: %v", err)
		}

		state := &vCenterBuildState{
			cfg:                     cfg.VCenter,
			client:                  client,
			placement:               placement,
			vm:                      vm,
			remoteISOPath:           remoteISOPath,
			datastoreISOPath:        placement.Datastore.Path(remoteISOPath),
			remoteConsoleLogPath:    remoteConsoleLogPath,
			datastoreConsoleLogPath: placement.Datastore.Path(remoteConsoleLogPath),
			vmCreated:               true,
		}

		cleanupInterruptedVCenterBuild(ctx, state)

		assertTargetVMNotFound(t, ctx, client, cfg.VCenter, placement)
		assertDatastoreFileMissing(t, ctx, placement.Datastore, remoteISOPath)
		assertDatastoreFileMissing(t, ctx, placement.Datastore, remoteConsoleLogPath)
		if !state.isoDeleted || !state.consoleLogDeleted || state.vmCreated {
			t.Fatalf("cleanup state = %#v, want ISO/log deleted and VM not created", state)
		}
	})
}

func TestCleanupInterruptedVCenterBuildFindsVMByInventoryPath(t *testing.T) {
	runVPXSimulator(t, func(ctx context.Context, client *vim25.Client) {
		cfg := simulatorVCenterConfig("interrupt-find-vm")
		placement, err := ResolvePlacement(ctx, client, cfg.VCenter)
		if err != nil {
			t.Fatalf("ResolvePlacement returned error: %v", err)
		}
		if _, err := createSimulatorVM(ctx, client, cfg, placement); err != nil {
			t.Fatalf("create simulator VM: %v", err)
		}

		state := &vCenterBuildState{
			cfg:       cfg.VCenter,
			client:    client,
			placement: placement,
			vmCreated: true,
		}
		cleanupInterruptedVCenterBuild(ctx, state)

		assertTargetVMNotFound(t, ctx, client, cfg.VCenter, placement)
		if state.vmCreated {
			t.Fatalf("cleanup state vmCreated = true, want false")
		}
	})
}

func TestVMConfigUsesRequestedHardware(t *testing.T) {
	runVPXSimulator(t, func(ctx context.Context, client *vim25.Client) {
		cfg := simulatorVCenterConfig("config-template")
		cfg.Hardware.VCenter.ReserveAllGuestMemory = true

		placement, err := ResolvePlacement(ctx, client, cfg.VCenter)
		if err != nil {
			t.Fatalf("ResolvePlacement returned error: %v", err)
		}

		datastoreISOPath := placement.Datastore.Path("installer.iso")
		datastoreConsoleLogPath := placement.Datastore.Path("installer-console.log")
		spec, err := BuildVMConfig(ctx, cfg, placement, datastoreISOPath, datastoreConsoleLogPath, 20*1024*1024*1024)
		if err != nil {
			t.Fatalf("BuildVMConfig returned error: %v", err)
		}

		if spec.Firmware != string(types.GuestOsDescriptorFirmwareTypeEfi) {
			t.Fatalf("Firmware = %q, want efi", spec.Firmware)
		}
		if spec.NumCPUs != 4 || spec.MemoryMB != 8192 {
			t.Fatalf("CPU/memory = %d/%d, want 4/8192", spec.NumCPUs, spec.MemoryMB)
		}
		if spec.MemoryReservationLockedToMax == nil || !*spec.MemoryReservationLockedToMax {
			t.Fatalf("MemoryReservationLockedToMax = %#v, want true", spec.MemoryReservationLockedToMax)
		}
		if spec.MemoryAllocation == nil || spec.MemoryAllocation.Reservation == nil || *spec.MemoryAllocation.Reservation != 8192 {
			t.Fatalf("MemoryAllocation = %#v, want reservation 8192", spec.MemoryAllocation)
		}
		if spec.Files == nil || spec.Files.VmPathName != "[LocalDS_0]" {
			t.Fatalf("VmPathName = %#v, want [LocalDS_0]", spec.Files)
		}

		if _, ok := findDeviceChange[*types.ParaVirtualSCSIController](spec); !ok {
			t.Fatalf("DeviceChange does not contain a PVSCSI controller: %#v", spec.DeviceChange)
		}
		if _, ok := findDeviceChange[*types.VirtualVmxnet3](spec); !ok {
			t.Fatalf("DeviceChange does not contain a VmxNet3 NIC: %#v", spec.DeviceChange)
		}
		if _, ok := findDeviceChange[*types.VirtualSIOController](spec); !ok {
			t.Fatalf("DeviceChange does not contain an SIO controller: %#v", spec.DeviceChange)
		}
		disk, ok := findDeviceChange[*types.VirtualDisk](spec)
		if !ok {
			t.Fatalf("DeviceChange does not contain a disk: %#v", spec.DeviceChange)
		}
		diskBacking, ok := disk.Backing.(*types.VirtualDiskFlatVer2BackingInfo)
		if !ok || diskBacking.Datastore == nil || *diskBacking.Datastore != placement.Datastore.Reference() {
			t.Fatalf("disk backing = %#v, want selected datastore ref %s", disk.Backing, placement.Datastore.Reference())
		}
		if diskBacking.ThinProvisioned == nil || *diskBacking.ThinProvisioned {
			t.Fatalf("disk ThinProvisioned = %#v, want false for thick provision lazy zeroed", diskBacking.ThinProvisioned)
		}
		if diskBacking.EagerlyScrub == nil || *diskBacking.EagerlyScrub {
			t.Fatalf("disk EagerlyScrub = %#v, want false for thick provision lazy zeroed", diskBacking.EagerlyScrub)
		}
		cdrom, ok := findDeviceChange[*types.VirtualCdrom](spec)
		if !ok {
			t.Fatalf("DeviceChange does not contain a CDROM: %#v", spec.DeviceChange)
		}
		isoBacking, ok := cdrom.Backing.(*types.VirtualCdromIsoBackingInfo)
		if !ok || isoBacking.FileName != datastoreISOPath {
			t.Fatalf("CDROM backing = %#v, want ISO %q", cdrom.Backing, datastoreISOPath)
		}
		nic, ok := findDeviceChange[*types.VirtualVmxnet3](spec)
		if !ok {
			t.Fatal("missing VmxNet3 NIC")
		}
		networkBacking, ok := nic.Backing.(*types.VirtualEthernetCardNetworkBackingInfo)
		if !ok || networkBacking.DeviceName != "VM Network" {
			t.Fatalf("NIC backing = %#v, want VM Network", nic.Backing)
		}
		serial, ok := findDeviceChange[*types.VirtualSerialPort](spec)
		if !ok {
			t.Fatal("missing serial console port")
		}
		serialBacking, ok := serial.Backing.(*types.VirtualSerialPortFileBackingInfo)
		if !ok || serialBacking.FileName != datastoreConsoleLogPath {
			t.Fatalf("serial backing = %#v, want file %q", serial.Backing, datastoreConsoleLogPath)
		}

		folders, err := placement.Datacenter.Folders(ctx)
		if err != nil {
			t.Fatalf("read datacenter folders: %v", err)
		}
		if placement.Folder.Reference() != folders.VmFolder.Reference() {
			t.Fatalf("folder ref = %s, want VM folder %s", placement.Folder.Reference(), folders.VmFolder.Reference())
		}
	})
}

func TestBuildPostInstallDeviceSpecDisconnectsInstallerDevicesAndBootsDiskOnly(t *testing.T) {
	runVPXSimulator(t, func(ctx context.Context, client *vim25.Client) {
		cfg := simulatorVCenterConfig("post-install-spec")
		placement, err := ResolvePlacement(ctx, client, cfg.VCenter)
		if err != nil {
			t.Fatalf("ResolvePlacement returned error: %v", err)
		}
		installSpec, err := BuildVMConfig(ctx, cfg, placement, placement.Datastore.Path("installer.iso"), placement.Datastore.Path("installer-console.log"), 20*1024*1024*1024)
		if err != nil {
			t.Fatalf("BuildVMConfig returned error: %v", err)
		}
		devices := devicesFromConfigSpec(t, installSpec)
		disk := firstDiskDevice(devices)
		if disk == nil {
			t.Fatal("install spec missing disk")
		}

		finalSpec, err := buildPostInstallDeviceSpec(devices)
		if err != nil {
			t.Fatalf("buildPostInstallDeviceSpec returned error: %v", err)
		}

		assertDiskOnlyBootOrder(t, finalSpec.BootOptions, disk.Key)
		cdrom, ok := findDeviceChange[*types.VirtualCdrom](finalSpec)
		if !ok {
			t.Fatalf("post-install spec missing CDROM edit: %#v", finalSpec.DeviceChange)
		}
		if _, ok := cdrom.Backing.(*types.VirtualCdromIsoBackingInfo); ok {
			t.Fatalf("CDROM still has ISO backing: %#v", cdrom.Backing)
		}
		assertDeviceDisconnected(t, cdrom.GetVirtualDevice(), "CDROM")

		serial, ok := findDeviceChange[*types.VirtualSerialPort](finalSpec)
		if !ok {
			t.Fatalf("post-install spec missing serial edit: %#v", finalSpec.DeviceChange)
		}
		assertDeviceDisconnected(t, serial.GetVirtualDevice(), "serial")
	})
}

func TestFinalizePostInstallDevicesInSimulator(t *testing.T) {
	runVPXSimulator(t, func(ctx context.Context, client *vim25.Client) {
		cfg := simulatorVCenterConfig("post-install-finalize")
		placement, err := ResolvePlacement(ctx, client, cfg.VCenter)
		if err != nil {
			t.Fatalf("ResolvePlacement returned error: %v", err)
		}
		vm, err := createSimulatorVM(ctx, client, cfg, placement)
		if err != nil {
			t.Fatalf("create simulator VM: %v", err)
		}

		if err := finalizePostInstallDevices(ctx, vm); err != nil {
			t.Fatalf("finalizePostInstallDevices returned error: %v", err)
		}

		devices, err := vm.Device(ctx)
		if err != nil {
			t.Fatalf("read VM devices after finalization: %v", err)
		}
		disk := firstDiskDevice(devices)
		if disk == nil {
			t.Fatal("finalized VM missing disk")
		}
		for _, device := range devices.SelectByType((*types.VirtualCdrom)(nil)) {
			cdrom := device.(*types.VirtualCdrom)
			if _, ok := cdrom.Backing.(*types.VirtualCdromIsoBackingInfo); ok {
				t.Fatalf("finalized CDROM still has ISO backing: %#v", cdrom.Backing)
			}
			assertDeviceDisconnected(t, cdrom.GetVirtualDevice(), "CDROM")
		}
		for _, device := range devices.SelectByType((*types.VirtualSerialPort)(nil)) {
			serial := device.(*types.VirtualSerialPort)
			assertDeviceDisconnected(t, serial.GetVirtualDevice(), "serial")
		}

		var vmMO mo.VirtualMachine
		if err := vm.Properties(ctx, vm.Reference(), []string{"config.bootOptions"}, &vmMO); err != nil {
			t.Fatalf("read VM boot options: %v", err)
		}
		if vmMO.Config == nil {
			t.Fatal("VM config was nil after reading boot options")
		}
		assertDiskOnlyBootOrder(t, vmMO.Config.BootOptions, disk.Key)
	})
}

func TestVMConfigSupportsVCenterDiskProvisioningTypes(t *testing.T) {
	runVPXSimulator(t, func(ctx context.Context, client *vim25.Client) {
		placement, err := ResolvePlacement(ctx, client, simulatorVCenterConfig("placement").VCenter)
		if err != nil {
			t.Fatalf("ResolvePlacement returned error: %v", err)
		}

		tests := []struct {
			name                string
			provisioning        string
			wantThinProvisioned bool
			wantEagerlyScrub    bool
		}{
			{name: "thin", provisioning: common.VCenterDiskProvisioningThin, wantThinProvisioned: true, wantEagerlyScrub: false},
			{name: "thick lazy", provisioning: common.VCenterDiskProvisioningThickLazyZeroed, wantThinProvisioned: false, wantEagerlyScrub: false},
			{name: "thick eager", provisioning: common.VCenterDiskProvisioningThickEagerZeroed, wantThinProvisioned: false, wantEagerlyScrub: true},
		}
		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				cfg := simulatorVCenterConfig("disk-" + common.SafeName(test.name))
				cfg.Hardware.VCenter.DiskProvisioning = test.provisioning
				spec, err := BuildVMConfig(ctx, cfg, placement, placement.Datastore.Path("installer.iso"), placement.Datastore.Path("installer-console.log"), 20*1024*1024*1024)
				if err != nil {
					t.Fatalf("BuildVMConfig returned error: %v", err)
				}
				disk, ok := findDeviceChange[*types.VirtualDisk](spec)
				if !ok {
					t.Fatalf("DeviceChange does not contain a disk: %#v", spec.DeviceChange)
				}
				backing, ok := disk.Backing.(*types.VirtualDiskFlatVer2BackingInfo)
				if !ok {
					t.Fatalf("disk backing = %#v, want flat v2 backing", disk.Backing)
				}
				if backing.ThinProvisioned == nil || *backing.ThinProvisioned != test.wantThinProvisioned {
					t.Fatalf("ThinProvisioned = %#v, want %v", backing.ThinProvisioned, test.wantThinProvisioned)
				}
				if backing.EagerlyScrub == nil || *backing.EagerlyScrub != test.wantEagerlyScrub {
					t.Fatalf("EagerlyScrub = %#v, want %v", backing.EagerlyScrub, test.wantEagerlyScrub)
				}
			})
		}
	})
}

func TestResolvePlacementRejectsInvalidPlacementInputs(t *testing.T) {
	runVPXSimulator(t, func(ctx context.Context, client *vim25.Client) {
		tests := []struct {
			name    string
			mutate  func(*ConnectionConfig)
			wantErr string
		}{
			{
				name: "datacenter",
				mutate: func(cfg *ConnectionConfig) {
					cfg.Datacenter = "missing-datacenter"
				},
				wantErr: `find datacenter "missing-datacenter"`,
			},
			{
				name: "esxi host",
				mutate: func(cfg *ConnectionConfig) {
					cfg.ESXiHost = "missing-esxi-host"
				},
				wantErr: `find ESXi host "missing-esxi-host"`,
			},
			{
				name: "datastore",
				mutate: func(cfg *ConnectionConfig) {
					cfg.Datastore = "missing-datastore"
				},
				wantErr: `find datastore "missing-datastore"`,
			},
			{
				name: "folder",
				mutate: func(cfg *ConnectionConfig) {
					cfg.Folder = "missing-folder"
				},
				wantErr: `find VM folder "missing-folder"`,
			},
			{
				name: "network",
				mutate: func(cfg *ConnectionConfig) {
					cfg.Network = "missing-network"
				},
				wantErr: `find network "missing-network"`,
			},
		}

		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				cfg := simulatorVCenterConfig("invalid-" + common.SafeName(test.name)).VCenter
				test.mutate(&cfg)

				_, err := ResolvePlacement(ctx, client, cfg)
				if err == nil {
					t.Fatal("ResolvePlacement returned nil error")
				}
				if !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("ResolvePlacement error = %q, want to contain %q", err.Error(), test.wantErr)
				}
			})
		}
	})
}

func TestSimulatorResolvesPlacementAndCreatesVM(t *testing.T) {
	runVPXSimulator(t, func(ctx context.Context, client *vim25.Client) {
		cfg := simulatorVCenterConfig("sim-template")
		placement, err := ResolvePlacement(ctx, client, cfg.VCenter)
		if err != nil {
			t.Fatalf("ResolvePlacement returned error: %v", err)
		}
		spec, err := BuildVMConfig(ctx, cfg, placement, placement.Datastore.Path("installer.iso"), placement.Datastore.Path("installer-console.log"), 20*1024*1024*1024)
		if err != nil {
			t.Fatalf("BuildVMConfig returned error: %v", err)
		}

		task, err := placement.Folder.CreateVM(ctx, spec, placement.ResourcePool, placement.Host)
		if err != nil {
			t.Fatalf("CreateVM returned error: %v", err)
		}
		info, err := task.WaitForResult(ctx)
		if err != nil {
			t.Fatalf("wait for CreateVM: %v", err)
		}
		vm := object.NewVirtualMachine(client, info.Result.(types.ManagedObjectReference))
		name, err := vm.ObjectName(ctx)
		if err != nil {
			t.Fatalf("read VM name: %v", err)
		}
		if name != cfg.VCenter.Name {
			t.Fatalf("created VM name = %q, want %q", name, cfg.VCenter.Name)
		}
	})
}

func runVPXSimulator(t *testing.T, f func(context.Context, *vim25.Client)) {
	t.Helper()
	runVPXSimulatorService(t, func(ctx context.Context, serverURL *url.URL) {
		client, err := govmomi.NewClient(ctx, serverURL, true)
		if err != nil {
			t.Fatalf("connect to simulator: %v", err)
		}
		defer client.Logout(ctx)

		f(ctx, client.Client)
	})
}

func runVPXSimulatorService(t *testing.T, f func(context.Context, *url.URL)) {
	t.Helper()
	model := simulator.VPX()
	defer model.Remove()
	if err := model.Create(); err != nil {
		t.Fatalf("create simulator model: %v", err)
	}
	server := model.Service.NewServer()
	defer server.Close()

	ctx := context.Background()
	f(ctx, server.URL)
}

func simulatorVCenterConfig(name string) Config {
	hardware := common.DefaultHardwareConfig()
	hardware.BootFirmware = common.BootFirmwareUEFI
	hardware.DiskSize = "20G"
	hardware.VCPU = 4
	hardware.MemoryMB = 8192
	hardware.VCenter.SCSIController = "pvscsi"
	hardware.VCenter.NetworkAdapter = "vmxnet3"

	return Config{
		DiskSize:    "20G",
		DisplayName: name,
		Hardware:    hardware,
		VCenter: ConnectionConfig{
			Datacenter: "DC0",
			ESXiHost:   "DC0_C0_H0",
			Datastore:  "LocalDS_0",
			Folder:     "vm",
			Network:    "VM Network",
			Name:       name,
		},
	}
}

func createSimulatorVM(ctx context.Context, client *vim25.Client, cfg Config, placement *placement) (*object.VirtualMachine, error) {
	spec, err := BuildVMConfig(ctx, cfg, placement, placement.Datastore.Path("installer.iso"), placement.Datastore.Path("installer-console.log"), 20*1024*1024*1024)
	if err != nil {
		return nil, err
	}
	task, err := placement.Folder.CreateVM(ctx, spec, placement.ResourcePool, placement.Host)
	if err != nil {
		return nil, err
	}
	info, err := task.WaitForResult(ctx)
	if err != nil {
		return nil, err
	}
	vmRef, ok := info.Result.(types.ManagedObjectReference)
	if !ok {
		return nil, fmt.Errorf("CreateVM returned unexpected result %T", info.Result)
	}
	return object.NewVirtualMachine(client, vmRef), nil
}

func assertTargetVMNotFound(t *testing.T, ctx context.Context, client *vim25.Client, cfg ConnectionConfig, placement *placement) {
	t.Helper()
	targetPath, err := targetInventoryPath(ctx, client, cfg, placement.Folder)
	if err != nil {
		t.Fatalf("resolve target inventory path: %v", err)
	}
	found, err := object.NewSearchIndex(client).FindByInventoryPath(ctx, targetPath)
	if err != nil {
		t.Fatalf("search target inventory path %q: %v", targetPath, err)
	}
	if found != nil {
		t.Fatalf("target VM still exists at %q as %s", targetPath, refString(found.Reference()))
	}
}

func assertDatastoreFileMissing(t *testing.T, ctx context.Context, datastore *object.Datastore, remotePath string) {
	t.Helper()
	reader, _, err := datastore.Download(ctx, remotePath, nil)
	if err == nil {
		_ = reader.Close()
		t.Fatalf("datastore file %q still exists", remotePath)
	}
	if !isMissingDatastoreFile(err) {
		t.Fatalf("datastore file %q download error = %v, want missing file error", remotePath, err)
	}
}

func devicesFromConfigSpec(t *testing.T, spec types.VirtualMachineConfigSpec) object.VirtualDeviceList {
	t.Helper()
	var devices object.VirtualDeviceList
	for _, change := range spec.DeviceChange {
		device := change.GetVirtualDeviceConfigSpec().Device
		if device == nil {
			t.Fatalf("device change has nil device: %#v", change)
		}
		devices = append(devices, device)
	}
	return devices
}

func assertDeviceDisconnected(t *testing.T, device *types.VirtualDevice, label string) {
	t.Helper()
	if device.Connectable == nil {
		t.Fatalf("%s connectable is nil, want disconnected flags", label)
	}
	if device.Connectable.Connected {
		t.Fatalf("%s Connected = true, want false", label)
	}
	if device.Connectable.StartConnected {
		t.Fatalf("%s StartConnected = true, want false", label)
	}
}

func assertDiskOnlyBootOrder(t *testing.T, bootOptions *types.VirtualMachineBootOptions, diskKey int32) {
	t.Helper()
	if bootOptions == nil {
		t.Fatal("BootOptions = nil")
	}
	if len(bootOptions.BootOrder) != 1 {
		t.Fatalf("BootOrder length = %d, want 1: %#v", len(bootOptions.BootOrder), bootOptions.BootOrder)
	}
	bootDisk, ok := bootOptions.BootOrder[0].(*types.VirtualMachineBootOptionsBootableDiskDevice)
	if !ok {
		t.Fatalf("BootOrder[0] = %T, want disk boot device", bootOptions.BootOrder[0])
	}
	if bootDisk.DeviceKey != diskKey {
		t.Fatalf("BootOrder disk key = %d, want %d", bootDisk.DeviceKey, diskKey)
	}
}

type fakeTemplateMarker struct {
	calls int
	err   error
}

func (f *fakeTemplateMarker) MarkAsTemplate(context.Context) error {
	f.calls++
	return f.err
}

func findDeviceChange[T types.BaseVirtualDevice](spec types.VirtualMachineConfigSpec) (T, bool) {
	var zero T
	for _, change := range spec.DeviceChange {
		device := change.GetVirtualDeviceConfigSpec().Device
		if typed, ok := device.(T); ok {
			return typed, true
		}
	}
	return zero, false
}
