package common

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	diskfs "github.com/diskfs/go-diskfs"
	"github.com/diskfs/go-diskfs/filesystem/iso9660"
	"gopkg.in/yaml.v3"
)

const (
	DefaultBootFirmware = "uefi"
	BootFirmwareUEFI    = "uefi"
	BootFirmwareBIOS    = "bios"
	FallbackName        = "ubuntu-vm-template-builder"

	VCenterDiskProvisioningThin             = "thin"
	VCenterDiskProvisioningThickLazyZeroed  = "thick_provision_lazy_zeroed"
	VCenterDiskProvisioningThickEagerZeroed = "thick_provision_eager_zeroed"

	VCenterOutputTypeTemplate = "template"
	VCenterOutputTypeVM       = "vm"
)

type HardwareConfig struct {
	BootFirmware string                `yaml:"boot_firmware"`
	DiskSize     string                `yaml:"disk_size"`
	VCPU         int                   `yaml:"vcpu"`
	MemoryMB     int                   `yaml:"memory_mb"`
	QEMU         QEMUHardwareConfig    `yaml:"qemu"`
	VCenter      VCenterHardwareConfig `yaml:"vcenter"`
}

type QEMUHardwareConfig struct {
	CPUModel      string `yaml:"cpu_model"`
	DiskInterface string `yaml:"disk_interface"`
	ISOInterface  string `yaml:"iso_interface"`
}

type VCenterHardwareConfig struct {
	SCSIController        string `yaml:"scsi_controller"`
	NetworkAdapter        string `yaml:"network_adapter"`
	Network               string `yaml:"network"`
	DiskProvisioning      string `yaml:"disk_provisioning"`
	ReserveAllGuestMemory bool   `yaml:"reserve_all_guest_memory"`
	OutputType            string `yaml:"output_type"`
}

func DefaultHardwareConfig() HardwareConfig {
	return HardwareConfig{
		BootFirmware: DefaultBootFirmware,
		VCPU:         2,
		MemoryMB:     2048,
		QEMU: QEMUHardwareConfig{
			CPUModel:      "host",
			DiskInterface: "virtio",
			ISOInterface:  "virtio",
		},
		VCenter: VCenterHardwareConfig{
			SCSIController:   "pvscsi",
			NetworkAdapter:   "vmxnet3",
			DiskProvisioning: VCenterDiskProvisioningThickLazyZeroed,
			OutputType:       VCenterOutputTypeTemplate,
		},
	}
}

func LoadHardwareConfig(path string) (HardwareConfig, error) {
	cfg := DefaultHardwareConfig()
	if strings.TrimSpace(path) == "" {
		return cfg, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("read hardware config %q: %w", path, err)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return cfg, fmt.Errorf("hardware config %q is empty", path)
	}

	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&cfg); err != nil {
		return cfg, fmt.Errorf("parse hardware config %q: %w", path, err)
	}
	cfg = cfg.Normalize()
	if err := cfg.Validate(); err != nil {
		return cfg, fmt.Errorf("invalid hardware config %q: %w", path, err)
	}
	return cfg, nil
}

func (c HardwareConfig) Normalize() HardwareConfig {
	c.BootFirmware = NormalizeBootFirmware(c.BootFirmware)
	c.DiskSize = strings.TrimSpace(c.DiskSize)
	c.QEMU.CPUModel = strings.TrimSpace(c.QEMU.CPUModel)
	c.QEMU.DiskInterface = strings.ToLower(strings.TrimSpace(c.QEMU.DiskInterface))
	c.QEMU.ISOInterface = strings.ToLower(strings.TrimSpace(c.QEMU.ISOInterface))
	c.VCenter.SCSIController = strings.ToLower(strings.TrimSpace(c.VCenter.SCSIController))
	c.VCenter.NetworkAdapter = strings.ToLower(strings.TrimSpace(c.VCenter.NetworkAdapter))
	c.VCenter.Network = strings.TrimSpace(c.VCenter.Network)
	c.VCenter.DiskProvisioning = normalizeVCenterDiskProvisioning(c.VCenter.DiskProvisioning)
	c.VCenter.OutputType = NormalizeVCenterOutputType(c.VCenter.OutputType)
	return c
}

