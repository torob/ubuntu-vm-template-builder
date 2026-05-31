package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"

	"ubuntu-vm-template-builder/internal/backend/proxmox"
	"ubuntu-vm-template-builder/internal/backend/qemu"
	"ubuntu-vm-template-builder/internal/backend/vcenter"
	"ubuntu-vm-template-builder/internal/common"
	"ubuntu-vm-template-builder/internal/offlineapt"
)

const (
	commandQEMU                  = "qemu"
	commandVCenter               = "vcenter"
	commandProxmox               = "proxmox"
	commandBuild                 = "build"
	commandUpload                = "upload"
	commandPrerequisites         = "prerequisites"
	commandHardwareConfigExample = "hardware-config-example"
)

type prerequisiteItem struct {
	Name        string
	Description string
	Required    bool
	OK          bool
	Detail      string
	Suggestion  string
}

type prerequisiteReport struct {
	Backend            string
	OS                 qemu.OSInfo
	Items              []prerequisiteItem
	InputPrerequisites []string
}

func main() {
	os.Exit(run(os.Args, os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) < 2 {
		fmt.Fprintln(stderr, "Error: missing command")
		printUsage(stderr, args[0])
		return 1
	}

	switch args[1] {
	case "-h", "--help", "help":
		printUsage(stdout, args[0])
		return 0
	case commandQEMU:
		return runQEMU(args[0], args[2:], stdout, stderr)
	case commandVCenter:
		return runVCenter(args[0], args[2:], stdout, stderr)
	case commandProxmox:
		return runProxmox(args[0], args[2:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "Error: unknown command %q\n", args[1])
		printUsage(stderr, args[0])
		return 1
	}
}

func runProxmox(program string, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "Error: missing proxmox command")
		printProxmoxUsage(stderr, program)
		return 1
	}

	switch args[0] {
	case "-h", "--help", "help":
		printProxmoxUsage(stdout, program)
		return 0
	case commandBuild:
		return runProxmoxBuild(program, args[1:], stdout, stderr)
	case commandHardwareConfigExample:
		return runHardwareConfigExampleCommand(program, commandProxmox, args[1:], stdout, stderr, printProxmoxHardwareConfigExample)
	case commandPrerequisites, "prereqs", "prerequests", "prequests":
		return runPrerequisitesCommand(program, commandProxmox, args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "Error: unknown proxmox command %q\n", args[0])
		printProxmoxUsage(stderr, program)
		return 1
	}
}

func runQEMU(program string, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "Error: missing qemu command")
		printQEMUUsage(stderr, program)
		return 1
	}

	switch args[0] {
	case "-h", "--help", "help":
		printQEMUUsage(stdout, program)
		return 0
	case commandBuild:
		return runQEMUBuild(program, args[1:], stdout, stderr)
	case commandHardwareConfigExample:
		return runHardwareConfigExampleCommand(program, commandQEMU, args[1:], stdout, stderr, printQEMUHardwareConfigExample)
	case commandPrerequisites, "prereqs", "prerequests", "prequests":
		return runPrerequisitesCommand(program, commandQEMU, args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "Error: unknown qemu command %q\n", args[0])
		printQEMUUsage(stderr, program)
		return 1
	}
}

func runQEMUBuild(program string, args []string, stdout, stderr io.Writer) int {
	var (
		isoPath            string
		imagePath          string
		userDataPath       string
		diskFormat         string
		hardwareConfigPath string
		extraPackagesPath  string
	)

	fs := flag.NewFlagSet(commandQEMU+" "+commandBuild, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&isoPath, "iso", "", "Ubuntu ISO file path")
	fs.StringVar(&imagePath, "image", "", "Destination image file path")
	fs.StringVar(&userDataPath, "user-data", "", "cloud-init autoinstall user-data file")
	fs.StringVar(&diskFormat, "disk-format", qemu.DefaultDiskFormat, "Disk image format (raw, vmdk, or qcow2)")
	fs.StringVar(&hardwareConfigPath, "hardware-config", "", "Hardware config YAML file containing disk_size")
	fs.StringVar(&extraPackagesPath, "install-extra-packages", "", "YAML file with apt_url and packages to embed in the installer ISO")
	fs.Usage = func() {
		printQEMUBuildUsage(fs.Output(), program)
	}
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 1
	}

	missing := requiredFlagErrors(map[string]string{
		"image":     imagePath,
		"iso":       isoPath,
		"user-data": userDataPath,
	})
	if len(missing) > 0 {
		fmt.Fprintf(stderr, "Error: missing required flags: %s\n", strings.Join(missing, ", "))
		fs.Usage()
		return 1
	}

	diskFormat = strings.ToLower(strings.TrimSpace(diskFormat))
	if !qemu.ValidateDiskFormat(diskFormat) {
		fmt.Fprintf(stderr, "Error: unsupported --disk-format %q. Supported values: raw, vmdk, qcow2\n", diskFormat)
		return 1
	}

	hardware, err := common.LoadHardwareConfig(hardwareConfigPath)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return 1
	}
	if err := hardware.Validate(); err != nil {
		fmt.Fprintf(stderr, "Error: invalid hardware config: %v\n", err)
		return 1
	}
	userData, displayName, err := loadDisplayUserData(userDataPath)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return 1
	}
	extraPackages, err := offlineapt.LoadConfig(extraPackagesPath)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return 1
	}

	cfg := qemu.Config{
		UbuntuISO:     isoPath,
		ImagePath:     imagePath,
		UserDataPath:  userDataPath,
		UserData:      userData,
		DiskSize:      hardware.DiskSize,
		DiskFormat:    diskFormat,
		DisplayName:   displayName,
		Hardware:      hardware,
		ExtraPackages: extraPackages,
	}

	fmt.Println("Checking prerequisites...")
	if err := qemu.CheckPrerequisites(cfg); err != nil {
		fmt.Fprintf(stderr, "Error: prerequisite check failed: %v\n", err)
		return 1
	}
	fmt.Println("OK prerequisites satisfied")

	installer, err := qemu.NewInstaller(cfg)
	if err != nil {
		fmt.Fprintf(stderr, "Failed to create installer: %v\n", err)
		return 1
	}
	if installer.Install() {
		return 0
	}
	return 1
}

