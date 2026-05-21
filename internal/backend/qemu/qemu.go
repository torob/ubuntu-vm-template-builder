package qemu

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	diskfs "github.com/diskfs/go-diskfs"
	"github.com/diskfs/go-diskfs/disk"
	"github.com/diskfs/go-diskfs/filesystem"
	"github.com/diskfs/go-diskfs/filesystem/iso9660"
	"golang.org/x/sys/unix"

	"ubuntu-vm-template-builder/internal/common"
	"ubuntu-vm-template-builder/internal/offlineapt"
	"ubuntu-vm-template-builder/internal/qemulog"
	"ubuntu-vm-template-builder/internal/seediso"
)

const (
	SeedISOSizeBytes  int64 = 8 * 1024 * 1024
	DefaultDiskFormat       = "raw"
	interruptGrace          = 5 * time.Second
)

type Config struct {
	UbuntuISO     string
	ImagePath     string
	UserDataPath  string
	UserData      []byte
	DiskSize      string
	DiskFormat    string
	DisplayName   string
	Hardware      common.HardwareConfig
	ExtraPackages offlineapt.Config
}

type Installer struct {
	ubuntuISO            string
	installerISO         string
	imagePath            string
	userData             []byte
	diskSize             string
	diskFmt              string
	hardware             common.HardwareConfig
	ovmfCodePath         string
	ovmfVarsTemplatePath string
	display              string
	tempDir              string
	extraPackages        offlineapt.Config
	offlineRepo          offlineapt.Repository
}

type OSInfo struct {
	GOOS      string
	ID        string
	Name      string
	VersionID string
	IDLike    []string
}

type OVMFFirmware struct {
	CodePath string
	VarsPath string
}

var OVMFFirmwareCandidates = []OVMFFirmware{
	{CodePath: "/usr/share/OVMF/OVMF_CODE_4M.fd", VarsPath: "/usr/share/OVMF/OVMF_VARS_4M.fd"},
	{CodePath: "/usr/share/OVMF/OVMF_CODE.fd", VarsPath: "/usr/share/OVMF/OVMF_VARS.fd"},
	{CodePath: "/usr/share/edk2/ovmf/OVMF_CODE.fd", VarsPath: "/usr/share/edk2/ovmf/OVMF_VARS.fd"},
	{CodePath: "/usr/share/edk2/ovmf/OVMF_CODE_4M.fd", VarsPath: "/usr/share/edk2/ovmf/OVMF_VARS_4M.fd"},
	{CodePath: "/usr/share/edk2/x64/OVMF_CODE.fd", VarsPath: "/usr/share/edk2/x64/OVMF_VARS.fd"},
	{CodePath: "/usr/share/edk2/x64/OVMF_CODE.4m.fd", VarsPath: "/usr/share/edk2/x64/OVMF_VARS.4m.fd"},
	{CodePath: "/usr/share/qemu/ovmf-x86_64-code.bin", VarsPath: "/usr/share/qemu/ovmf-x86_64-vars.bin"},
}

func NewInstaller(cfg Config) (*Installer, error) {
	tmpDir, err := os.MkdirTemp("", "ubuntu-installer-")
	if err != nil {
		return nil, fmt.Errorf("create temporary directory: %w", err)
	}

	fmt.Printf("Created temporary directory: %s\n", tmpDir)

	displayName := cfg.DisplayName
	if displayName == "" {
		displayName = common.FallbackName
	}

	hardware := cfg.Hardware.Normalize()
	if err := hardware.Validate(); err != nil {
		os.RemoveAll(tmpDir)
		return nil, err
	}

	var firmware OVMFFirmware
	if common.NormalizeBootFirmware(hardware.BootFirmware) == common.BootFirmwareUEFI {
		firmware, err = FindOVMFFirmware()
		if err != nil {
			os.RemoveAll(tmpDir)
			return nil, err
		}
	}

	diskFormat := strings.ToLower(strings.TrimSpace(cfg.DiskFormat))
	if diskFormat == "" {
		diskFormat = DefaultDiskFormat
	}

	return &Installer{
		ubuntuISO:            cfg.UbuntuISO,
		installerISO:         cfg.UbuntuISO,
		imagePath:            cfg.ImagePath,
		userData:             cfg.UserData,
		diskSize:             cfg.DiskSize,
		diskFmt:              diskFormat,
		hardware:             hardware,
		ovmfCodePath:         firmware.CodePath,
		ovmfVarsTemplatePath: firmware.VarsPath,
		display:              displayName,
		tempDir:              tmpDir,
		extraPackages:        cfg.ExtraPackages.Normalize(),
	}, nil
}