func (c HardwareConfig) Validate() error {
	c = c.Normalize()
	if !ValidateBootFirmware(c.BootFirmware) {
		return fmt.Errorf("boot_firmware must be %q or %q", BootFirmwareUEFI, BootFirmwareBIOS)
	}
	if strings.TrimSpace(c.DiskSize) == "" {
		return errors.New("disk_size is required")
	}
	if _, err := ParseDiskSize(c.DiskSize); err != nil {
		return fmt.Errorf("disk_size is invalid: %w", err)
	}
	if c.VCPU <= 0 {
		return errors.New("vcpu must be greater than zero")
	}
	if c.MemoryMB <= 0 {
		return errors.New("memory_mb must be greater than zero")
	}
	if strings.TrimSpace(c.QEMU.CPUModel) == "" {
		return errors.New("qemu.cpu_model must not be empty")
	}
	if !isOneOf(c.QEMU.DiskInterface, "virtio", "ide", "scsi", "sata") {
		return errors.New("qemu.disk_interface must be one of virtio, ide, scsi, sata")
	}
	if !isOneOf(c.QEMU.ISOInterface, "virtio", "ide", "scsi", "sata") {
		return errors.New("qemu.iso_interface must be one of virtio, ide, scsi, sata")
	}
	if !isOneOf(c.VCenter.SCSIController, "pvscsi", "lsilogic", "buslogic", "lsilogic-sas") {
		return errors.New("vcenter.scsi_controller must be one of pvscsi, lsilogic, buslogic, lsilogic-sas")
	}
	if !isOneOf(c.VCenter.NetworkAdapter, "vmxnet3", "vmxnet2", "vmxnet", "e1000", "e1000e") {
		return errors.New("vcenter.network_adapter must be one of vmxnet3, vmxnet2, vmxnet, e1000, e1000e")
	}
	if !isOneOf(c.VCenter.DiskProvisioning, VCenterDiskProvisioningThin, VCenterDiskProvisioningThickLazyZeroed, VCenterDiskProvisioningThickEagerZeroed) {
		return errors.New("vcenter.disk_provisioning must be one of thin, thick_provision_lazy_zeroed, thick_provision_eager_zeroed")
	}
	if !isOneOf(c.VCenter.OutputType, VCenterOutputTypeTemplate, VCenterOutputTypeVM) {
		return errors.New("vcenter.output_type must be template or vm")
	}
	return nil
}

func normalizeVCenterDiskProvisioning(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.ReplaceAll(value, "-", "_")
	value = strings.Join(strings.Fields(value), "_")
	switch value {
	case "", "thick", "thick_lazy_zeroed", "thick_provisioned_lazy_zeroed":
		return VCenterDiskProvisioningThickLazyZeroed
	case "eager_zeroed", "thick_eager_zeroed", "thick_provisioned_eager_zeroed":
		return VCenterDiskProvisioningThickEagerZeroed
	default:
		return value
	}
}

func NormalizeVCenterOutputType(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.ReplaceAll(value, "-", "_")
	value = strings.Join(strings.Fields(value), "_")
	if value == "" {
		return VCenterOutputTypeTemplate
	}
	return value
}

func isOneOf(value string, allowed ...string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}

func NormalizeBootFirmware(firmware string) string {
	firmware = strings.ToLower(strings.TrimSpace(firmware))
	if firmware == "" {
		return DefaultBootFirmware
	}
	return firmware
}

func ValidateBootFirmware(firmware string) bool {
	firmware = NormalizeBootFirmware(firmware)
	return firmware == BootFirmwareUEFI || firmware == BootFirmwareBIOS
}

func CheckISOFile(path string) error {
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("ubuntu ISO file %q not found", path)
		}
		return fmt.Errorf("stat ubuntu ISO %q: %w", path, err)
	}

	d, err := diskfs.Open(path, diskfs.WithOpenMode(diskfs.ReadOnly))
	if err != nil {
		return fmt.Errorf("open ISO %q: %w", path, err)
	}
	defer d.Close()

	fs, err := d.GetFilesystem(0)
	if err != nil {
		return fmt.Errorf("read filesystem from ISO %q: %w", path, err)
	}
	defer fs.Close()

	isoFS, ok := fs.(*iso9660.FileSystem)
	if !ok {
		return fmt.Errorf("filesystem in %q is not ISO9660", path)
	}

	for _, bootFile := range []string{"/casper/vmlinuz", "/casper/initrd"} {
		file, err := OpenISOFile(isoFS, bootFile)
		if err != nil {
			return fmt.Errorf("required boot file %s not found in ISO %q: %w", bootFile, path, err)
		}
		if err := file.Close(); err != nil {
			return fmt.Errorf("close %s from ISO %q: %w", bootFile, path, err)
		}
	}

	return nil
}