func runVCenter(program string, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "Error: missing vcenter command")
		printVCenterUsage(stderr, program)
		return 1
	}

	switch args[0] {
	case "-h", "--help", "help":
		printVCenterUsage(stdout, program)
		return 0
	case commandBuild:
		return runVCenterBuild(program, args[1:], stdout, stderr)
	case commandUpload:
		return runVCenterUpload(program, args[1:], stdout, stderr)
	case commandHardwareConfigExample:
		return runHardwareConfigExampleCommand(program, commandVCenter, args[1:], stdout, stderr, printVCenterHardwareConfigExample)
	case commandPrerequisites, "prereqs", "prerequests", "prequests":
		return runPrerequisitesCommand(program, commandVCenter, args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "Error: unknown vcenter command %q\n", args[0])
		printVCenterUsage(stderr, program)
		return 1
	}
}

func runVCenterBuild(program string, args []string, stdout, stderr io.Writer) int {
	var (
		isoPath            string
		userDataPath       string
		hardwareConfigPath string
		vcenterHost        string
		vcenterUsername    string
		vcenterPassword    string
		vcenterInsecure    bool
		vcenterDatacenter  string
		vcenterESXiHost    string
		vcenterDatastore   string
		vcenterFolder      string
		vcenterNetwork     string
		templateName       string
		extraPackagesPath  string
	)

	fs := flag.NewFlagSet(commandVCenter+" "+commandBuild, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&isoPath, "iso", "", "Ubuntu ISO file path")
	fs.StringVar(&userDataPath, "user-data", "", "cloud-init autoinstall user-data file")
	fs.StringVar(&hardwareConfigPath, "hardware-config", "", "Hardware config YAML file containing disk_size")
	fs.StringVar(&vcenterHost, "vcenter-host", "", "vCenter hostname or URL")
	fs.StringVar(&vcenterUsername, "vcenter-username", "", "vCenter username")
	fs.StringVar(&vcenterPassword, "vcenter-password", "", "vCenter password")
	fs.BoolVar(&vcenterInsecure, "vcenter-insecure", false, "Skip vCenter TLS certificate verification")
	fs.StringVar(&vcenterDatacenter, "vcenter-datacenter", "", "vCenter datacenter name or inventory path")
	fs.StringVar(&vcenterESXiHost, "vcenter-esxi-host", "", "ESXi host name or inventory path")
	fs.StringVar(&vcenterDatastore, "vcenter-datastore", "", "Datastore name or inventory path")
	fs.StringVar(&vcenterFolder, "vcenter-folder", "", "VM folder name or inventory path")
	fs.StringVar(&vcenterNetwork, "vcenter-network", "", "Network name or inventory path")
	fs.StringVar(&templateName, "template-name", "", "vCenter VM/template name (defaults to user-data hostname)")
	fs.StringVar(&extraPackagesPath, "install-extra-packages", "", "YAML file with apt_url and packages to embed in the installer ISO")
	fs.Usage = func() {
		printVCenterBuildUsage(fs.Output(), program)
	}
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 1
	}

	missing := requiredFlagErrors(map[string]string{
		"iso":                isoPath,
		"user-data":          userDataPath,
		"vcenter-datacenter": vcenterDatacenter,
		"vcenter-datastore":  vcenterDatastore,
		"vcenter-esxi-host":  vcenterESXiHost,
		"vcenter-folder":     vcenterFolder,
		"vcenter-host":       vcenterHost,
		"vcenter-password":   vcenterPassword,
		"vcenter-username":   vcenterUsername,
	})
	if len(missing) > 0 {
		fmt.Fprintf(stderr, "Error: missing required flags: %s\n", strings.Join(missing, ", "))
		fs.Usage()
		return 1
	}

	hardware, err := common.LoadHardwareConfig(hardwareConfigPath)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return 1
	}
	if err := hardware.Validate(); err != nil {
		fmt.Fprintf(stderr, "Error: invalid hardware config: %v\n", err)
		return 1
	}
	effectiveNetwork := effectiveVCenterNetwork(vcenterNetwork, hardware)
	if strings.TrimSpace(effectiveNetwork) == "" {
		fmt.Fprintln(stderr, "Error: missing required vCenter network: pass --vcenter-network or set vcenter.network in --hardware-config")
		fs.Usage()
		return 1
	}
	userData, displayName, err := loadDisplayUserData(userDataPath)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return 1
	}
	if strings.TrimSpace(templateName) == "" {
		templateName = displayName
	}
	extraPackages, err := offlineapt.LoadConfig(extraPackagesPath)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return 1
	}

	cfg := vcenter.Config{
		UbuntuISO:     isoPath,
		UserDataPath:  userDataPath,
		UserData:      userData,
		DiskSize:      hardware.DiskSize,
		DisplayName:   displayName,
		Hardware:      hardware,
		ExtraPackages: extraPackages,
		VCenter: vcenter.ConnectionConfig{
			Host:       vcenterHost,
			Username:   vcenterUsername,
			Password:   vcenterPassword,
			Insecure:   vcenterInsecure,
			Datacenter: vcenterDatacenter,
			ESXiHost:   vcenterESXiHost,
			Datastore:  vcenterDatastore,
			Folder:     vcenterFolder,
			Network:    effectiveNetwork,
			Name:       templateName,
		},
	}

	fmt.Println("Checking prerequisites...")
	if err := vcenter.CheckPrerequisites(cfg); err != nil {
		fmt.Fprintf(stderr, "Error: prerequisite check failed: %v\n", err)
		return 1
	}
	fmt.Println("OK prerequisites satisfied")

	installer, err := vcenter.NewInstaller(cfg)
	if err != nil {
		fmt.Fprintf(stderr, "Failed to create vCenter installer: %v\n", err)
		return 1
	}
	if installer.Install() {
		return 0
	}
	return 1
}