func (i *Installer) cleanup() {
	if i.tempDir == "" {
		return
	}
	if err := os.RemoveAll(i.tempDir); err != nil {
		fmt.Printf("Warning: could not clean up temporary directory %s: %v\n", i.tempDir, err)
		return
	}
	fmt.Printf("OK cleaned up temporary directory: %s\n", i.tempDir)
}

func CheckPrerequisites(cfg Config) error {
	cfg.Hardware = cfg.Hardware.Normalize()
	if err := cfg.Hardware.Validate(); err != nil {
		return err
	}
	if common.NormalizeBootFirmware(cfg.Hardware.BootFirmware) == common.BootFirmwareUEFI {
		if err := common.ValidateUEFIPortableUserData(cfg.UserData); err != nil {
			return err
		}
	}
	if _, err := exec.LookPath("qemu-system-x86_64"); err != nil {
		return errors.New("missing required dependency: qemu-system-x86_64")
	}
	if _, err := exec.LookPath("xorriso"); err != nil {
		return errors.New("missing required dependency: xorriso")
	}
	diskFormat := strings.ToLower(strings.TrimSpace(cfg.DiskFormat))
	if diskFormat == "" {
		diskFormat = DefaultDiskFormat
	}
	if diskFormat == "qcow2" || diskFormat == "vmdk" {
		if _, err := exec.LookPath("qemu-img"); err != nil {
			return errors.New("missing required dependency: qemu-img")
		}
	}
	if common.NormalizeBootFirmware(cfg.Hardware.BootFirmware) == common.BootFirmwareUEFI {
		if _, err := FindOVMFFirmware(); err != nil {
			return err
		}
	}
	if err := CheckKVMAccess(); err != nil {
		return err
	}
	if err := common.CheckISOFile(cfg.UbuntuISO); err != nil {
		return err
	}
	if err := offlineapt.CheckPrerequisites(cfg.ExtraPackages); err != nil {
		return err
	}
	if err := checkOutputPath(cfg.ImagePath); err != nil {
		return err
	}
	if _, err := common.ParseDiskSize(cfg.DiskSize); err != nil {
		return err
	}
	return nil
}

func CheckKVMAccess() error {
	info, err := os.Stat("/dev/kvm")
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return errors.New("/dev/kvm not found; this program launches QEMU with --enable-kvm")
		}
		return fmt.Errorf("check /dev/kvm: %w", err)
	}
	if info.IsDir() {
		return errors.New("/dev/kvm is a directory, expected a device")
	}

	f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("/dev/kvm is not accessible for read/write: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close /dev/kvm: %w", err)
	}
	return nil
}

func FindOVMFFirmware() (OVMFFirmware, error) {
	return FindOVMFFirmwareIn(OVMFFirmwareCandidates)
}

func FindOVMFFirmwareIn(candidates []OVMFFirmware) (OVMFFirmware, error) {
	for _, candidate := range candidates {
		if isRegularFile(candidate.CodePath) && isRegularFile(candidate.VarsPath) {
			return candidate, nil
		}
	}
	return OVMFFirmware{}, errors.New("missing OVMF UEFI firmware; install OVMF or use boot_firmware: bios")
}