func LoadUserData(path string) ([]byte, string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, "", fmt.Errorf("read user-data file %q: %w", path, err)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, "", fmt.Errorf("user-data file %q is empty", path)
	}

	hostname, err := ValidateUserData(data)
	if err != nil {
		return nil, "", fmt.Errorf("invalid user-data file %q: %w", path, err)
	}
	return data, hostname, nil
}

func ValidateUserData(data []byte) (string, error) {
	autoinstall, err := ParseAutoinstallMapping(data)
	if err != nil {
		return "", err
	}

	identity := MappingValue(autoinstall, "identity")
	if identity == nil || identity.Kind != yaml.MappingNode {
		return "", nil
	}

	hostname := MappingValue(identity, "hostname")
	if hostname == nil || hostname.Kind != yaml.ScalarNode {
		return "", nil
	}
	return strings.TrimSpace(hostname.Value), nil
}

func ValidateUEFIPortableUserData(data []byte) error {
	autoinstall, err := ParseAutoinstallMapping(data)
	if err != nil {
		return fmt.Errorf("UEFI user-data compatibility check failed: %w", err)
	}

	if !hasPortableUEFIFallbackCommand(autoinstall) {
		return uefiCompatibilityError("missing autoinstall.late-commands entry that installs a fallback UEFI bootloader")
	}

	storage := MappingValue(autoinstall, "storage")
	if storage == nil {
		return nil
	}
	if storage.Kind != yaml.MappingNode {
		return uefiCompatibilityError("autoinstall.storage must be a mapping when present")
	}

	config := MappingValue(storage, "config")
	if config == nil {
		return nil
	}
	if config.Kind != yaml.SequenceNode {
		return uefiCompatibilityError("autoinstall.storage.config must be a list when present")
	}
	if !storageConfigHasUEFIESP(config) {
		return uefiCompatibilityError("custom autoinstall.storage.config must define a GPT EFI System Partition formatted as FAT and mounted at /boot/efi")
	}

	return nil
}

func ParseAutoinstallMapping(data []byte) (*yaml.Node, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("parse YAML: %w", err)
	}

	return AutoinstallMappingFromRoot(&root)
}

func AutoinstallMappingFromRoot(root *yaml.Node) (*yaml.Node, error) {
	node := root
	if root.Kind == yaml.DocumentNode {
		if len(root.Content) == 0 {
			return nil, errors.New("YAML document is empty")
		}
		node = root.Content[0]
	}
	if node.Kind != yaml.MappingNode {
		return nil, errors.New("top-level YAML document must be a mapping")
	}

	autoinstall := MappingValue(node, "autoinstall")
	if autoinstall == nil || autoinstall.Kind != yaml.MappingNode {
		return nil, errors.New("top-level autoinstall mapping is required")
	}
	return autoinstall, nil
}

func hasPortableUEFIFallbackCommand(autoinstall *yaml.Node) bool {
	lateCommands := MappingValue(autoinstall, "late-commands")
	if lateCommands == nil || lateCommands.Kind != yaml.SequenceNode {
		return false
	}
	for _, commandNode := range lateCommands.Content {
		if isPortableUEFIFallbackCommand(commandNodeText(commandNode)) {
			return true
		}
	}
	return false
}

func commandNodeText(node *yaml.Node) string {
	switch {
	case node == nil:
		return ""
	case node.Kind == yaml.ScalarNode:
		return node.Value
	case node.Kind == yaml.SequenceNode:
		var parts []string
		for _, child := range node.Content {
			if child.Kind == yaml.ScalarNode {
				parts = append(parts, child.Value)
			}
		}
		return strings.Join(parts, " ")
	default:
		return ""
	}
}

func isPortableUEFIFallbackCommand(command string) bool {
	command = strings.ToLower(command)
	if strings.Contains(command, "grub-install") && strings.Contains(command, "--removable") {
		return true
	}
	command = strings.ReplaceAll(command, "\\", "/")
	return strings.Contains(command, "efi/boot/bootx64.efi")
}