func runVCenterUpload(program string, args []string, stdout, stderr io.Writer) int {
	var (
		sourcePath        string
		destinationPath   string
		overwrite         bool
		vcenterHost       string
		vcenterUsername   string
		vcenterPassword   string
		vcenterInsecure   bool
		vcenterDatacenter string
		vcenterESXiHost   string
		vcenterDatastore  string
	)

	fs := flag.NewFlagSet(commandVCenter+" "+commandUpload, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&sourcePath, "source", "", "Local source file path")
	fs.StringVar(&destinationPath, "destination", "", "Datastore-relative destination path")
	fs.BoolVar(&overwrite, "overwrite", false, "Overwrite destination file if it already exists")
	fs.StringVar(&vcenterHost, "vcenter-host", "", "vCenter hostname or URL")
	fs.StringVar(&vcenterUsername, "vcenter-username", "", "vCenter username")
	fs.StringVar(&vcenterPassword, "vcenter-password", "", "vCenter password")
	fs.BoolVar(&vcenterInsecure, "vcenter-insecure", false, "Skip vCenter TLS certificate verification")
	fs.StringVar(&vcenterDatacenter, "vcenter-datacenter", "", "vCenter datacenter name or inventory path")
	fs.StringVar(&vcenterESXiHost, "vcenter-esxi-host", "", "ESXi host name or inventory path")
	fs.StringVar(&vcenterDatastore, "vcenter-datastore", "", "Datastore name or inventory path")
	fs.Usage = func() {
		printVCenterUploadUsage(fs.Output(), program)
	}
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 1
	}

	missing := requiredFlagErrors(map[string]string{
		"destination":        destinationPath,
		"source":             sourcePath,
		"vcenter-datacenter": vcenterDatacenter,
		"vcenter-datastore":  vcenterDatastore,
		"vcenter-esxi-host":  vcenterESXiHost,
		"vcenter-host":       vcenterHost,
		"vcenter-password":   vcenterPassword,
		"vcenter-username":   vcenterUsername,
	})
	if len(missing) > 0 {
		fmt.Fprintf(stderr, "Error: missing required flags: %s\n", strings.Join(missing, ", "))
		fs.Usage()
		return 1
	}

	cfg := vcenter.UploadConfig{
		SourcePath:      sourcePath,
		DestinationPath: destinationPath,
		Overwrite:       overwrite,
		VCenter: vcenter.ConnectionConfig{
			Host:       vcenterHost,
			Username:   vcenterUsername,
			Password:   vcenterPassword,
			Insecure:   vcenterInsecure,
			Datacenter: vcenterDatacenter,
			ESXiHost:   vcenterESXiHost,
			Datastore:  vcenterDatastore,
		},
	}
	if _, err := vcenter.UploadFileToDatastore(context.Background(), cfg); err != nil {
		fmt.Fprintf(stderr, "Error: vCenter upload failed: %v\n", err)
		return 1
	}
	return 0
}

func runProxmoxBuild(program string, args []string, stdout, stderr io.Writer) int {
	var (
		isoPath            string
		userDataPath       string
		hardwareConfigPath string
		proxmoxHost        string
		proxmoxTokenID     string
		proxmoxTokenSecret string
		proxmoxInsecure    bool
		proxmoxNode        string
		proxmoxISOStorage  string
		proxmoxDiskStorage string
		proxmoxVMID        int
		proxmoxBridge      string
		templateName       string
		extraPackagesPath  string
		optionsPath        string
		cloudInitPath      string
	)

	fs := flag.NewFlagSet(commandProxmox+" "+commandBuild, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&isoPath, "iso", "", "Ubuntu ISO file path")
	fs.StringVar(&userDataPath, "user-data", "", "cloud-init autoinstall user-data file")
	fs.StringVar(&hardwareConfigPath, "hardware-config", "", "Hardware config YAML file containing disk_size")
	fs.StringVar(&proxmoxHost, "proxmox-host", "", "Proxmox VE hostname or URL")
	fs.StringVar(&proxmoxTokenID, "proxmox-token-id", "", "Proxmox API token ID, for example root@pam!builder")
	fs.StringVar(&proxmoxTokenSecret, "proxmox-token-secret", "", "Proxmox API token secret")
	fs.BoolVar(&proxmoxInsecure, "proxmox-insecure", false, "Skip Proxmox TLS certificate verification")
	fs.StringVar(&proxmoxNode, "proxmox-node", "", "Proxmox node name")
	fs.StringVar(&proxmoxISOStorage, "proxmox-iso-storage", "", "Proxmox storage ID used for the temporary installer ISO; must allow iso content")
	fs.StringVar(&proxmoxDiskStorage, "proxmox-disk-storage", "", "Proxmox storage ID used for VM disks and EFI vars; must allow images content")
	fs.IntVar(&proxmoxVMID, "proxmox-vmid", 0, "Proxmox VMID to use (default: allocate the next available ID)")
	fs.StringVar(&proxmoxBridge, "proxmox-bridge", "", "Proxmox network bridge, for example vmbr0")
	fs.StringVar(&templateName, "template-name", "", "Proxmox VM/template name (defaults to user-data hostname)")
	fs.StringVar(&extraPackagesPath, "install-extra-packages", "", "YAML file with apt_url and packages to embed in the installer ISO")
	fs.StringVar(&optionsPath, "options", "", "YAML file with Proxmox VM Options-tab settings")
	fs.StringVar(&cloudInitPath, "cloud-init-options", "", "YAML file with Proxmox Cloud-Init settings")
	fs.Usage = func() {
		printProxmoxBuildUsage(fs.Output(), program)
	}
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 1
	}

	missing := requiredFlagErrors(map[string]string{
		"iso":                  isoPath,
		"proxmox-disk-storage": proxmoxDiskStorage,
		"proxmox-host":         proxmoxHost,
		"proxmox-iso-storage":  proxmoxISOStorage,
		"proxmox-node":         proxmoxNode,
		"proxmox-token-id":     proxmoxTokenID,
		"proxmox-token-secret": proxmoxTokenSecret,
		"user-data":            userDataPath,
	})
	if len(missing) > 0 {
		fmt.Fprintf(stderr, "Error: missing required flags: %s\n", strings.Join(missing, ", "))
		fs.Usage()
		return 1
	}
	if proxmoxVMID < 0 {
		fmt.Fprintln(stderr, "Error: --proxmox-vmid must be greater than zero when set")
		return 1
	}

	hardware, err := common.LoadHardwareConfig(hardwareConfigPath)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return 1
	}
	if err := hardware.Validate(); err != nil {
		fmt.Fprintf(stderr, "Error: invalid hardware config: %v\n", err)
		return 1
	}
	effectiveBridge := effectiveProxmoxBridge(proxmoxBridge, hardware)
	if strings.TrimSpace(effectiveBridge) == "" {
		fmt.Fprintln(stderr, "Error: missing required Proxmox bridge: pass --proxmox-bridge or set proxmox.bridge in --hardware-config")
		fs.Usage()
		return 1
	}
	userData, displayName, err := loadDisplayUserData(userDataPath)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return 1
	}
	if strings.TrimSpace(templateName) == "" {
		templateName = displayName
	}
	extraPackages, err := offlineapt.LoadConfig(extraPackagesPath)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return 1
	}
	options, err := proxmox.LoadOptionsConfig(optionsPath)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return 1
	}
	cloudInitOptions, err := proxmox.LoadCloudInitOptionsConfig(cloudInitPath)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return 1
	}

	cfg := proxmox.Config{
		UbuntuISO:        isoPath,
		UserDataPath:     userDataPath,
		UserData:         userData,
		DiskSize:         hardware.DiskSize,
		DisplayName:      displayName,
		Hardware:         hardware,
		ExtraPackages:    extraPackages,
		Options:          options,
		CloudInitOptions: cloudInitOptions,
		Proxmox: proxmox.ConnectionConfig{
			Host:        proxmoxHost,
			TokenID:     proxmoxTokenID,
			TokenSecret: proxmoxTokenSecret,
			Insecure:    proxmoxInsecure,
			Node:        proxmoxNode,
			ISOStorage:  proxmoxISOStorage,
			DiskStorage: proxmoxDiskStorage,
			VMID:        proxmoxVMID,
			Bridge:      effectiveBridge,
			Name:        templateName,
		},
	}

	fmt.Println("Checking prerequisites...")
	if err := proxmox.CheckPrerequisites(cfg); err != nil {
		fmt.Fprintf(stderr, "Error: prerequisite check failed: %v\n", err)
		return 1
	}
	fmt.Println("OK prerequisites satisfied")

	installer, err := proxmox.NewInstaller(cfg)
	if err != nil {
		fmt.Fprintf(stderr, "Failed to create Proxmox installer: %v\n", err)
		return 1
	}
	if installer.Install() {
		return 0
	}
	return 1
}