func isRegularFile(path string) bool {
	if strings.TrimSpace(path) == "" {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

func copyFile(sourcePath, destinationPath string) error {
	in, err := os.Open(sourcePath)
	if err != nil {
		return fmt.Errorf("open %q: %w", sourcePath, err)
	}
	defer in.Close()

	out, err := os.OpenFile(destinationPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("create %q: %w", destinationPath, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return fmt.Errorf("copy %q to %q: %w", sourcePath, destinationPath, err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("close %q: %w", destinationPath, err)
	}
	return nil
}

func checkOutputPath(path string) error {
	if err := common.EnsureWritableNewFile(path); err != nil {
		return err
	}
	parent := filepath.Dir(path)
	if parent == "" {
		parent = "."
	}
	if err := unix.Access(parent, unix.W_OK); err != nil {
		return fmt.Errorf("destination image parent %q is not writable: %w", parent, err)
	}
	return nil
}

func ValidateDiskFormat(format string) bool {
	format = strings.ToLower(strings.TrimSpace(format))
	return format == "raw" || format == "vmdk" || format == "qcow2"
}

func (i *Installer) createSeedISO() (string, error) {
	fmt.Println("Creating seed ISO...")
	seedPath := filepath.Join(i.tempDir, fmt.Sprintf("seed-%s.iso", common.SafeName(i.display)))

	d, err := diskfs.Create(seedPath, SeedISOSizeBytes, diskfs.SectorSizeDefault)
	if err != nil {
		return "", fmt.Errorf("create ISO image: %w", err)
	}
	defer d.Close()

	d.LogicalBlocksize = 2048

	fs, err := d.CreateFilesystem(disk.FilesystemSpec{
		Partition:   0,
		FSType:      filesystem.TypeISO9660,
		VolumeLabel: "CIDATA",
	})
	if err != nil {
		return "", fmt.Errorf("create ISO9660 filesystem: %w", err)
	}
	defer fs.Close()

	seedUserData, err := seediso.TransformUserDataWithOptions(i.userData, seediso.Options{ExtraPackages: i.offlineRepo.InstallConfig()})
	if err != nil {
		return "", fmt.Errorf("prepare seed user-data: %w", err)
	}

	if err := writeISOFile(fs, "/user-data", seedUserData); err != nil {
		return "", err
	}
	if err := writeISOFile(fs, "/meta-data", nil); err != nil {
		return "", err
	}

	isoFS, ok := fs.(*iso9660.FileSystem)
	if !ok {
		return "", errors.New("created filesystem is not ISO9660")
	}
	if err := isoFS.Finalize(iso9660.FinalizeOptions{
		RockRidge:        true,
		VolumeIdentifier: "CIDATA",
	}); err != nil {
		return "", fmt.Errorf("finalize seed ISO: %w", err)
	}

	fmt.Printf("OK seed ISO created: %s\n", seedPath)
	return seedPath, nil
}

func writeISOFile(fs filesystem.FileSystem, path string, data []byte) error {
	file, err := fs.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_TRUNC)
	if err != nil {
		return fmt.Errorf("open %s in seed ISO: %w", path, err)
	}
	if len(data) > 0 {
		if _, err := file.Write(data); err != nil {
			file.Close()
			return fmt.Errorf("write %s in seed ISO: %w", path, err)
		}
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close %s in seed ISO: %w", path, err)
	}
	return nil
}

func (i *Installer) createDiskImage() error {
	fmt.Printf("Creating %s disk image (%s) at %s...\n", i.diskFmt, i.diskSize, i.imagePath)

	if i.diskFmt == DefaultDiskFormat {
		sizeBytes, err := common.ParseDiskSize(i.diskSize)
		if err != nil {
			return err
		}
		f, err := os.OpenFile(i.imagePath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			return fmt.Errorf("create raw image: %w", err)
		}
		defer f.Close()
		if err := f.Truncate(sizeBytes); err != nil {
			return fmt.Errorf("truncate raw image: %w", err)
		}
	} else {
		cmd := exec.Command("qemu-img", "create", "-f", i.diskFmt, i.imagePath, i.diskSize)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("qemu-img create failed: %v (%s)", err, strings.TrimSpace(string(out)))
		}
	}

	fmt.Printf("OK disk image created: %s\n", i.imagePath)
	return nil
}

func (i *Installer) extractBootFiles() (string, string, error) {
	fmt.Println("Extracting boot files from Ubuntu ISO...")
	kernelPath := filepath.Join(i.tempDir, "vmlinuz")
	initrdPath := filepath.Join(i.tempDir, "initrd")

	if err := common.ExtractFileFromISO(i.activeISO(), "/casper/vmlinuz", kernelPath); err != nil {
		return "", "", fmt.Errorf("extract kernel from ISO: %w", err)
	}
	if err := common.ExtractFileFromISO(i.activeISO(), "/casper/initrd", initrdPath); err != nil {
		return "", "", fmt.Errorf("extract initrd from ISO: %w", err)
	}

	fmt.Printf("OK extracted kernel: %s\n", kernelPath)
	fmt.Printf("OK extracted initrd: %s\n", initrdPath)
	return kernelPath, initrdPath, nil
}

func (i *Installer) activeISO() string {
	if strings.TrimSpace(i.installerISO) != "" {
		return i.installerISO
	}
	return i.ubuntuISO
}

func runInterruptibleCommand(cmd *exec.Cmd, signals <-chan os.Signal, grace time.Duration) error {
	configureCommandProcessGroup(cmd)

	if err := cmd.Start(); err != nil {
		return err
	}
	return waitInterruptibleCommand(cmd, signals, grace)
}

func configureCommandProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

func waitInterruptibleCommand(cmd *exec.Cmd, signals <-chan os.Signal, grace time.Duration) error {
	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	select {
	case err := <-done:
		return err
	case sig := <-signals:
		fmt.Printf("\nInterrupt received; stopping QEMU...\n")
		if err := signalCommandProcessGroup(cmd, signalToSyscall(sig)); err != nil {
			return fmt.Errorf("stop QEMU after interrupt: %w", err)
		}

		timer := time.NewTimer(grace)
		defer timer.Stop()

		select {
		case err := <-done:
			if err != nil {
				return fmt.Errorf("qemu stopped after interrupt: %w", err)
			}
			return errors.New("qemu stopped after interrupt")
		case <-signals:
			fmt.Println("Second interrupt received; killing QEMU...")
			if err := signalCommandProcessGroup(cmd, syscall.SIGKILL); err != nil {
				return fmt.Errorf("kill QEMU after repeated interrupt: %w", err)
			}
			return fmt.Errorf("qemu killed after repeated interrupt: %w", <-done)
		case <-timer.C:
			fmt.Printf("QEMU did not stop within %s; killing it...\n", grace)
			if err := signalCommandProcessGroup(cmd, syscall.SIGKILL); err != nil {
				return fmt.Errorf("kill QEMU after interrupt timeout: %w", err)
			}
			return fmt.Errorf("qemu killed after interrupt timeout: %w", <-done)
		}
	}
}

func signalCommandProcessGroup(cmd *exec.Cmd, sig syscall.Signal) error {
	if cmd.Process == nil {
		return nil
	}
	if err := syscall.Kill(-cmd.Process.Pid, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}

func signalToSyscall(sig os.Signal) syscall.Signal {
	if sysSig, ok := sig.(syscall.Signal); ok {
		return sysSig
	}
	return syscall.SIGTERM
}

func (i *Installer) QEMUArgs(seedISOPath, kernelPath, initrdPath string) ([]string, error) {
	args := []string{
		"--enable-kvm",
		"-cpu", i.hardware.QEMU.CPUModel,
		"-smp", strconv.Itoa(i.hardware.VCPU),
		"-no-reboot",
		"-m", strconv.Itoa(i.hardware.MemoryMB),
		"-nographic",
	}

	if common.NormalizeBootFirmware(i.hardware.BootFirmware) == common.BootFirmwareUEFI {
		firmwareArgs, err := i.uefiFirmwareArgs()
		if err != nil {
			return nil, err
		}
		args = append(args, firmwareArgs...)
	}

	args = append(args,
		"-drive", fmt.Sprintf("file=%s,format=%s,cache=none,if=%s", i.imagePath, i.diskFmt, i.hardware.QEMU.DiskInterface),
		"-drive", fmt.Sprintf("file=%s,format=raw,readonly=on,if=%s", i.activeISO(), i.hardware.QEMU.ISOInterface),
		"-drive", fmt.Sprintf("file=%s,format=raw,readonly=on,if=%s", seedISOPath, i.hardware.QEMU.ISOInterface),
		"-kernel", kernelPath,
		"-initrd", initrdPath,
		"-append", "autoinstall console=ttyS0,115200n8 ---",
	)
	return args, nil
}

func (i *Installer) prepareInstallerISO(ctx context.Context) error {
	var repo offlineapt.Repository
	if i.extraPackages.Enabled() {
		fmt.Println("Preparing offline APT repository for extra packages...")
		var err error
		repo, err = offlineapt.BuildRepository(ctx, i.extraPackages, i.ubuntuISO, i.tempDir)
		if err != nil {
			return fmt.Errorf("prepare offline APT repository: %w", err)
		}
		fmt.Printf("OK offline APT repository prepared with %d requested package(s): %s\n", len(repo.Packages), repo.Path)
	}

	remasteredISO := filepath.Join(i.tempDir, fmt.Sprintf("installer-%s.iso", common.SafeName(i.display)))
	fmt.Println("Creating remastered installer ISO with builder support scripts...")
	if err := seediso.RemasterUbuntuISOWithSupport(ctx, i.ubuntuISO, remasteredISO, i.tempDir, repo.Path, seediso.Options{ExtraPackages: repo.InstallConfig()}); err != nil {
		return err
	}
	if err := offlineapt.ValidateEmbeddedRepository(remasteredISO, repo); err != nil {
		return err
	}
	i.offlineRepo = repo
	i.installerISO = remasteredISO
	fmt.Printf("OK remastered installer ISO created: %s\n", remasteredISO)
	return nil
}

func (i *Installer) uefiFirmwareArgs() ([]string, error) {
	varsPath := filepath.Join(i.tempDir, "OVMF_VARS.fd")
	if err := copyFile(i.ovmfVarsTemplatePath, varsPath); err != nil {
		return nil, fmt.Errorf("prepare OVMF variables store: %w", err)
	}

	return []string{
		"-drive", fmt.Sprintf("if=pflash,format=raw,readonly=on,file=%s", i.ovmfCodePath),
		"-drive", fmt.Sprintf("if=pflash,format=raw,file=%s", varsPath),
	}, nil
}

func (i *Installer) runQemuInstallation(seedISOPath, kernelPath, initrdPath string) bool {
	fmt.Println("Starting QEMU installation...")
	fmt.Println("This may take several minutes. The VM will automatically shut down when installation is complete.")

	args, err := i.QEMUArgs(seedISOPath, kernelPath, initrdPath)
	if err != nil {
		fmt.Printf("Installation failed: %v\n", err)
		return false
	}

	cmd := exec.Command("qemu-system-x86_64", args...)
	stdoutLog := qemulog.NewCompactingWriter(os.Stdout)
	stderrLog := qemulog.NewCompactingWriter(os.Stderr)
	cmd.Stdout = stdoutLog
	cmd.Stderr = stderrLog

	signals := make(chan os.Signal, 2)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)

	err = runInterruptibleCommand(cmd, signals, interruptGrace)
	if flushErr := stdoutLog.Flush(); flushErr != nil && err == nil {
		err = flushErr
	}
	if flushErr := stderrLog.Flush(); flushErr != nil && err == nil {
		err = flushErr
	}
	if err == nil {
		fmt.Println("OK installation completed successfully")
		return true
	}

	fmt.Printf("Installation failed: %v\n", err)
	return false
}

func (i *Installer) Install() bool {
	fmt.Printf("\n%s\n", strings.Repeat("=", 60))
	fmt.Printf("Installing: %s -> %s\n", i.display, i.imagePath)
	fmt.Printf("Boot firmware: %s\n", common.NormalizeBootFirmware(i.hardware.BootFirmware))
	fmt.Printf("%s\n", strings.Repeat("=", 60))

	defer i.cleanup()

	if err := i.prepareInstallerISO(context.Background()); err != nil {
		fmt.Printf("Installation failed for %s: %v\n", i.display, err)
		return false
	}

	seedISOPath, err := i.createSeedISO()
	if err != nil {
		fmt.Printf("Installation failed for %s: %v\n", i.display, err)
		return false
	}

	if err := i.createDiskImage(); err != nil {
		fmt.Printf("Installation failed for %s: %v\n", i.display, err)
		return false
	}

	kernelPath, initrdPath, err := i.extractBootFiles()
	if err != nil {
		fmt.Printf("Installation failed for %s: %v\n", i.display, err)
		return false
	}

	success := i.runQemuInstallation(seedISOPath, kernelPath, initrdPath)
	if success {
		fmt.Printf("\nOK installation completed successfully for %s\n", i.display)
		fmt.Printf("  Image: %s\n", i.imagePath)
		fmt.Printf("  Disk format: %s\n", i.diskFmt)
		fmt.Printf("  Boot firmware: %s\n", common.NormalizeBootFirmware(i.hardware.BootFirmware))
		fmt.Printf("  To boot: %s\n", i.bootCommandHint())
	}

	return success
}

func (i *Installer) bootCommandHint() string {
	diskDrive := fmt.Sprintf("-drive file=%s,format=%s,if=%s", i.imagePath, i.diskFmt, i.hardware.QEMU.DiskInterface)
	if common.NormalizeBootFirmware(i.hardware.BootFirmware) != common.BootFirmwareUEFI {
		return fmt.Sprintf("qemu-system-x86_64 --enable-kvm -m %d %s", i.hardware.MemoryMB, diskDrive)
	}

	return fmt.Sprintf("create a VM with EFI/UEFI firmware and attach %s as the boot disk", i.imagePath)
}

func DetectOSInfo() OSInfo {
	info := OSInfo{GOOS: runtime.GOOS}
	if runtime.GOOS != "linux" {
		info.Name = runtime.GOOS
		return info
	}

	values, err := readOSRelease("/etc/os-release")
	if err != nil {
		info.Name = "Linux"
		return info
	}

	info.ID = strings.ToLower(values["ID"])
	info.Name = values["PRETTY_NAME"]
	if info.Name == "" {
		info.Name = values["NAME"]
	}
	info.VersionID = values["VERSION_ID"]
	for _, idLike := range strings.Fields(strings.ToLower(values["ID_LIKE"])) {
		info.IDLike = append(info.IDLike, idLike)
	}
	if info.Name == "" {
		info.Name = "Linux"
	}
	return info
}

func readOSRelease(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseOSRelease(data), nil
}

func ParseOSRelease(data []byte) map[string]string {
	values := make(map[string]string)
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if unquoted, err := strconv.Unquote(value); err == nil {
			value = unquoted
		}
		values[key] = value
	}
	return values
}

func (o OSInfo) DisplayName() string {
	switch {
	case o.Name != "" && o.ID != "":
		return fmt.Sprintf("%s (%s)", o.Name, o.ID)
	case o.Name != "":
		return o.Name
	case o.GOOS != "":
		return o.GOOS
	default:
		return "unknown"
	}
}

func (o OSInfo) HasID(ids ...string) bool {
	for _, id := range ids {
		id = strings.ToLower(id)
		if o.ID == id {
			return true
		}
		for _, idLike := range o.IDLike {
			if idLike == id {
				return true
			}
		}
	}
	return false
}

func InstallSuggestion(osInfo OSInfo) string {
	if osInfo.GOOS != "linux" {
		return "Use a Linux host, then install QEMU from that distribution's package manager."
	}
	switch {
	case osInfo.HasID("debian", "ubuntu"):
		return "sudo apt update && sudo apt install -y qemu-system-x86 qemu-utils"
	case osInfo.HasID("fedora"):
		return "sudo dnf install -y qemu-system-x86 qemu-img"
	case osInfo.HasID("rhel", "centos", "rocky", "almalinux"):
		return "sudo dnf install -y qemu-kvm qemu-img"
	case osInfo.HasID("arch"):
		return "sudo pacman -S qemu-base"
	case osInfo.HasID("opensuse", "suse"):
		return "sudo zypper install qemu-x86 qemu-tools"
	default:
		return "Install the packages that provide qemu-system-x86_64 and qemu-img for this Linux distribution."
	}
}

func OVMFInstallSuggestion(osInfo OSInfo) string {
	if osInfo.GOOS != "linux" {
		return "Use a Linux host with OVMF installed, or set boot_firmware: bios."
	}
	switch {
	case osInfo.HasID("debian", "ubuntu"):
		return "sudo apt update && sudo apt install -y ovmf, or set boot_firmware: bios"
	case osInfo.HasID("fedora", "rhel", "centos", "rocky", "almalinux"):
		return "sudo dnf install -y edk2-ovmf, or set boot_firmware: bios"
	case osInfo.HasID("arch"):
		return "sudo pacman -S edk2-ovmf, or set boot_firmware: bios"
	case osInfo.HasID("opensuse", "suse"):
		return "sudo zypper install ovmf, or set boot_firmware: bios"
	default:
		return "Install the package that provides OVMF UEFI firmware for this Linux distribution, or set boot_firmware: bios."
	}
}

func GoVersionAtLeast(versionOutput string, wantMajor, wantMinor int) bool {
	re := regexp.MustCompile(`go([0-9]+)\.([0-9]+)`)
	m := re.FindStringSubmatch(versionOutput)
	if m == nil {
		return false
	}
	major, err := strconv.Atoi(m[1])
	if err != nil {
		return false
	}
	minor, err := strconv.Atoi(m[2])
	if err != nil {
		return false
	}
	if major != wantMajor {
		return major > wantMajor
	}
	return minor >= wantMinor
}