func storageConfigHasUEFIESP(config *yaml.Node) bool {
	gptDisks := make(map[string]bool)
	bootPartitionDevice := make(map[string]string)
	fatFormatForVolume := make(map[string]string)
	efiMountDevices := make(map[string]bool)

	for _, action := range config.Content {
		if action.Kind != yaml.MappingNode {
			continue
		}
		id := MappingScalarValue(action, "id")
		switch MappingScalarValue(action, "type") {
		case "disk":
			if id != "" && MappingScalarValue(action, "ptable") == "gpt" {
				gptDisks[id] = true
			}
		case "partition":
			deviceID := MappingScalarValue(action, "device")
			if id != "" && deviceID != "" && MappingScalarValue(action, "flag") == "boot" {
				bootPartitionDevice[id] = deviceID
			}
		case "format":
			volumeID := MappingScalarValue(action, "volume")
			if id != "" && volumeID != "" && isFATFilesystem(MappingScalarValue(action, "fstype")) {
				fatFormatForVolume[volumeID] = id
			}
		case "mount":
			deviceID := MappingScalarValue(action, "device")
			if deviceID != "" && MappingScalarValue(action, "path") == "/boot/efi" {
				efiMountDevices[deviceID] = true
			}
		}
	}

	for partitionID, deviceID := range bootPartitionDevice {
		if !gptDisks[deviceID] {
			continue
		}
		formatID := fatFormatForVolume[partitionID]
		if formatID == "" {
			continue
		}
		if efiMountDevices[formatID] || efiMountDevices[partitionID] {
			return true
		}
	}

	return false
}

func isFATFilesystem(fstype string) bool {
	switch strings.ToLower(strings.TrimSpace(fstype)) {
	case "fat", "fat16", "fat32", "vfat":
		return true
	default:
		return false
	}
}

func MappingScalarValue(node *yaml.Node, key string) string {
	value := MappingValue(node, key)
	if value == nil || value.Kind != yaml.ScalarNode {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(value.Value))
}

func uefiCompatibilityError(reason string) error {
	return fmt.Errorf("UEFI user-data is not portable as a single image: %s. Add an EFI System Partition mounted at /boot/efi and a late-command such as: curtin in-target --target=/target -- grub-install --target=x86_64-efi --efi-directory=/boot/efi --bootloader-id=ubuntu --removable --recheck", reason)
}

func MappingValue(node *yaml.Node, key string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for idx := 0; idx+1 < len(node.Content); idx += 2 {
		if node.Content[idx].Value == key {
			return node.Content[idx+1]
		}
	}
	return nil
}

func SetMappingScalar(mapping *yaml.Node, key, value string) {
	for idx := 0; idx+1 < len(mapping.Content); idx += 2 {
		if mapping.Content[idx].Value == key {
			mapping.Content[idx+1] = &yaml.Node{
				Kind:  yaml.ScalarNode,
				Tag:   "!!str",
				Value: value,
			}
			return
		}
	}

	mapping.Content = append(mapping.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value},
	)
}

func EnsureCloudConfigHeader(data []byte) []byte {
	trimmed := bytes.TrimLeft(data, " \t\r\n")
	if bytes.HasPrefix(trimmed, []byte("#cloud-config")) {
		return data
	}

	out := make([]byte, 0, len("#cloud-config\n")+len(data))
	out = append(out, "#cloud-config\n"...)
	out = append(out, data...)
	return out
}

func ParseDiskSize(size string) (int64, error) {
	size = strings.TrimSpace(size)
	re := regexp.MustCompile(`(?i)^([0-9]+)([kmgt]?i?b?)?$`)
	m := re.FindStringSubmatch(size)
	if m == nil {
		return 0, fmt.Errorf("invalid disk size %q", size)
	}

	value, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid disk size %q: %w", size, err)
	}

	unit := strings.ToUpper(m[2])
	unit = strings.TrimSuffix(unit, "IB")
	unit = strings.TrimSuffix(unit, "B")

	mult := int64(1)
	switch unit {
	case "":
		mult = 1
	case "K":
		mult = 1024
	case "M":
		mult = 1024 * 1024
	case "G":
		mult = 1024 * 1024 * 1024
	case "T":
		mult = 1024 * 1024 * 1024 * 1024
	default:
		return 0, fmt.Errorf("unsupported size unit in %q", size)
	}

	if value > math.MaxInt64/mult {
		return 0, fmt.Errorf("disk size overflows int64: %q", size)
	}

	return value * mult, nil
}