func runHardwareConfigExampleCommand(program, backend string, args []string, stdout, stderr io.Writer, printExample func(io.Writer)) int {
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help") {
		printHardwareConfigExampleUsage(stdout, program, backend)
		return 0
	}
	if len(args) > 0 {
		fmt.Fprintf(stderr, "Error: %s %s does not accept arguments\n", backend, commandHardwareConfigExample)
		printHardwareConfigExampleUsage(stderr, program, backend)
		return 1
	}

	printExample(stdout)
	return 0
}

func effectiveVCenterNetwork(cliNetwork string, hardware common.HardwareConfig) string {
	if network := strings.TrimSpace(cliNetwork); network != "" {
		return network
	}
	return strings.TrimSpace(hardware.VCenter.Network)
}

func effectiveProxmoxBridge(cliBridge string, hardware common.HardwareConfig) string {
	if bridge := strings.TrimSpace(cliBridge); bridge != "" {
		return bridge
	}
	return strings.TrimSpace(hardware.Proxmox.Bridge)
}

func loadDisplayUserData(userDataPath string) ([]byte, string, error) {
	userData, hostname, err := common.LoadUserData(userDataPath)
	if err != nil {
		return nil, "", err
	}
	displayName := hostname
	if strings.TrimSpace(displayName) == "" {
		displayName = common.FallbackName
	}
	return userData, displayName, nil
}

func requiredFlagErrors(values map[string]string) []string {
	var missing []string
	for name, value := range values {
		if strings.TrimSpace(value) == "" {
			missing = append(missing, "--"+name)
		}
	}
	sort.Strings(missing)
	return missing
}

func printUsage(out io.Writer, program string) {
	fmt.Fprintf(out, "Usage: %s <command> [options]\n\n", program)
	fmt.Fprintln(out, "Commands:")
	fmt.Fprintln(out, "  qemu")
	fmt.Fprintln(out, "        Build a local QEMU/KVM disk image")
	fmt.Fprintln(out, "  vcenter")
	fmt.Fprintln(out, "        Build a vCenter VM or template on a selected ESXi host")
	fmt.Fprintln(out, "  proxmox")
	fmt.Fprintln(out, "        Build a Proxmox VE VM or template on a selected node")
	fmt.Fprintln(out)
	fmt.Fprintf(out, "Run %s <command> --help for command-specific flags.\n", program)
}

func printQEMUUsage(out io.Writer, program string) {
	fmt.Fprintf(out, "Usage: %s qemu <command> [options]\n\n", program)
	fmt.Fprintln(out, "Commands:")
	fmt.Fprintf(out, "  %s\n", commandBuild)
	fmt.Fprintln(out, "        Build a local QEMU/KVM disk image")
	fmt.Fprintf(out, "  %s\n", commandHardwareConfigExample)
	fmt.Fprintln(out, "        Print an example QEMU hardware config YAML file")
	fmt.Fprintf(out, "  %s\n", commandPrerequisites)
	fmt.Fprintln(out, "        Print QEMU backend host prerequisites and installation suggestions")
	fmt.Fprintln(out, "        Aliases: prereqs, prerequests, prequests")
	fmt.Fprintln(out)
	fmt.Fprintf(out, "Run %s qemu <command> --help for command-specific flags.\n", program)
}

func printQEMUBuildUsage(out io.Writer, program string) {
	fmt.Fprintf(out, "Usage: %s qemu %s --iso ubuntu.iso --image output.img --user-data autoinstall.yaml [--disk-format raw|qcow2|vmdk] --hardware-config hardware.yaml\n\n", program, commandBuild)
	fmt.Fprintln(out, "Options:")
	fmt.Fprintln(out, "  --iso string")
	fmt.Fprintln(out, "        Ubuntu ISO file path")
	fmt.Fprintln(out, "  --image string")
	fmt.Fprintln(out, "        Destination image file path")
	fmt.Fprintln(out, "  --user-data string")
	fmt.Fprintln(out, "        cloud-init autoinstall user-data file")
	fmt.Fprintln(out, "  --disk-format string")
	fmt.Fprintln(out, "        Disk image format: raw, qcow2, or vmdk (default \"raw\")")
	fmt.Fprintln(out, "  --hardware-config string")
	fmt.Fprintln(out, "        Hardware config YAML file containing disk_size and hardware settings")
	fmt.Fprintln(out, "  --install-extra-packages string")
	fmt.Fprintln(out, "        YAML file with apt_url and packages to embed in the installer ISO")
}

