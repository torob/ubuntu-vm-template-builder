package proxmox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"ubuntu-vm-template-builder/internal/common"
	"ubuntu-vm-template-builder/internal/offlineapt"
	"ubuntu-vm-template-builder/internal/qemulog"
	"ubuntu-vm-template-builder/internal/seediso"
)

const interruptCleanupTimeout = 2 * time.Minute

const (
	proxmoxEFIType2MBytes = 2 * 1024 * 1024
	proxmoxEFIType4MBytes = 4 * 1024 * 1024
	proxmoxGiB            = 1024 * 1024 * 1024
)

type Config struct {
	UbuntuISO        string
	UserDataPath     string
	UserData         []byte
	DiskSize         string
	DisplayName      string
	Hardware         common.HardwareConfig
	ExtraPackages    offlineapt.Config
	Options          OptionsConfig
	CloudInitOptions CloudInitOptionsConfig
	Proxmox          ConnectionConfig
}

type ConnectionConfig struct {
	Host        string
	TokenID     string
	TokenSecret string
	Insecure    bool
	Node        string
	ISOStorage  string
	DiskStorage string
	VMID        int
	Bridge      string
	Name        string
}

type Installer struct {
	cfg     Config
	tempDir string
}

type buildState struct {
	cfg              ConnectionConfig
	client           api
	vmid             int
	vmCreated        bool
	tempISOContent   string
	tempISOFileName  string
	tempISOVolumeID  string
	tempISOStorage   string
	tempISODeleted   bool
	completed        bool
	outputWasStarted bool
}