func SafeName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return FallbackName
	}

	var b strings.Builder
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			b.WriteRune(r)
			continue
		}
		b.WriteByte('-')
	}

	clean := strings.Trim(b.String(), "-_.")
	if clean == "" {
		return FallbackName
	}
	return clean
}

func ExtractFileFromISO(isoPath, sourcePath, destinationPath string) error {
	data, err := ReadISOFile(isoPath, sourcePath)
	if err != nil {
		return err
	}

	out, err := os.OpenFile(destinationPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("create destination file %q: %w", destinationPath, err)
	}
	defer out.Close()

	if _, err := out.Write(data); err != nil {
		return fmt.Errorf("write destination file %q: %w", destinationPath, err)
	}
	return nil
}

func ReadISOFile(isoPath, sourcePath string) ([]byte, error) {
	d, err := diskfs.Open(isoPath, diskfs.WithOpenMode(diskfs.ReadOnly))
	if err != nil {
		return nil, fmt.Errorf("open ISO %q: %w", isoPath, err)
	}
	defer d.Close()

	fs, err := d.GetFilesystem(0)
	if err != nil {
		return nil, fmt.Errorf("read filesystem from ISO %q: %w", isoPath, err)
	}
	defer fs.Close()

	isoFS, ok := fs.(*iso9660.FileSystem)
	if !ok {
		return nil, fmt.Errorf("filesystem in %q is not ISO9660", isoPath)
	}

	in, err := OpenISOFile(isoFS, sourcePath)
	if err != nil {
		return nil, err
	}
	defer in.Close()

	data, err := io.ReadAll(in)
	if err != nil {
		return nil, fmt.Errorf("read %s from ISO %q: %w", sourcePath, isoPath, err)
	}
	return data, nil
}

func OpenISOFile(isoFS *iso9660.FileSystem, sourcePath string) (io.ReadCloser, error) {
	candidates := []string{
		sourcePath,
		strings.TrimPrefix(sourcePath, "/"),
	}
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		file, err := isoFS.OpenFile(candidate, os.O_RDONLY)
		if err == nil {
			return file, nil
		}
	}

	resolvedPath, err := ResolveISOPathCaseInsensitive(isoFS, sourcePath)
	if err != nil {
		return nil, err
	}

	file, err := isoFS.OpenFile(resolvedPath, os.O_RDONLY)
	if err != nil {
		return nil, err
	}
	return file, nil
}

func ResolveISOPathCaseInsensitive(isoFS *iso9660.FileSystem, sourcePath string) (string, error) {
	trimmed := strings.Trim(strings.TrimSpace(sourcePath), "/")
	if trimmed == "" {
		return "", fmt.Errorf("invalid source path %q", sourcePath)
	}

	parts := strings.Split(trimmed, "/")
	canonicalParts := make([]string, 0, len(parts))

	currentPath := "/"
	for idx, part := range parts {
		if part == "" {
			continue
		}
		entries, err := isoFS.ReadDir(currentPath)
		if err != nil {
			return "", fmt.Errorf("read directory %q: %w", currentPath, err)
		}

		match := ""
		for _, entry := range entries {
			if strings.EqualFold(entry.Name(), part) {
				match = entry.Name()
				break
			}
		}
		if match == "" {
			return "", fmt.Errorf("path %q not found in ISO", sourcePath)
		}

		canonicalParts = append(canonicalParts, match)
		if idx < len(parts)-1 {
			currentPath = "/" + strings.Join(canonicalParts, "/")
		}
	}

	return "/" + strings.Join(canonicalParts, "/"), nil
}

func EnsureWritableNewFile(path string) error {
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("destination image file %q already exists", path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("check destination image %q: %w", path, err)
	}

	parent := filepath.Dir(path)
	if parent == "" {
		parent = "."
	}
	info, err := os.Stat(parent)
	if err != nil {
		return fmt.Errorf("check destination image directory %q: %w", parent, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("destination image parent %q is not a directory", parent)
	}
	return nil
}