func printVCenterUsage(out io.Writer, program string) {
	fmt.Fprintf(out, "Usage: %s vcenter <command> [options]\n\n", program)
	fmt.Fprintln(out, "Commands:")
	fmt.Fprintf(out, "  %s\n", commandBuild)
	fmt.Fprintln(out, "        Build a vCenter VM or template on a selected ESXi host")
	fmt.Fprintf(out, "  %s\n", commandUpload)
	fmt.Fprintln(out, "        Upload a local file to a selected vCenter datastore")
	fmt.Fprintf(out, "  %s\n", commandHardwareConfigExample)
	fmt.Fprintln(out, "        Print an example vCenter hardware config YAML file")
	fmt.Fprintf(out, "  %s\n", commandPrerequisites)
	fmt.Fprintln(out, "        Print vCenter backend host prerequisites and installation suggestions")
	fmt.Fprintln(out, "        Aliases: prereqs, prerequests, prequests")
	fmt.Fprintln(out)
	fmt.Fprintf(out, "Run %s vcenter <command> --help for command-specific flags.\n", program)
}

func printVCenterBuildUsage(out io.Writer, program string) {
	fmt.Fprintf(out, "Usage: %s vcenter %s --iso ubuntu.iso --user-data autoinstall.yaml --vcenter-host vc.example.com --vcenter-username user --vcenter-password pass --vcenter-datacenter DC --vcenter-esxi-host esxi.example.com --vcenter-datastore datastore1 --vcenter-folder /DC/vm/Templates --vcenter-network 'VM Network' [--template-name ubuntu-template] --hardware-config hardware.yaml\n\n", program, commandBuild)
	fmt.Fprintln(out, "Options:")
	fmt.Fprintln(out, "  --iso string")
	fmt.Fprintln(out, "        Ubuntu ISO file path")
	fmt.Fprintln(out, "  --user-data string")
	fmt.Fprintln(out, "        cloud-init autoinstall user-data file")
	fmt.Fprintln(out, "  --hardware-config string")
	fmt.Fprintln(out, "        Hardware config YAML file containing disk_size and hardware settings")
	fmt.Fprintln(out, "  --vcenter-host string")
	fmt.Fprintln(out, "        vCenter hostname or URL")
	fmt.Fprintln(out, "  --vcenter-username string")
	fmt.Fprintln(out, "        vCenter username")
	fmt.Fprintln(out, "  --vcenter-password string")
	fmt.Fprintln(out, "        vCenter password")
	fmt.Fprintln(out, "  --vcenter-insecure")
	fmt.Fprintln(out, "        Skip vCenter TLS certificate verification")
	fmt.Fprintln(out, "  --vcenter-datacenter string")
	fmt.Fprintln(out, "        Datacenter name or inventory path")
	fmt.Fprintln(out, "  --vcenter-esxi-host string")
	fmt.Fprintln(out, "        ESXi host name or inventory path")
	fmt.Fprintln(out, "  --vcenter-datastore string")
	fmt.Fprintln(out, "        Datastore name or inventory path")
	fmt.Fprintln(out, "  --vcenter-folder string")
	fmt.Fprintln(out, "        VM folder name or inventory path")
	fmt.Fprintln(out, "  --vcenter-network string")
	fmt.Fprintln(out, "        Network name or inventory path")
	fmt.Fprintln(out, "  --template-name string")
	fmt.Fprintln(out, "        vCenter VM/template name (defaults to user-data hostname)")
	fmt.Fprintln(out, "  --install-extra-packages string")
	fmt.Fprintln(out, "        YAML file with apt_url and packages to embed in the installer ISO")
}

func printVCenterUploadUsage(out io.Writer, program string) {
	fmt.Fprintf(out, "Usage: %s vcenter %s --source local.file --destination uploads/local.file --vcenter-host vc.example.com --vcenter-username user --vcenter-password pass --vcenter-datacenter DC --vcenter-esxi-host esxi.example.com --vcenter-datastore datastore1 [--overwrite]\n\n", program, commandUpload)
	fmt.Fprintln(out, "Options:")
	fmt.Fprintln(out, "  --source string")
	fmt.Fprintln(out, "        Local source file path")
	fmt.Fprintln(out, "  --destination string")
	fmt.Fprintln(out, "        Datastore-relative destination path, for example uploads/local.file")
	fmt.Fprintln(out, "  --overwrite")
	fmt.Fprintln(out, "        Overwrite destination file if it already exists")
	fmt.Fprintln(out, "  --vcenter-host string")
	fmt.Fprintln(out, "        vCenter hostname or URL")
	fmt.Fprintln(out, "  --vcenter-username string")
	fmt.Fprintln(out, "        vCenter username")
	fmt.Fprintln(out, "  --vcenter-password string")
	fmt.Fprintln(out, "        vCenter password")
	fmt.Fprintln(out, "  --vcenter-insecure")
	fmt.Fprintln(out, "        Skip vCenter TLS certificate verification")
	fmt.Fprintln(out, "  --vcenter-datacenter string")
	fmt.Fprintln(out, "        Datacenter name or inventory path")
	fmt.Fprintln(out, "  --vcenter-esxi-host string")
	fmt.Fprintln(out, "        ESXi host name or inventory path")
	fmt.Fprintln(out, "  --vcenter-datastore string")
	fmt.Fprintln(out, "        Datastore name or inventory path")
}