func NewInstaller(cfg Config) (*Installer, error) {
	cfg.Hardware = cfg.Hardware.Normalize()
	if err := cfg.Hardware.Validate(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(cfg.DisplayName) == "" {
		cfg.DisplayName = common.FallbackName
	}
	cfg.ExtraPackages = cfg.ExtraPackages.Normalize()
	cfg.Proxmox = normalizeConnectionConfig(cfg.Proxmox, cfg.DisplayName)
	return &Installer{cfg: cfg}, nil
}

func normalizeConnectionConfig(cfg ConnectionConfig, fallback string) ConnectionConfig {
	if strings.TrimSpace(cfg.Name) == "" {
		cfg.Name = fallback
	}
	if strings.TrimSpace(cfg.Name) == "" {
		cfg.Name = common.FallbackName
	}
	cfg.Bridge = strings.TrimSpace(cfg.Bridge)
	return cfg
}

func CheckPrerequisites(cfg Config) error {
	cfg.Hardware = cfg.Hardware.Normalize()
	if err := cfg.Hardware.Validate(); err != nil {
		return err
	}
	if _, err := exec.LookPath("xorriso"); err != nil {
		return errors.New("missing required dependency for proxmox backend: xorriso")
	}
	if err := common.CheckISOFile(cfg.UbuntuISO); err != nil {
		return err
	}
	if _, err := common.ParseDiskSize(cfg.DiskSize); err != nil {
		return err
	}
	extraPackages := cfg.ExtraPackages.Normalize()
	if err := offlineapt.CheckPrerequisites(extraPackages); err != nil {
		return err
	}
	if _, err := seediso.TransformUserDataWithOptions(cfg.UserData, seediso.Options{}); err != nil {
		return fmt.Errorf("prepare Proxmox seed user-data: %w", err)
	}
	return nil
}

func (i *Installer) Install() bool {
	fmt.Printf("\n%s\n", strings.Repeat("=", 60))
	fmt.Printf("Installing: %s -> Proxmox %s %s\n", i.cfg.DisplayName, outputNoun(i.cfg.Hardware.Proxmox.OutputType), i.cfg.Proxmox.Name)
	fmt.Printf("Boot firmware: %s\n", common.NormalizeBootFirmware(i.cfg.Hardware.BootFirmware))
	fmt.Printf("%s\n", strings.Repeat("=", 60))

	defer i.cleanupLocal()

	ctx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()

	client, err := NewClient(i.cfg.Proxmox)
	if err != nil {
		fmt.Printf("Proxmox installation failed for %s: %v\n", i.cfg.DisplayName, err)
		return false
	}
	err = i.installWithClient(ctx, client, func(ctx context.Context) (string, error) {
		return i.createInstallerISO(ctx)
	})
	if err != nil {
		fmt.Printf("Proxmox installation failed for %s: %v\n", i.cfg.DisplayName, err)
		return false
	}
	return true
}

func (i *Installer) installWithClient(ctx context.Context, client api, createInstallerISO func(context.Context) (string, error)) error {
	cfg := normalizeConnectionConfig(i.cfg.Proxmox, i.cfg.DisplayName)
	state := &buildState{cfg: cfg, client: client}
	defer func() {
		if ctx.Err() == nil || state.completed {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.Background(), interruptCleanupTimeout)
		defer cancel()
		cleanupInterruptedBuild(cleanupCtx, state)
	}()

	if err := validateConnectionFields(cfg); err != nil {
		return err
	}
	fmt.Println("Connecting to Proxmox and validating node...")
	if err := client.ValidateNode(ctx, cfg.Node); err != nil {
		return err
	}
	if err := validateTargetNameAvailable(ctx, client, cfg); err != nil {
		return err
	}
	fmt.Println("OK Proxmox node and target name validated")

	localISOPath, err := createInstallerISO(ctx)
	if err != nil {
		return err
	}
	if err := preflightBuildStorages(ctx, client, cfg, i.cfg, localISOPath); err != nil {
		return err
	}

	tempISOFileName := buildTempISOFileName(cfg.Name)
	tempISOVolumeID := buildVolumeID(cfg.ISOStorage, UploadContentISO, tempISOFileName)
	fmt.Printf("Uploading installer ISO to Proxmox storage: %s\n", tempISOVolumeID)
	uploadedVolumeID, err := uploadTemporaryInstallerISO(ctx, client, cfg, localISOPath, tempISOFileName, tempISOVolumeID)
	if err != nil {
		return err
	}
	state.tempISOContent = UploadContentISO
	state.tempISOFileName = tempISOFileName
	state.tempISOVolumeID = uploadedVolumeID
	state.tempISOStorage = cfg.ISOStorage
	fmt.Printf("OK installer ISO uploaded: %s\n", uploadedVolumeID)

	vmid, err := selectVMID(ctx, client, cfg)
	if err != nil {
		_ = deleteTemporaryISO(ctx, client, state)
		return err
	}
	state.vmid = vmid

	values, err := BuildCreateVMValues(i.cfg, vmid, uploadedVolumeID)
	if err != nil {
		_ = deleteTemporaryISO(ctx, client, state)
		return err
	}
	printCreateVMRequestDetails(i.cfg, vmid, uploadedVolumeID, values)
	upID, err := client.CreateVM(ctx, cfg.Node, values)
	if err != nil {
		_ = deleteTemporaryISO(ctx, client, state)
		return fmt.Errorf("create VM: %w", err)
	}
	if err := client.WaitTask(ctx, cfg.Node, upID); err != nil {
		return fmt.Errorf("wait for VM creation: %w", err)
	}
	state.vmCreated = true
	fmt.Printf("OK VM created: %s (%d)\n", cfg.Name, vmid)

	if err := powerOnAndWaitForInstaller(ctx, client, cfg.Node, vmid, os.Stdout); err != nil {
		return err
	}
	state.outputWasStarted = true

	if err := finalizePostInstallConfig(ctx, client, i.cfg, cfg.Node, vmid); err != nil {
		return err
	}
	if err := applyCloudInitOptions(ctx, client, i.cfg, cfg.Node, vmid); err != nil {
		return err
	}
	if err := applyVMOptions(ctx, client, i.cfg, cfg.Node, vmid); err != nil {
		return err
	}
	if err := maybeConvertToTemplate(ctx, client, cfg.Node, vmid, i.cfg.Hardware.Proxmox.OutputType); err != nil {
		return err
	}
	if err := deleteTemporaryISO(ctx, client, state); err != nil {
		return err
	}
	state.completed = true

	fmt.Printf("\nOK Proxmox %s created successfully for %s\n", outputNoun(i.cfg.Hardware.Proxmox.OutputType), i.cfg.DisplayName)
	fmt.Printf("  %s: %s\n", outputField(i.cfg.Hardware.Proxmox.OutputType), cfg.Name)
	fmt.Printf("  VMID: %d\n", vmid)
	fmt.Printf("  Node: %s\n", cfg.Node)
	fmt.Printf("  ISO storage: %s\n", cfg.ISOStorage)
	fmt.Printf("  Disk storage: %s\n", cfg.DiskStorage)
	fmt.Printf("  Bridge: %s\n", cfg.Bridge)
	fmt.Printf("  Boot firmware: %s\n", common.NormalizeBootFirmware(i.cfg.Hardware.BootFirmware))
	return nil
}

func validateConnectionFields(cfg ConnectionConfig) error {
	missing := map[string]string{
		"--proxmox-host":         cfg.Host,
		"--proxmox-disk-storage": cfg.DiskStorage,
		"--proxmox-iso-storage":  cfg.ISOStorage,
		"--proxmox-token-id":     cfg.TokenID,
		"--proxmox-token-secret": cfg.TokenSecret,
		"--proxmox-node":         cfg.Node,
	}
	var names []string
	for name, value := range missing {
		if strings.TrimSpace(value) == "" {
			names = append(names, name)
		}
	}
	if len(names) > 0 {
		sort.Strings(names)
		return fmt.Errorf("missing required flags: %s", strings.Join(names, ", "))
	}
	if cfg.VMID < 0 {
		return errors.New("--proxmox-vmid must be greater than zero when set")
	}
	return nil
}

func validateTargetNameAvailable(ctx context.Context, client api, cfg ConnectionConfig) error {
	vms, err := client.ListVMs(ctx, cfg.Node)
	if err != nil {
		return fmt.Errorf("list Proxmox VMs on node %q: %w", cfg.Node, err)
	}
	for _, vm := range vms {
		if cfg.VMID > 0 && vm.VMID == cfg.VMID {
			return fmt.Errorf("Proxmox VMID %d already exists on node %q", cfg.VMID, cfg.Node)
		}
		if strings.EqualFold(strings.TrimSpace(vm.Name), strings.TrimSpace(cfg.Name)) {
			return fmt.Errorf("Proxmox VM/template name %q already exists on node %q as VMID %d", cfg.Name, cfg.Node, vm.VMID)
		}
	}
	return nil
}

type storagePreflightRequirement struct {
	Storage       string
	Roles         []string
	RequiredBytes int64
	ContentTypes  []string
}

func preflightBuildStorages(ctx context.Context, client api, cfg ConnectionConfig, buildCfg Config, localISOPath string) error {
	isoInfo, err := os.Stat(localISOPath)
	if err != nil {
		return fmt.Errorf("stat remastered installer ISO %q: %w", localISOPath, err)
	}
	if !isoInfo.Mode().IsRegular() {
		return fmt.Errorf("remastered installer ISO %q is not a regular file", localISOPath)
	}

	diskSize := strings.TrimSpace(buildCfg.DiskSize)
	if diskSize == "" {
		diskSize = buildCfg.Hardware.DiskSize
	}
	diskBytes, err := proxmoxDiskAllocationBytes(diskSize)
	if err != nil {
		return err
	}
	diskBytes += proxmoxEFIDiskBytes(buildCfg.Hardware)

	requirements := map[string]*storagePreflightRequirement{}
	addStorageRequirement(requirements, cfg.ISOStorage, "ISO", UploadContentISO, isoInfo.Size())
	addStorageRequirement(requirements, cfg.DiskStorage, "disk", "images", diskBytes)
	if buildCfg.CloudInitOptions.Enabled {
		addStorageRequirement(requirements, cfg.DiskStorage, "cloud-init", "images", cloudInitDiskBytes)
	}

	var storages []string
	for storage := range requirements {
		storages = append(storages, storage)
	}
	sort.Strings(storages)
	for _, storage := range storages {
		requirement := requirements[storage]
		status, err := client.StorageStatus(ctx, cfg.Node, requirement.Storage)
		if err != nil {
			return fmt.Errorf("validate Proxmox %s storage %q on node %q: %w", strings.Join(requirement.Roles, "+"), requirement.Storage, cfg.Node, err)
		}
		if err := validateStorageStatus(requirement, status); err != nil {
			return err
		}
	}
	return nil
}

func addStorageRequirement(requirements map[string]*storagePreflightRequirement, storage, role, contentType string, requiredBytes int64) {
	req := requirements[storage]
	if req == nil {
		req = &storagePreflightRequirement{Storage: storage}
		requirements[storage] = req
	}
	req.Roles = append(req.Roles, role)
	req.ContentTypes = append(req.ContentTypes, contentType)
	req.RequiredBytes += requiredBytes
}

func validateStorageStatus(req *storagePreflightRequirement, status StorageStatus) error {
	role := strings.Join(req.Roles, "+")
	if isJSONFalse(status.Active) {
		return fmt.Errorf("Proxmox %s storage %q is not active", role, req.Storage)
	}
	if isJSONFalse(status.Enabled) {
		return fmt.Errorf("Proxmox %s storage %q is not enabled", role, req.Storage)
	}
	if strings.TrimSpace(status.Content) == "" {
		return fmt.Errorf("Proxmox %s storage %q content list is unavailable; required content: %s", role, req.Storage, strings.Join(req.ContentTypes, ", "))
	}
	for _, contentType := range req.ContentTypes {
		if !storageContentAllows(status.Content, contentType) {
			return fmt.Errorf("Proxmox %s storage %q must allow %q content, got %q", role, req.Storage, contentType, status.Content)
		}
	}
	if !status.Avail.Set {
		return fmt.Errorf("Proxmox %s storage %q capacity is unavailable; required bytes: %d", role, req.Storage, req.RequiredBytes)
	}
	if status.Avail.Value < req.RequiredBytes {
		return fmt.Errorf("Proxmox %s storage %q has insufficient capacity: required bytes %d, available bytes %d", role, req.Storage, req.RequiredBytes, status.Avail.Value)
	}
	return nil
}

func proxmoxEFIDiskBytes(hardware common.HardwareConfig) int64 {
	hardware = hardware.Normalize()
	if common.NormalizeBootFirmware(hardware.BootFirmware) != common.BootFirmwareUEFI {
		return 0
	}
	if hardware.Proxmox.EFIType == "2m" {
		return proxmoxEFIType2MBytes
	}
	return proxmoxEFIType4MBytes
}

func uploadTemporaryInstallerISO(ctx context.Context, client api, cfg ConnectionConfig, localISOPath, fileName, volumeID string) (string, error) {
	exists, err := client.StorageVolumeExists(ctx, cfg.Node, cfg.ISOStorage, UploadContentISO, volumeID)
	if err != nil {
		return "", fmt.Errorf("check temporary installer ISO %q: %w", volumeID, err)
	}
	if exists {
		return "", fmt.Errorf("temporary installer ISO destination already exists: %s", volumeID)
	}
	gotVolumeID, err := client.UploadFile(ctx, cfg.Node, cfg.ISOStorage, StorageUpload{
		SourcePath: localISOPath,
		FileName:   fileName,
		Content:    UploadContentISO,
	})
	if err != nil {
		return "", fmt.Errorf("upload installer ISO to Proxmox storage: %w", err)
	}
	if strings.TrimSpace(gotVolumeID) == "" {
		gotVolumeID = volumeID
	}
	return gotVolumeID, nil
}

func selectVMID(ctx context.Context, client api, cfg ConnectionConfig) (int, error) {
	if cfg.VMID > 0 {
		fmt.Printf("Using requested Proxmox VMID: %d\n", cfg.VMID)
		return cfg.VMID, nil
	}
	vmid, err := client.NextID(ctx)
	if err != nil {
		return 0, fmt.Errorf("allocate Proxmox VMID: %w", err)
	}
	if vmid <= 0 {
		return 0, fmt.Errorf("Proxmox returned invalid VMID %d", vmid)
	}
	fmt.Printf("Allocated Proxmox VMID: %d\n", vmid)
	return vmid, nil
}

func BuildCreateVMValues(cfg Config, vmid int, installerVolumeID string) (url.Values, error) {
	hardware := cfg.Hardware.Normalize()
	if err := hardware.Validate(); err != nil {
		return nil, err
	}
	connection := normalizeConnectionConfig(cfg.Proxmox, cfg.DisplayName)
	if strings.TrimSpace(connection.Bridge) == "" {
		return nil, errors.New("proxmox bridge is required")
	}
	diskSize := strings.TrimSpace(cfg.DiskSize)
	if diskSize == "" {
		diskSize = hardware.DiskSize
	}
	if _, err := common.ParseDiskSize(diskSize); err != nil {
		return nil, err
	}
	diskAllocationGiB, err := proxmoxDiskAllocationGiB(diskSize)
	if err != nil {
		return nil, err
	}

	diskKey := diskDeviceKey(hardware.Proxmox.DiskInterface)
	values := url.Values{}
	values.Set("vmid", strconv.Itoa(vmid))
	values.Set("name", connection.Name)
	values.Set("cores", strconv.Itoa(hardware.VCPU))
	values.Set("memory", strconv.Itoa(hardware.MemoryMB))
	values.Set("ostype", hardware.Proxmox.OSType)
	values.Set("machine", hardware.Proxmox.Machine)
	values.Set("cpu", hardware.Proxmox.CPUType)
	values.Set("scsihw", hardware.Proxmox.SCSIController)
	values.Set("agent", "1")
	values.Set("onboot", "0")
	values.Set("net0", fmt.Sprintf("%s,bridge=%s", hardware.Proxmox.NetworkAdapter, connection.Bridge))
	values.Set("ide2", fmt.Sprintf("%s,media=cdrom", installerVolumeID))
	values.Set(diskKey, proxmoxDiskSpec(connection.DiskStorage, diskAllocationGiB, hardware.Proxmox))
	values.Set("boot", fmt.Sprintf("order=ide2;%s", diskKey))
	values.Set("serial0", "socket")
	values.Set("vga", "serial0")
	if common.NormalizeBootFirmware(hardware.BootFirmware) == common.BootFirmwareUEFI {
		values.Set("bios", "ovmf")
		values.Set("efidisk0", fmt.Sprintf("%s:1,format=%s,efitype=%s,pre-enrolled-keys=%d", connection.DiskStorage, hardware.Proxmox.DiskFormat, hardware.Proxmox.EFIType, boolInt(hardware.Proxmox.PreEnrolledKeys)))
	} else {
		values.Set("bios", "seabios")
	}
	return values, nil
}

func proxmoxDiskSpec(storage string, allocationGiB int64, hardware common.ProxmoxHardwareConfig) string {
	parts := []string{
		fmt.Sprintf("%s:%d", storage, allocationGiB),
		"format=" + hardware.DiskFormat,
	}
	if hardware.DiskIOThread {
		parts = append(parts, "iothread=1")
	}
	return strings.Join(parts, ",")
}

func BuildFinalizeVMValues(cfg Config) (url.Values, error) {
	hardware := cfg.Hardware.Normalize()
	if err := hardware.Validate(); err != nil {
		return nil, err
	}
	values := url.Values{}
	values.Set("boot", fmt.Sprintf("order=%s", diskDeviceKey(hardware.Proxmox.DiskInterface)))
	values.Set("delete", "ide2,serial0")
	values.Set("vga", "std")
	values.Set("cores", strconv.Itoa(hardware.VCPU))
	values.Set("memory", strconv.Itoa(hardware.MemoryMB))
	return values, nil
}

func printCreateVMRequestDetails(cfg Config, vmid int, installerVolumeID string, values url.Values) {
	hardware := cfg.Hardware.Normalize()
	fmt.Println("Proxmox VM create request details:")
	fmt.Printf("  VM name: %s\n", cfg.Proxmox.Name)
	fmt.Printf("  VMID: %d\n", vmid)
	fmt.Printf("  Node: %s\n", cfg.Proxmox.Node)
	fmt.Printf("  ISO storage: %s\n", cfg.Proxmox.ISOStorage)
	fmt.Printf("  Disk storage: %s\n", cfg.Proxmox.DiskStorage)
	fmt.Printf("  Bridge: %s\n", cfg.Proxmox.Bridge)
	fmt.Printf("  Installer ISO: %s\n", installerVolumeID)
	fmt.Printf("  Hardware: firmware=%s vcpu=%d memory_mb=%d disk_size=%s disk=%s format=%s scsi=%s nic=%s cpu=%s machine=%s ostype=%s\n",
		hardware.BootFirmware,
		hardware.VCPU,
		hardware.MemoryMB,
		hardware.DiskSize,
		diskDeviceKey(hardware.Proxmox.DiskInterface),
		hardware.Proxmox.DiskFormat,
		hardware.Proxmox.SCSIController,
		hardware.Proxmox.NetworkAdapter,
		hardware.Proxmox.CPUType,
		hardware.Proxmox.Machine,
		hardware.Proxmox.OSType,
	)
	fmt.Printf("  Proxmox disk spec: %s=%s\n", diskDeviceKey(hardware.Proxmox.DiskInterface), values.Get(diskDeviceKey(hardware.Proxmox.DiskInterface)))
}

func powerOnAndWaitForInstaller(ctx context.Context, client api, node string, vmid int, out io.Writer) error {
	fmt.Println("Powering on VM and waiting for the installer to power it off...")
	upID, err := client.StartVM(ctx, node, vmid)
	if err != nil {
		return fmt.Errorf("power on VM: %w", err)
	}
	if err := client.WaitTask(ctx, node, upID); err != nil {
		return fmt.Errorf("wait for power on: %w", err)
	}

	streamCtx, cancelStream := context.WithCancel(ctx)
	streamDone := make(chan struct{})
	go func() {
		defer close(streamDone)
		fmt.Printf("Streaming Proxmox installer serial console for VMID %d\n", vmid)
		if err := streamSerialConsoleUntilStopped(streamCtx, client, node, vmid, out); err != nil && streamCtx.Err() == nil {
			fmt.Printf("Warning: could not stream Proxmox serial console for VMID %d: %v\n", vmid, err)
		}
	}()
	defer func() {
		cancelStream()
		<-streamDone
	}()

	for {
		status, err := client.CurrentVMStatus(ctx, node, vmid)
		if err != nil {
			return fmt.Errorf("read VM power status: %w", err)
		}
		if status.Status == "stopped" {
			fmt.Println("OK installer powered off the VM")
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(vmPowerPollInterval):
		}
	}
}

func streamSerialConsoleUntilStopped(ctx context.Context, client api, node string, vmid int, out io.Writer) error {
	if out == nil {
		out = io.Discard
	}
	writer := qemulog.NewCompactingWriter(out)
	defer writer.Flush()

	for {
		err := client.StreamSerialConsole(ctx, node, vmid, writer)
		if err == nil || ctx.Err() != nil {
			return nil
		}
		if !isTransientConsoleStreamError(err) {
			return err
		}
		status, statusErr := client.CurrentVMStatus(ctx, node, vmid)
		if statusErr != nil {
			return fmt.Errorf("read VM power status after Proxmox serial console disconnect: %w", statusErr)
		}
		if status.Status == "stopped" {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(consoleReconnectDelay):
		}
	}
}

func finalizePostInstallConfig(ctx context.Context, client api, cfg Config, node string, vmid int) error {
	values, err := BuildFinalizeVMValues(cfg)
	if err != nil {
		return err
	}
	fmt.Println("Finalizing post-install VM config and boot order...")
	upID, err := client.UpdateVMConfig(ctx, node, vmid, values)
	if err != nil {
		return fmt.Errorf("finalize post-install VM config: %w", err)
	}
	if err := client.WaitTask(ctx, node, upID); err != nil {
		return fmt.Errorf("wait for post-install VM finalization: %w", err)
	}
	fmt.Printf("OK post-install VM config finalized (removed installer ISO and serial console; boot order is %s-only)\n", diskDeviceKey(cfg.Hardware.Normalize().Proxmox.DiskInterface))
	return nil
}

func applyCloudInitOptions(ctx context.Context, client api, cfg Config, node string, vmid int) error {
	if !cfg.CloudInitOptions.Enabled {
		return nil
	}
	values, err := BuildCloudInitValues(cfg)
	if err != nil {
		return err
	}
	if len(values) == 0 {
		return nil
	}
	fmt.Println("Applying Proxmox Cloud-Init options...")
	upID, err := client.UpdateVMConfig(ctx, node, vmid, values)
	if err != nil {
		return fmt.Errorf("apply Proxmox Cloud-Init options: %w", err)
	}
	if err := client.WaitTask(ctx, node, upID); err != nil {
		return fmt.Errorf("wait for Proxmox Cloud-Init options: %w", err)
	}
	fmt.Println("OK Proxmox Cloud-Init options applied")
	return nil
}

func applyVMOptions(ctx context.Context, client api, cfg Config, node string, vmid int) error {
	if !cfg.Options.Enabled {
		return nil
	}
	values, err := BuildOptionsValues(cfg)
	if err != nil {
		return err
	}
	if len(values) == 0 {
		return nil
	}
	fmt.Println("Applying Proxmox VM options...")
	upID, err := client.UpdateVMConfig(ctx, node, vmid, values)
	if err != nil {
		return fmt.Errorf("apply Proxmox VM options: %w", err)
	}
	if err := client.WaitTask(ctx, node, upID); err != nil {
		return fmt.Errorf("wait for Proxmox VM options: %w", err)
	}
	fmt.Println("OK Proxmox VM options applied")
	return nil
}

func maybeConvertToTemplate(ctx context.Context, client api, node string, vmid int, outputType string) error {
	if common.NormalizeProxmoxOutputType(outputType) == common.ProxmoxOutputTypeVM {
		return nil
	}
	fmt.Printf("Converting Proxmox VMID %d to template...\n", vmid)
	upID, err := client.TemplateVM(ctx, node, vmid)
	if err != nil {
		return fmt.Errorf("convert VM to template: %w", err)
	}
	if err := client.WaitTask(ctx, node, upID); err != nil {
		return fmt.Errorf("wait for template conversion: %w", err)
	}
	fmt.Printf("OK Proxmox VMID %d converted to template\n", vmid)
	return nil
}

func deleteTemporaryISO(ctx context.Context, client api, state *buildState) error {
	if state == nil || state.tempISOVolumeID == "" || state.tempISODeleted {
		return nil
	}
	fmt.Printf("Deleting temporary Proxmox installer ISO: %s\n", state.tempISOVolumeID)
	storage := state.tempISOStorage
	if storage == "" {
		storage = state.cfg.ISOStorage
	}
	upID, err := client.DeleteVolume(ctx, state.cfg.Node, storage, state.tempISOVolumeID)
	if err != nil {
		return fmt.Errorf("delete temporary Proxmox installer ISO: %w", err)
	}
	if err := client.WaitTask(ctx, state.cfg.Node, upID); err != nil {
		return fmt.Errorf("wait for temporary Proxmox installer ISO delete: %w", err)
	}
	state.tempISODeleted = true
	fmt.Println("OK temporary Proxmox installer ISO deleted")
	return nil
}

func proxmoxDiskAllocationGiB(diskSize string) (int64, error) {
	bytes, err := common.ParseDiskSize(diskSize)
	if err != nil {
		return 0, err
	}
	if bytes <= 0 {
		return 0, fmt.Errorf("disk_size must be greater than zero")
	}
	gib := bytes / proxmoxGiB
	if bytes%proxmoxGiB != 0 {
		gib++
	}
	if gib < 1 {
		gib = 1
	}
	return gib, nil
}

func proxmoxDiskAllocationBytes(diskSize string) (int64, error) {
	gib, err := proxmoxDiskAllocationGiB(diskSize)
	if err != nil {
		return 0, err
	}
	if gib > math.MaxInt64/proxmoxGiB {
		return 0, fmt.Errorf("disk size overflows int64: %q", diskSize)
	}
	return gib * proxmoxGiB, nil
}

func cleanupInterruptedBuild(ctx context.Context, state *buildState) {
	if state == nil || state.client == nil {
		return
	}
	fmt.Println("Interrupt received; cleaning up Proxmox resources...")
	if state.vmCreated && state.vmid > 0 {
		if err := destroyInterruptedVM(ctx, state.client, state.cfg.Node, state.vmid, state.cfg.Name); err != nil {
			fmt.Printf("Warning: could not delete interrupted Proxmox VM %q (%d): %v\n", state.cfg.Name, state.vmid, err)
		} else {
			state.vmCreated = false
		}
	}
	if state.tempISOVolumeID != "" && !state.tempISODeleted {
		if err := deleteTemporaryISO(ctx, state.client, state); err != nil {
			fmt.Printf("Warning: could not delete interrupted Proxmox installer ISO %q: %v\n", state.tempISOVolumeID, err)
		}
	}
}

func destroyInterruptedVM(ctx context.Context, client api, node string, vmid int, name string) error {
	status, err := client.CurrentVMStatus(ctx, node, vmid)
	if err == nil && status.Status != "" && status.Status != "stopped" {
		fmt.Printf("Stopping interrupted Proxmox VM %q (%d)...\n", name, vmid)
		upID, err := client.StopVM(ctx, node, vmid)
		if err != nil {
			return fmt.Errorf("stop VM: %w", err)
		}
		if err := client.WaitTask(ctx, node, upID); err != nil {
			return fmt.Errorf("wait for VM stop: %w", err)
		}
	} else if err != nil && !isNotFoundError(err) {
		fmt.Printf("Warning: could not read interrupted Proxmox VM %q (%d) power status before cleanup: %v\n", name, vmid, err)
	}

	fmt.Printf("Deleting interrupted Proxmox VM %q (%d)...\n", name, vmid)
	upID, err := client.DeleteVM(ctx, node, vmid)
	if err != nil {
		return fmt.Errorf("delete VM: %w", err)
	}
	if err := client.WaitTask(ctx, node, upID); err != nil {
		return fmt.Errorf("wait for VM delete: %w", err)
	}
	fmt.Printf("OK interrupted Proxmox VM deleted: %s (%d)\n", name, vmid)
	return nil
}

func (i *Installer) createInstallerISO(ctx context.Context) (string, error) {
	if err := i.ensureTempDir(); err != nil {
		return "", err
	}
	var repo offlineapt.Repository
	if i.cfg.ExtraPackages.Enabled() {
		fmt.Println("Preparing offline APT repository for extra packages...")
		var err error
		repo, err = offlineapt.BuildRepository(ctx, i.cfg.ExtraPackages, i.cfg.UbuntuISO, i.tempDir)
		if err != nil {
			return "", fmt.Errorf("prepare offline APT repository: %w", err)
		}
		fmt.Printf("OK offline APT repository prepared with %d requested package(s): %s\n", len(repo.Packages), repo.Path)
	}

	fmt.Println("Creating remastered autoinstall ISO...")
	outputPath := filepath.Join(i.tempDir, fmt.Sprintf("installer-%s.iso", common.SafeName(i.cfg.Proxmox.Name)))
	if err := seediso.RemasterUbuntuISOWithNoCloud(ctx, i.cfg.UbuntuISO, outputPath, i.cfg.UserData, i.cfg.DisplayName, i.tempDir, repo.Path, seediso.Options{ExtraPackages: repo.InstallConfig()}); err != nil {
		return "", err
	}
	if err := offlineapt.ValidateEmbeddedRepository(outputPath, repo); err != nil {
		return "", err
	}
	fmt.Printf("OK remastered installer ISO created: %s\n", outputPath)
	return outputPath, nil
}

func (i *Installer) ensureTempDir() error {
	if i.tempDir != "" {
		return nil
	}
	tmpDir, err := os.MkdirTemp("", "ubuntu-proxmox-installer-")
	if err != nil {
		return fmt.Errorf("create temporary directory: %w", err)
	}
	i.tempDir = tmpDir
	fmt.Printf("Created temporary directory: %s\n", tmpDir)
	return nil
}

func (i *Installer) cleanupLocal() {
	if i.tempDir == "" {
		return
	}
	if err := os.RemoveAll(i.tempDir); err != nil {
		fmt.Printf("Warning: could not clean up temporary directory %s: %v\n", i.tempDir, err)
		return
	}
	fmt.Printf("OK cleaned up temporary directory: %s\n", i.tempDir)
}

func buildVolumeID(storage, content, fileName string) string {
	return fmt.Sprintf("%s:%s/%s", storage, strings.ToLower(strings.TrimSpace(content)), fileName)
}

func buildTempISOFileName(templateName string) string {
	return fmt.Sprintf("ubuntu-vm-template-builder-%s-%s.iso", common.SafeName(templateName), time.Now().UTC().Format("20060102T150405Z"))
}

func diskDeviceKey(diskInterface string) string {
	return common.SafeName(strings.ToLower(strings.TrimSpace(diskInterface))) + "0"
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func outputNoun(outputType string) string {
	if common.NormalizeProxmoxOutputType(outputType) == common.ProxmoxOutputTypeVM {
		return "VM"
	}
	return common.ProxmoxOutputTypeTemplate
}

func outputField(outputType string) string {
	if common.NormalizeProxmoxOutputType(outputType) == common.ProxmoxOutputTypeVM {
		return "VM"
	}
	return "Template"
}

func XorrisoInstallSuggestion(osInfo interface {
	HasID(...string) bool
}) string {
	switch {
	case osInfo.HasID("debian", "ubuntu"):
		return "sudo apt update && sudo apt install -y xorriso"
	case osInfo.HasID("fedora", "rhel", "centos", "rocky", "almalinux"):
		return "sudo dnf install -y xorriso"
	case osInfo.HasID("arch"):
		return "sudo pacman -S xorriso"
	case osInfo.HasID("opensuse", "suse"):
		return "sudo zypper install xorriso"
	default:
		return "Install the package that provides xorriso for this Linux distribution."
	}
}
