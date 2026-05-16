package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
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
	"gopkg.in/yaml.v3"
)

const (
	seedISOSizeBytes   int64 = 8 * 1024 * 1024
	defaultDiskFmt           = "raw"
	fallbackName             = "ubuntu-autoinstall"
	qemuInterruptGrace       = 5 * time.Second
)

type InstallConfig struct {
	UbuntuISO    string
	ImagePath    string
	UserDataPath string
	UserData     []byte
	DiskSize     string
	DiskFormat   string
	DisplayName  string
}

type Installer struct {
	ubuntuISO string
	imagePath string
	userData  []byte
	diskSize  string
	diskFmt   string
	display   string
	tempDir   string
}

type OSInfo struct {
	GOOS      string
	ID        string
	Name      string
	VersionID string
	IDLike    []string
}

type prerequisiteItem struct {
	Name        string
	Description string
	Required    bool
	OK          bool
	Detail      string
	Suggestion  string
}

type prerequisiteReport struct {
	OS    OSInfo
	Items []prerequisiteItem
}

func newInstaller(cfg InstallConfig) (*Installer, error) {
	tmpDir, err := os.MkdirTemp("", "ubuntu-installer-")
	if err != nil {
		return nil, fmt.Errorf("create temporary directory: %w", err)
	}

	fmt.Printf("Created temporary directory: %s\n", tmpDir)

	displayName := cfg.DisplayName
	if displayName == "" {
		displayName = fallbackName
	}

	return &Installer{
		ubuntuISO: cfg.UbuntuISO,
		imagePath: cfg.ImagePath,
		userData:  cfg.UserData,
		diskSize:  cfg.DiskSize,
		diskFmt:   strings.ToLower(cfg.DiskFormat),
		display:   displayName,
		tempDir:   tmpDir,
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

func checkPrerequisites(cfg InstallConfig) error {
	if _, err := exec.LookPath("qemu-system-x86_64"); err != nil {
		return errors.New("missing required dependency: qemu-system-x86_64")
	}
	if cfg.DiskFormat == "qcow2" || cfg.DiskFormat == "vmdk" {
		if _, err := exec.LookPath("qemu-img"); err != nil {
			return errors.New("missing required dependency: qemu-img")
		}
	}
	if err := checkKVMAccess(); err != nil {
		return err
	}
	if err := checkISOFile(cfg.UbuntuISO); err != nil {
		return err
	}
	if err := checkOutputPath(cfg.ImagePath); err != nil {
		return err
	}
	if _, err := parseDiskSize(cfg.DiskSize); err != nil {
		return err
	}
	return nil
}

func checkKVMAccess() error {
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

func collectPrerequisites() prerequisiteReport {
	osInfo := detectOSInfo()
	installCmd := qemuInstallSuggestion(osInfo)

	items := []prerequisiteItem{
		{
			Name:        "Linux host",
			Description: "Required because this program launches QEMU with KVM through /dev/kvm.",
			Required:    true,
			OK:          osInfo.GOOS == "linux",
			Detail:      osInfo.DisplayName(),
			Suggestion:  "Run this program on a Linux host with KVM support.",
		},
		goPrerequisite(),
		commandPrerequisite("qemu-system-x86_64", "QEMU system emulator used to boot the Ubuntu installer.", true, installCmd),
		commandPrerequisite("qemu-img", "QEMU image tool used when creating qcow2 or vmdk images.", false, installCmd),
		kvmPrerequisite(osInfo),
	}

	return prerequisiteReport{
		OS:    osInfo,
		Items: items,
	}
}

func commandPrerequisite(command, description string, required bool, suggestion string) prerequisiteItem {
	path, err := exec.LookPath(command)
	item := prerequisiteItem{
		Name:        command,
		Description: description,
		Required:    required,
		OK:          err == nil,
		Suggestion:  suggestion,
	}
	if err == nil {
		item.Detail = path
	} else {
		item.Detail = "not found in PATH"
	}
	return item
}

func goPrerequisite() prerequisiteItem {
	path, err := exec.LookPath("go")
	item := prerequisiteItem{
		Name:        "Go 1.26 or newer",
		Description: "Required only when building from source or running with go run.",
		Required:    false,
		OK:          err == nil,
		Suggestion:  "Install Go 1.26 or newer from https://go.dev/dl/ or use a prebuilt install-ubuntu binary.",
	}
	if err != nil {
		item.Detail = "go not found in PATH"
		return item
	}

	out, err := exec.Command(path, "version").Output()
	if err != nil {
		item.OK = false
		item.Detail = fmt.Sprintf("%s exists but go version failed: %v", path, err)
		return item
	}
	item.Detail = strings.TrimSpace(string(out))
	if !goVersionAtLeast(item.Detail, 1, 26) {
		item.OK = false
		item.Detail = item.Detail + " (too old)"
	}
	return item
}

func goVersionAtLeast(versionOutput string, wantMajor, wantMinor int) bool {
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

func kvmPrerequisite(osInfo OSInfo) prerequisiteItem {
	err := checkKVMAccess()
	item := prerequisiteItem{
		Name:        "/dev/kvm access",
		Description: "Required for hardware-accelerated QEMU installs.",
		Required:    true,
		OK:          err == nil,
	}
	if err == nil {
		item.Detail = "read/write access is available"
		return item
	}

	item.Detail = err.Error()
	if osInfo.GOOS != "linux" {
		item.Suggestion = "Use a Linux host with KVM support."
		return item
	}
	if errors.Is(err, os.ErrNotExist) || strings.Contains(err.Error(), "not found") {
		item.Suggestion = "Enable virtualization in BIOS/UEFI, load the KVM module with sudo modprobe kvm_intel or sudo modprobe kvm_amd, and install QEMU packages."
		return item
	}
	item.Suggestion = "Grant access with sudo usermod -aG kvm $USER and start a new login shell; for the current session, sudo setfacl -m u:$USER:rw /dev/kvm can be used."
	return item
}

func detectOSInfo() OSInfo {
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
	return parseOSRelease(data), nil
}

func parseOSRelease(data []byte) map[string]string {
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

func qemuInstallSuggestion(osInfo OSInfo) string {
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

func (r prerequisiteReport) RequiredOK() bool {
	for _, item := range r.Items {
		if item.Required && !item.OK {
			return false
		}
	}
	return true
}

func runPrerequisitesCommand(out io.Writer) int {
	report := collectPrerequisites()
	printPrerequisiteReport(out, report)
	if !report.RequiredOK() {
		return 1
	}
	return 0
}

func printPrerequisiteReport(out io.Writer, report prerequisiteReport) {
	fmt.Fprintln(out, "Prerequisites")
	fmt.Fprintf(out, "OS: %s\n\n", report.OS.DisplayName())

	for _, item := range report.Items {
		status := "OK"
		if !item.OK && item.Required {
			status = "MISSING"
		} else if !item.OK {
			status = "OPTIONAL MISSING"
		}
		required := "required"
		if !item.Required {
			required = "optional"
		}

		fmt.Fprintf(out, "[%s] %s (%s)\n", status, item.Name, required)
		fmt.Fprintf(out, "  %s\n", item.Description)
		if item.Detail != "" {
			fmt.Fprintf(out, "  Detail: %s\n", item.Detail)
		}
		if !item.OK && item.Suggestion != "" {
			fmt.Fprintf(out, "  Fix: %s\n", item.Suggestion)
		}
	}

	fmt.Fprintln(out)
	fmt.Fprintln(out, "Install input prerequisites checked during normal install runs:")
	fmt.Fprintln(out, "- Ubuntu live-server ISO containing /casper/vmlinuz and /casper/initrd")
	fmt.Fprintln(out, "- cloud-init autoinstall user-data YAML with a top-level autoinstall mapping")
	fmt.Fprintln(out, "- destination image path that does not already exist")
	fmt.Fprintln(out, "- writable destination image directory")
	fmt.Fprintln(out, "- valid disk size such as 20G")

	fmt.Fprintln(out)
	if report.RequiredOK() {
		fmt.Fprintln(out, "Required host prerequisites are satisfied.")
		return
	}
	fmt.Fprintln(out, "One or more required host prerequisites are missing.")
}

func checkISOFile(path string) error {
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
		file, err := openISOFile(isoFS, bootFile)
		if err != nil {
			return fmt.Errorf("required boot file %s not found in ISO %q: %w", bootFile, path, err)
		}
		if err := file.Close(); err != nil {
			return fmt.Errorf("close %s from ISO %q: %w", bootFile, path, err)
		}
	}

	return nil
}

func checkOutputPath(path string) error {
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
	if err := unix.Access(parent, unix.W_OK); err != nil {
		return fmt.Errorf("destination image parent %q is not writable: %w", parent, err)
	}
	return nil
}

func loadUserData(path string) ([]byte, string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, "", fmt.Errorf("read user-data file %q: %w", path, err)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, "", fmt.Errorf("user-data file %q is empty", path)
	}

	hostname, err := validateUserData(data)
	if err != nil {
		return nil, "", fmt.Errorf("invalid user-data file %q: %w", path, err)
	}
	return data, hostname, nil
}

func validateUserData(data []byte) (string, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return "", fmt.Errorf("parse YAML: %w", err)
	}

	node := &root
	if root.Kind == yaml.DocumentNode {
		if len(root.Content) == 0 {
			return "", errors.New("YAML document is empty")
		}
		node = root.Content[0]
	}
	if node.Kind != yaml.MappingNode {
		return "", errors.New("top-level YAML document must be a mapping")
	}

	autoinstall := mappingValue(node, "autoinstall")
	if autoinstall == nil || autoinstall.Kind != yaml.MappingNode {
		return "", errors.New("top-level autoinstall mapping is required")
	}

	identity := mappingValue(autoinstall, "identity")
	if identity == nil || identity.Kind != yaml.MappingNode {
		return "", nil
	}

	hostname := mappingValue(identity, "hostname")
	if hostname == nil || hostname.Kind != yaml.ScalarNode {
		return "", nil
	}
	return strings.TrimSpace(hostname.Value), nil
}

func mappingValue(node *yaml.Node, key string) *yaml.Node {
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

func (i *Installer) createSeedISO() (string, error) {
	fmt.Println("Creating seed ISO...")
	seedPath := filepath.Join(i.tempDir, fmt.Sprintf("seed-%s.iso", safeName(i.display)))

	d, err := diskfs.Create(seedPath, seedISOSizeBytes, diskfs.SectorSizeDefault)
	if err != nil {
		return "", fmt.Errorf("create ISO image: %w", err)
	}
	defer d.Close()

	// ISO9660 supports only logical block sizes of 2048/4096/8192.
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

	if err := writeISOFile(fs, "/user-data", i.userData); err != nil {
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

	if i.diskFmt == defaultDiskFmt {
		sizeBytes, err := parseDiskSize(i.diskSize)
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

	if err := extractFileFromISO(i.ubuntuISO, "/casper/vmlinuz", kernelPath); err != nil {
		return "", "", fmt.Errorf("extract kernel from ISO: %w", err)
	}
	if err := extractFileFromISO(i.ubuntuISO, "/casper/initrd", initrdPath); err != nil {
		return "", "", fmt.Errorf("extract initrd from ISO: %w", err)
	}

	fmt.Printf("OK extracted kernel: %s\n", kernelPath)
	fmt.Printf("OK extracted initrd: %s\n", initrdPath)
	return kernelPath, initrdPath, nil
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

func (i *Installer) qemuArgs(seedISOPath, kernelPath, initrdPath string) []string {
	return []string{
		"--enable-kvm",
		"-no-reboot",
		"-m", "2048",
		"-nographic",
		"-drive", fmt.Sprintf("file=%s,format=%s,cache=none,if=virtio", i.imagePath, i.diskFmt),
		"-drive", fmt.Sprintf("file=%s,media=cdrom", i.ubuntuISO),
		"-drive", fmt.Sprintf("file=%s,media=cdrom", seedISOPath),
		"-kernel", kernelPath,
		"-initrd", initrdPath,
		"-append", "autoinstall console=ttyS0,115200n8 ---",
	}
}

func (i *Installer) runQemuInstallation(seedISOPath, kernelPath, initrdPath string) bool {
	fmt.Println("Starting QEMU installation...")
	fmt.Println("This may take several minutes. The VM will automatically shut down when installation is complete.")

	cmd := exec.Command("qemu-system-x86_64", i.qemuArgs(seedISOPath, kernelPath, initrdPath)...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	signals := make(chan os.Signal, 2)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)

	err := runInterruptibleCommand(cmd, signals, qemuInterruptGrace)
	if err == nil {
		fmt.Println("OK installation completed successfully")
		return true
	}

	fmt.Printf("Installation failed: %v\n", err)
	return false
}

func (i *Installer) install() bool {
	fmt.Printf("\n%s\n", strings.Repeat("=", 60))
	fmt.Printf("Installing: %s -> %s\n", i.display, i.imagePath)
	fmt.Printf("%s\n", strings.Repeat("=", 60))

	defer i.cleanup()

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
		fmt.Printf("  To boot: qemu-system-x86_64 --enable-kvm -m 2048 -drive file=%s,format=%s\n", i.imagePath, i.diskFmt)
	}

	return success
}

func parseDiskSize(size string) (int64, error) {
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

func validateDiskFormat(format string) bool {
	format = strings.ToLower(strings.TrimSpace(format))
	return format == "raw" || format == "vmdk" || format == "qcow2"
}

func safeName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return fallbackName
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
		return fallbackName
	}
	return clean
}

func requiredFlagErrors(values map[string]string) []string {
	var missing []string
	for name, value := range values {
		if strings.TrimSpace(value) == "" {
			missing = append(missing, "--"+name)
		}
	}
	return missing
}

func isPrerequisitesCommand(command string) bool {
	switch command {
	case "prerequisites", "prereqs", "prerequests", "prequests":
		return true
	default:
		return false
	}
}

func printPrerequisitesUsage(out io.Writer, program string) {
	fmt.Fprintf(out, "Usage: %s prerequisites\n\n", program)
	fmt.Fprintln(out, "Aliases: prereqs, prerequests, prequests")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Print host prerequisites, check whether required host tools are available,")
	fmt.Fprintln(out, "and show OS-specific installation or permission suggestions when something is missing.")
}

func main() {
	if len(os.Args) > 1 && isPrerequisitesCommand(os.Args[1]) {
		if len(os.Args) > 2 {
			if os.Args[2] == "-h" || os.Args[2] == "--help" {
				printPrerequisitesUsage(os.Stdout, os.Args[0])
				os.Exit(0)
			}
			fmt.Fprintf(os.Stderr, "Error: %s does not accept arguments\n", os.Args[1])
			printPrerequisitesUsage(os.Stderr, os.Args[0])
			os.Exit(1)
		}
		os.Exit(runPrerequisitesCommand(os.Stdout))
	}

	var (
		isoPath      string
		imagePath    string
		userDataPath string
		diskFormat   string
		diskSize     string
	)

	flag.StringVar(&isoPath, "iso", "", "Ubuntu ISO file path")
	flag.StringVar(&imagePath, "image", "", "Destination image file path")
	flag.StringVar(&userDataPath, "user-data", "", "cloud-init autoinstall user-data file")
	flag.StringVar(&diskFormat, "disk-format", defaultDiskFmt, "Disk image format (raw, vmdk, or qcow2)")
	flag.StringVar(&diskSize, "disk-size", "", "Disk image size (e.g., 20G, 50G)")
	flag.Usage = func() {
		printUsage(flag.CommandLine.Output(), os.Args[0])
	}
	flag.Parse()

	missing := requiredFlagErrors(map[string]string{
		"iso":       isoPath,
		"image":     imagePath,
		"user-data": userDataPath,
		"disk-size": diskSize,
	})
	if len(missing) > 0 {
		fmt.Fprintf(os.Stderr, "Error: missing required flags: %s\n", strings.Join(missing, ", "))
		flag.Usage()
		os.Exit(1)
	}

	diskFormat = strings.ToLower(strings.TrimSpace(diskFormat))
	if !validateDiskFormat(diskFormat) {
		fmt.Fprintf(os.Stderr, "Error: unsupported --disk-format %q. Supported values: raw, vmdk, qcow2\n", diskFormat)
		os.Exit(1)
	}

	userData, hostname, err := loadUserData(userDataPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	displayName := hostname
	if strings.TrimSpace(displayName) == "" {
		displayName = fallbackName
	}

	cfg := InstallConfig{
		UbuntuISO:    isoPath,
		ImagePath:    imagePath,
		UserDataPath: userDataPath,
		UserData:     userData,
		DiskSize:     diskSize,
		DiskFormat:   diskFormat,
		DisplayName:  displayName,
	}

	fmt.Println("Checking prerequisites...")
	if err := checkPrerequisites(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "Error: prerequisite check failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("OK prerequisites satisfied")

	installer, err := newInstaller(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create installer: %v\n", err)
		os.Exit(1)
	}

	if installer.install() {
		os.Exit(0)
	}
	os.Exit(1)
}

func printUsage(out io.Writer, program string) {
	fmt.Fprintf(out, "Usage: %s --iso ubuntu.iso --image output.img --disk-size 20G --user-data autoinstall.yaml [--disk-format raw|qcow2|vmdk]\n", program)
	fmt.Fprintf(out, "       %s prerequisites\n\n", program)
	fmt.Fprintln(out, "Commands:")
	fmt.Fprintln(out, "  prerequisites")
	fmt.Fprintln(out, "        Print host prerequisites and installation suggestions")
	fmt.Fprintln(out, "        Aliases: prereqs, prerequests, prequests")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Options:")
	fmt.Fprintln(out, "  --iso string")
	fmt.Fprintln(out, "        Ubuntu ISO file path")
	fmt.Fprintln(out, "  --image string")
	fmt.Fprintln(out, "        Destination image file path")
	fmt.Fprintln(out, "  --disk-size string")
	fmt.Fprintln(out, "        Disk image size (e.g., 20G, 50G)")
	fmt.Fprintln(out, "  --user-data string")
	fmt.Fprintln(out, "        cloud-init autoinstall user-data file")
	fmt.Fprintln(out, "  --disk-format string")
	fmt.Fprintln(out, "        Disk image format: raw, qcow2, or vmdk (default \"raw\")")
}

func extractFileFromISO(isoPath, sourcePath, destinationPath string) error {
	d, err := diskfs.Open(isoPath, diskfs.WithOpenMode(diskfs.ReadOnly))
	if err != nil {
		return fmt.Errorf("open ISO %q: %w", isoPath, err)
	}
	defer d.Close()

	fs, err := d.GetFilesystem(0)
	if err != nil {
		return fmt.Errorf("read filesystem from ISO %q: %w", isoPath, err)
	}
	defer fs.Close()

	isoFS, ok := fs.(*iso9660.FileSystem)
	if !ok {
		return fmt.Errorf("filesystem in %q is not ISO9660", isoPath)
	}

	in, err := openISOFile(isoFS, sourcePath)
	if err != nil {
		return fmt.Errorf("open %q inside ISO %q: %w", sourcePath, isoPath, err)
	}
	defer in.Close()

	out, err := os.OpenFile(destinationPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("create destination file %q: %w", destinationPath, err)
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return fmt.Errorf("copy %s from ISO: %w", sourcePath, err)
	}

	return nil
}

func openISOFile(isoFS *iso9660.FileSystem, sourcePath string) (io.ReadCloser, error) {
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

	resolvedPath, err := resolveISOPathCaseInsensitive(isoFS, sourcePath)
	if err != nil {
		return nil, err
	}

	file, err := isoFS.OpenFile(resolvedPath, os.O_RDONLY)
	if err != nil {
		return nil, err
	}
	return file, nil
}

func resolveISOPathCaseInsensitive(isoFS *iso9660.FileSystem, sourcePath string) (string, error) {
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