func printProxmoxUsage(out io.Writer, program string) {
	fmt.Fprintf(out, "Usage: %s proxmox <command> [options]\n\n", program)
	fmt.Fprintln(out, "Commands:")
	fmt.Fprintf(out, "  %s\n", commandBuild)
	fmt.Fprintln(out, "        Build a Proxmox VE VM or template on a selected node")
	fmt.Fprintf(out, "  %s\n", commandHardwareConfigExample)
	fmt.Fprintln(out, "        Print an example Proxmox hardware config YAML file")
	fmt.Fprintf(out, "  %s\n", commandPrerequisites)
	fmt.Fprintln(out, "        Print Proxmox backend host prerequisites and installation suggestions")
	fmt.Fprintln(out, "        Aliases: prereqs, prerequests, prequests")
	fmt.Fprintln(out)
	fmt.Fprintf(out, "Run %s proxmox <command> --help for command-specific flags.\n", program)
}

func printProxmoxBuildUsage(out io.Writer, program string) {
	fmt.Fprintf(out, "Usage: %s proxmox %s --iso ubuntu.iso --user-data autoinstall.yaml --proxmox-host pve.example.com --proxmox-token-id 'root@pam!builder' --proxmox-token-secret secret --proxmox-node pve1 --proxmox-iso-storage local --proxmox-disk-storage vms --proxmox-bridge vmbr0 [--template-name ubuntu-template] [--options options.yaml] [--cloud-init-options cloud-init.yaml] --hardware-config hardware.yaml\n\n", program, commandBuild)
	fmt.Fprintln(out, "Options:")
	fmt.Fprintln(out, "  --iso string")
	fmt.Fprintln(out, "        Ubuntu ISO file path")
	fmt.Fprintln(out, "  --user-data string")
	fmt.Fprintln(out, "        cloud-init autoinstall user-data file")
	fmt.Fprintln(out, "  --hardware-config string")
	fmt.Fprintln(out, "        Hardware config YAML file containing disk_size and hardware settings")
	fmt.Fprintln(out, "  --proxmox-host string")
	fmt.Fprintln(out, "        Proxmox VE hostname or URL")
	fmt.Fprintln(out, "  --proxmox-token-id string")
	fmt.Fprintln(out, "        Proxmox API token ID, for example root@pam!builder")
	fmt.Fprintln(out, "  --proxmox-token-secret string")
	fmt.Fprintln(out, "        Proxmox API token secret")
	fmt.Fprintln(out, "  --proxmox-insecure")
	fmt.Fprintln(out, "        Skip Proxmox TLS certificate verification")
	fmt.Fprintln(out, "  --proxmox-node string")
	fmt.Fprintln(out, "        Proxmox node name")
	fmt.Fprintln(out, "  --proxmox-iso-storage string")
	fmt.Fprintln(out, "        Proxmox storage ID used for the temporary installer ISO; must allow iso content")
	fmt.Fprintln(out, "  --proxmox-disk-storage string")
	fmt.Fprintln(out, "        Proxmox storage ID used for VM disks and EFI vars; must allow images content")
	fmt.Fprintln(out, "  --proxmox-vmid int")
	fmt.Fprintln(out, "        Proxmox VMID to use (default: allocate the next available ID)")
	fmt.Fprintln(out, "  --proxmox-bridge string")
	fmt.Fprintln(out, "        Proxmox network bridge, for example vmbr0")
	fmt.Fprintln(out, "  --template-name string")
	fmt.Fprintln(out, "        Proxmox VM/template name (defaults to user-data hostname)")
	fmt.Fprintln(out, "  --install-extra-packages string")
	fmt.Fprintln(out, "        YAML file with apt_url and packages to embed in the installer ISO")
	fmt.Fprintln(out, "  --options string")
	fmt.Fprintln(out, "        YAML file with Proxmox VM Options-tab settings")
	fmt.Fprintln(out, "  --cloud-init-options string")
	fmt.Fprintln(out, "        YAML file with Proxmox Cloud-Init settings")
}

func printHardwareConfigExampleUsage(out io.Writer, program, backend string) {
	fmt.Fprintf(out, "Usage: %s %s %s\n\n", program, backend, commandHardwareConfigExample)
	fmt.Fprintln(out, "Print a complete example hardware config YAML file for this backend.")
	fmt.Fprintln(out, "Redirect the output to a file and pass it back with --hardware-config.")
}

func printQEMUHardwareConfigExample(out io.Writer) {
	fmt.Fprintln(out, "# QEMU hardware config example")
	fmt.Fprintln(out, "# boot_firmware: uefi or bios")
	fmt.Fprintln(out, "boot_firmware: uefi")
	fmt.Fprintln(out, "# disk_size is required and supports suffixes such as M, G, or T")
	fmt.Fprintln(out, "disk_size: 20G")
	fmt.Fprintln(out, "# vcpu must be greater than zero")
	fmt.Fprintln(out, "vcpu: 2")
	fmt.Fprintln(out, "# memory_mb must be greater than zero")
	fmt.Fprintln(out, "memory_mb: 2048")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "qemu:")
	fmt.Fprintln(out, "  # cpu_model is passed to qemu-system-x86_64 -cpu")
	fmt.Fprintln(out, "  cpu_model: host")
	fmt.Fprintln(out, "  # disk_interface: virtio, ide, scsi, or sata")
	fmt.Fprintln(out, "  disk_interface: virtio")
	fmt.Fprintln(out, "  # iso_interface: virtio, ide, scsi, or sata")
	fmt.Fprintln(out, "  iso_interface: virtio")
}

func printVCenterHardwareConfigExample(out io.Writer) {
	fmt.Fprintln(out, "# vCenter hardware config example")
	fmt.Fprintln(out, "# boot_firmware: uefi or bios")
	fmt.Fprintln(out, "boot_firmware: uefi")
	fmt.Fprintln(out, "# disk_size is required and supports suffixes such as M, G, or T")
	fmt.Fprintln(out, "disk_size: 20G")
	fmt.Fprintln(out, "# vcpu must be greater than zero")
	fmt.Fprintln(out, "vcpu: 2")
	fmt.Fprintln(out, "# memory_mb must be greater than zero")
	fmt.Fprintln(out, "memory_mb: 2048")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "vcenter:")
	fmt.Fprintln(out, "  # scsi_controller: pvscsi, lsilogic, buslogic, or lsilogic-sas")
	fmt.Fprintln(out, "  scsi_controller: pvscsi")
	fmt.Fprintln(out, "  # network_adapter: vmxnet3, vmxnet2, vmxnet, e1000, or e1000e")
	fmt.Fprintln(out, "  network_adapter: vmxnet3")
	fmt.Fprintln(out, "  # network is the vCenter network name or inventory path. --vcenter-network overrides this value.")
	fmt.Fprintln(out, "  network: VM Network")
	fmt.Fprintln(out, "  # disk_provisioning: thin, thick_provision_lazy_zeroed, or thick_provision_eager_zeroed")
	fmt.Fprintln(out, "  disk_provisioning: thick_provision_lazy_zeroed")
	fmt.Fprintln(out, "  # compatibility is optional; empty lets vCenter choose the default hardware version. Example: vmx-21")
	fmt.Fprintln(out, "  compatibility: \"\"")
	fmt.Fprintln(out, "  # guest_os_id is the vSphere guest OS identifier")
	fmt.Fprintln(out, "  guest_os_id: ubuntu64Guest")
	fmt.Fprintln(out, "  # reserve_all_guest_memory reserves memory_mb for this VM in vCenter")
	fmt.Fprintln(out, "  reserve_all_guest_memory: false")
	fmt.Fprintln(out, "  # output_type: template or vm")
	fmt.Fprintln(out, "  output_type: template")
}

func printProxmoxHardwareConfigExample(out io.Writer) {
	fmt.Fprintln(out, "# Proxmox hardware config example")
	fmt.Fprintln(out, "# boot_firmware: uefi or bios")
	fmt.Fprintln(out, "boot_firmware: uefi")
	fmt.Fprintln(out, "# disk_size is required and supports suffixes such as M, G, or T")
	fmt.Fprintln(out, "disk_size: 20G")
	fmt.Fprintln(out, "# vcpu must be greater than zero")
	fmt.Fprintln(out, "vcpu: 2")
	fmt.Fprintln(out, "# memory_mb must be greater than zero")
	fmt.Fprintln(out, "memory_mb: 2048")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "proxmox:")
	fmt.Fprintln(out, "  # bridge is the Proxmox bridge name. --proxmox-bridge overrides this value.")
	fmt.Fprintln(out, "  bridge: vmbr0")
	fmt.Fprintln(out, "  # network_adapter: virtio, e1000, e1000e, rtl8139, or vmxnet3")
	fmt.Fprintln(out, "  network_adapter: virtio")
	fmt.Fprintln(out, "  # scsi_controller: virtio-scsi-pci, virtio-scsi-single, lsi, lsi53c810, megasas, or pvscsi")
	fmt.Fprintln(out, "  scsi_controller: virtio-scsi-pci")
	fmt.Fprintln(out, "  # disk_interface: scsi, sata, virtio, or ide")
	fmt.Fprintln(out, "  disk_interface: scsi")
	fmt.Fprintln(out, "  # disk_format: raw, qcow2, or vmdk")
	fmt.Fprintln(out, "  disk_format: raw")
	fmt.Fprintln(out, "  # disk_io_thread enables Proxmox iothread=1 for the main disk; requires virtio disk_interface or scsi with virtio-scsi-single")
	fmt.Fprintln(out, "  disk_io_thread: false")
	fmt.Fprintln(out, "  # cpu_type is passed to Proxmox as the VM CPU type")
	fmt.Fprintln(out, "  cpu_type: host")
	fmt.Fprintln(out, "  # machine is the Proxmox machine type")
	fmt.Fprintln(out, "  machine: q35")
	fmt.Fprintln(out, "  # ostype is the Proxmox guest OS type")
	fmt.Fprintln(out, "  ostype: l26")
	fmt.Fprintln(out, "  # efi_type applies when boot_firmware is uefi: 2m or 4m")
	fmt.Fprintln(out, "  efi_type: 4m")
	fmt.Fprintln(out, "  # pre_enrolled_keys controls secure boot keys for the Proxmox EFI disk")
	fmt.Fprintln(out, "  pre_enrolled_keys: false")
	fmt.Fprintln(out, "  # output_type: template or vm")
	fmt.Fprintln(out, "  output_type: template")
}

func printPrerequisitesUsage(out io.Writer, program, backend string) {
	fmt.Fprintf(out, "Usage: %s %s %s\n\n", program, backend, commandPrerequisites)
	fmt.Fprintln(out, "Aliases: prereqs, prerequests, prequests")
	fmt.Fprintln(out)
	fmt.Fprintf(out, "Print %s backend host prerequisites, check whether required host tools are available,\n", backend)
	fmt.Fprintln(out, "and show OS-specific installation or permission suggestions when something is missing.")
}

func collectQEMUPrerequisites() prerequisiteReport {
	osInfo := qemu.DetectOSInfo()
	installCmd := qemu.InstallSuggestion(osInfo)

	items := []prerequisiteItem{
		{
			Name:        "Linux host",
			Description: "Required because the qemu backend launches QEMU with KVM through /dev/kvm.",
			Required:    true,
			OK:          osInfo.GOOS == "linux",
			Detail:      osInfo.DisplayName(),
			Suggestion:  "Run qemu builds on a Linux host with KVM support.",
		},
		commandPrerequisite("qemu-system-x86_64", "QEMU system emulator used by the qemu backend.", true, installCmd),
		commandPrerequisite("xorriso", "ISO remastering tool used by the qemu backend to inject builder support scripts.", true, xorrisoInstallSuggestion(osInfo)),
		commandPrerequisite("qemu-img", "QEMU image tool used when creating qcow2 or vmdk images.", false, installCmd),
		ovmfPrerequisite(osInfo),
		kvmPrerequisite(osInfo),
	}

	return prerequisiteReport{
		Backend: "QEMU",
		OS:      osInfo,
		Items:   items,
		InputPrerequisites: []string{
			"Ubuntu live-server ISO containing /casper/vmlinuz and /casper/initrd",
			"cloud-init autoinstall user-data YAML with a top-level autoinstall mapping",
			"UEFI-compatible user-data with an ESP and fallback bootloader command when hardware boot_firmware is uefi",
			"destination image path that does not already exist",
			"writable destination image directory",
			"hardware config YAML with disk_size set to a valid size such as 20G",
			"--install-extra-packages config with apt_url and packages; when used, host tool apt-get and the Ubuntu archive keyring are checked during the build",
		},
	}
}

func collectVCenterPrerequisites() prerequisiteReport {
	osInfo := qemu.DetectOSInfo()

	items := []prerequisiteItem{
		commandPrerequisite("xorriso", "ISO remastering tool used by the vcenter backend to inject autoinstall seed data and builder support scripts.", true, xorrisoInstallSuggestion(osInfo)),
	}

	return prerequisiteReport{
		Backend: "vCenter",
		OS:      osInfo,
		Items:   items,
		InputPrerequisites: []string{
			"Ubuntu live-server ISO containing /casper/vmlinuz and /casper/initrd",
			"cloud-init autoinstall user-data YAML with a top-level autoinstall mapping",
			"valid vCenter connection and placement flags",
			"target ESXi host with access to the selected datastore and network",
			"hardware config YAML with disk_size set to a valid size such as 20G",
			"--install-extra-packages config with apt_url and packages; when used, host tool apt-get and the Ubuntu archive keyring are checked during the build",
		},
	}
}

func collectProxmoxPrerequisites() prerequisiteReport {
	osInfo := qemu.DetectOSInfo()

	items := []prerequisiteItem{
		commandPrerequisite("xorriso", "ISO remastering tool used by the proxmox backend to inject autoinstall seed data and builder support scripts.", true, proxmox.XorrisoInstallSuggestion(osInfo)),
	}

	return prerequisiteReport{
		Backend: "Proxmox",
		OS:      osInfo,
		Items:   items,
		InputPrerequisites: []string{
			"Ubuntu live-server ISO containing /casper/vmlinuz and /casper/initrd",
			"cloud-init autoinstall user-data YAML with a top-level autoinstall mapping",
			"valid Proxmox VE API-token connection flags",
			"target Proxmox node with access to the selected storages and bridge",
			"--proxmox-iso-storage that allows iso content and has enough free space for the remastered installer ISO",
			"--proxmox-disk-storage that allows images content and has enough free space for the VM disk, EFI vars, and optional Cloud-Init drive",
			"hardware config YAML with disk_size set to a valid size such as 20G",
			"optional --options and --cloud-init-options YAML files with supported Proxmox settings only",
			"--install-extra-packages config with apt_url and packages; when used, host tool apt-get and the Ubuntu archive keyring are checked during the build",
		},
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

func ovmfPrerequisite(osInfo qemu.OSInfo) prerequisiteItem {
	firmware, err := qemu.FindOVMFFirmware()
	item := prerequisiteItem{
		Name:        "OVMF UEFI firmware",
		Description: "Required for qemu builds when hardware boot_firmware is uefi. Not required for boot_firmware: bios.",
		Required:    true,
		OK:          err == nil,
		Suggestion:  qemu.OVMFInstallSuggestion(osInfo),
	}
	if err == nil {
		item.Detail = fmt.Sprintf("CODE: %s; VARS: %s", firmware.CodePath, firmware.VarsPath)
		return item
	}

	item.Detail = err.Error()
	return item
}

func kvmPrerequisite(osInfo qemu.OSInfo) prerequisiteItem {
	err := qemu.CheckKVMAccess()
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
	if strings.Contains(err.Error(), "not found") {
		item.Suggestion = "Enable virtualization in BIOS/UEFI, load the KVM module with sudo modprobe kvm_intel or sudo modprobe kvm_amd, and install QEMU packages."
		return item
	}
	item.Suggestion = "Grant access with sudo usermod -aG kvm $USER and start a new login shell; for the current session, sudo setfacl -m u:$USER:rw /dev/kvm can be used."
	return item
}

func xorrisoInstallSuggestion(osInfo qemu.OSInfo) string {
	if osInfo.GOOS != "linux" {
		return "Install xorriso from the host operating system package manager before using build backends that remaster installer ISOs."
	}
	return vcenter.XorrisoInstallSuggestion(osInfo)
}

func (r prerequisiteReport) RequiredOK() bool {
	for _, item := range r.Items {
		if item.Required && !item.OK {
			return false
		}
	}
	return true
}

func runPrerequisitesCommand(program, backend string, args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help") {
		printPrerequisitesUsage(stdout, program, backend)
		return 0
	}
	if len(args) > 0 {
		fmt.Fprintf(stderr, "Error: %s %s does not accept arguments\n", backend, commandPrerequisites)
		printPrerequisitesUsage(stderr, program, backend)
		return 1
	}

	var report prerequisiteReport
	switch backend {
	case commandQEMU:
		report = collectQEMUPrerequisites()
	case commandVCenter:
		report = collectVCenterPrerequisites()
	case commandProxmox:
		report = collectProxmoxPrerequisites()
	default:
		fmt.Fprintf(stderr, "Error: unknown backend %q\n", backend)
		return 1
	}

	printPrerequisiteReport(stdout, report)
	if !report.RequiredOK() {
		return 1
	}
	return 0
}

func printPrerequisiteReport(out io.Writer, report prerequisiteReport) {
	if report.Backend == "" {
		fmt.Fprintln(out, "Prerequisites")
	} else {
		fmt.Fprintf(out, "%s prerequisites\n", report.Backend)
	}
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

	if len(report.InputPrerequisites) > 0 {
		fmt.Fprintln(out)
		if report.Backend == "" {
			fmt.Fprintln(out, "Install input prerequisites checked during normal install runs:")
		} else {
			fmt.Fprintf(out, "%s install input prerequisites checked during normal install runs:\n", report.Backend)
		}
		for _, input := range report.InputPrerequisites {
			fmt.Fprintf(out, "- %s\n", input)
		}
	}

	fmt.Fprintln(out)
	if report.RequiredOK() {
		if report.Backend == "" {
			fmt.Fprintln(out, "Required host prerequisites are satisfied.")
		} else {
			fmt.Fprintf(out, "Required %s host prerequisites are satisfied.\n", report.Backend)
		}
		return
	}
	if report.Backend == "" {
		fmt.Fprintln(out, "One or more required host prerequisites are missing.")
		return
	}
	fmt.Fprintf(out, "One or more required %s host prerequisites are missing.\n", report.Backend)
}
