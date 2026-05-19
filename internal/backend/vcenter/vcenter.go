package vcenter

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/vmware/govmomi"
	"github.com/vmware/govmomi/find"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"

	"ubuntu-vm-template-builder/internal/common"
)

const interruptCleanupTimeout = 2 * time.Minute

type Config struct {
	UbuntuISO    string
	UserDataPath string
	UserData     []byte
	DiskSize     string
	DisplayName  string
	Hardware     common.HardwareConfig
	VCenter      ConnectionConfig
}

type UploadConfig struct {
	SourcePath      string
	DestinationPath string
	Overwrite       bool
	VCenter         ConnectionConfig
}

type UploadResult struct {
	SourcePath      string
	DestinationPath string
	DatastorePath   string
	Bytes           int64
	Overwrite       bool
}

type ConnectionConfig struct {
	Host       string
	Username   string
	Password   string
	Insecure   bool
	Datacenter string
	ESXiHost   string
	Datastore  string
	Folder     string
	Network    string
	Name       string
}

type Installer struct {
	cfg     Config
	tempDir string
}

type vCenterBuildState struct {
	cfg                     ConnectionConfig
	client                  *vim25.Client
	placement               *placement
	vm                      *object.VirtualMachine
	remoteISOPath           string
	datastoreISOPath        string
	remoteConsoleLogPath    string
	datastoreConsoleLogPath string
	vmCreated               bool
	isoDeleted              bool
	consoleLogDeleted       bool
	completed               bool
}

type placement struct {
	Datacenter   *object.Datacenter
	Host         *object.HostSystem
	Datastore    *object.Datastore
	Folder       *object.Folder
	Network      object.NetworkReference
	ResourcePool *object.ResourcePool
}

func NewInstaller(cfg Config) (*Installer, error) {
	cfg.Hardware = cfg.Hardware.Normalize()
	if err := cfg.Hardware.Validate(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(cfg.DisplayName) == "" {
		cfg.DisplayName = common.FallbackName
	}
	cfg.VCenter = normalizeConnectionConfig(cfg.VCenter, cfg.DisplayName)

	return &Installer{cfg: cfg}, nil
}

func normalizeConnectionConfig(cfg ConnectionConfig, fallback string) ConnectionConfig {
	if strings.TrimSpace(cfg.Name) == "" {
		cfg.Name = fallback
	}
	if strings.TrimSpace(cfg.Name) == "" {
		cfg.Name = common.FallbackName
	}
	return cfg
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
	if _, err := exec.LookPath("xorriso"); err != nil {
		return errors.New("missing required dependency for vcenter backend: xorriso")
	}
	if err := common.CheckISOFile(cfg.UbuntuISO); err != nil {
		return err
	}
	if _, err := common.ParseDiskSize(cfg.DiskSize); err != nil {
		return err
	}
	if _, err := TransformUserData(cfg.UserData); err != nil {
		return fmt.Errorf("prepare vCenter seed user-data: %w", err)
	}
	return nil
}

func UploadFileToDatastore(ctx context.Context, cfg UploadConfig) (UploadResult, error) {
	sourcePath := strings.TrimSpace(cfg.SourcePath)
	sourceInfo, err := os.Stat(sourcePath)
	if err != nil {
		return UploadResult{}, fmt.Errorf("source file %q: %w", sourcePath, err)
	}
	if !sourceInfo.Mode().IsRegular() {
		return UploadResult{}, fmt.Errorf("source file %q is not a regular file", sourcePath)
	}

	remotePath, err := normalizeUploadDestinationPath(cfg.DestinationPath)
	if err != nil {
		return UploadResult{}, err
	}

	client, placement, err := connectAndResolveUploadPlacement(ctx, cfg.VCenter)
	if err != nil {
		return UploadResult{}, err
	}
	defer client.Logout(context.Background())

	datastorePath := placement.Datastore.Path(remotePath)
	fmt.Printf("Upload source: %s (%d bytes)\n", sourcePath, sourceInfo.Size())
	fmt.Printf("Upload destination: %s\n", datastorePath)
	fmt.Printf("Overwrite existing destination: %t\n", cfg.Overwrite)

	if !cfg.Overwrite {
		exists, err := datastoreFileExists(ctx, placement, remotePath)
		if err != nil {
			return UploadResult{}, fmt.Errorf("check destination %q: %w", datastorePath, err)
		}
		if exists {
			return UploadResult{}, fmt.Errorf("destination already exists: %s (pass --overwrite to replace it)", datastorePath)
		}
	}

	if err := ensureDatastoreParentDirectory(ctx, placement, remotePath); err != nil {
		return UploadResult{}, err
	}

	fmt.Printf("Uploading file to datastore: %s\n", datastorePath)
	if err := uploadDatastoreFile(ctx, placement, sourcePath, remotePath); err != nil {
		return UploadResult{}, fmt.Errorf("upload file to datastore: %w", err)
	}
	fmt.Printf("OK file uploaded: %s\n", datastorePath)
	return UploadResult{
		SourcePath:      sourcePath,
		DestinationPath: remotePath,
		DatastorePath:   datastorePath,
		Bytes:           sourceInfo.Size(),
		Overwrite:       cfg.Overwrite,
	}, nil
}

func (i *Installer) Install() bool {
	outputType := common.NormalizeVCenterOutputType(i.cfg.Hardware.VCenter.OutputType)
	outputNoun := vCenterOutputNoun(outputType)
	outputField := vCenterOutputField(outputType)

	fmt.Printf("\n%s\n", strings.Repeat("=", 60))
	fmt.Printf("Installing: %s -> vCenter %s %s\n", i.cfg.DisplayName, outputNoun, i.cfg.VCenter.Name)
	fmt.Printf("Boot firmware: %s\n", common.NormalizeBootFirmware(i.cfg.Hardware.BootFirmware))
	fmt.Printf("%s\n", strings.Repeat("=", 60))

	defer i.cleanup()

	ctx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()

	state := &vCenterBuildState{cfg: i.cfg.VCenter}

	client, placement, err := connectAndResolvePlacement(ctx, i.cfg.VCenter)
	if err != nil {
		fmt.Printf("vCenter installation failed for %s: %v\n", i.cfg.DisplayName, err)
		return false
	}
	defer client.Logout(context.Background())
	state.client = client.Client
	state.placement = placement
	defer func() {
		if ctx.Err() == nil || state.completed {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.Background(), interruptCleanupTimeout)
		defer cancel()
		cleanupInterruptedVCenterBuild(cleanupCtx, state)
	}()

	localISOPath, err := i.createInstallerISO(ctx)
	if err != nil {
		fmt.Printf("vCenter installation failed for %s: %v\n", i.cfg.DisplayName, err)
		return false
	}

	remoteISOPath, datastoreISOPath, err := uploadInstallerISO(ctx, placement, localISOPath, i.cfg.VCenter.Name)
	if err != nil {
		fmt.Printf("vCenter installation failed for %s: %v\n", i.cfg.DisplayName, err)
		return false
	}
	state.remoteISOPath = remoteISOPath
	state.datastoreISOPath = datastoreISOPath
	remoteConsoleLogPath, datastoreConsoleLogPath := buildConsoleLogPaths(placement, i.cfg.VCenter.Name)
	state.remoteConsoleLogPath = remoteConsoleLogPath
	state.datastoreConsoleLogPath = datastoreConsoleLogPath

	var vm *object.VirtualMachine
	defer func() {
		if state.vmCreated && !state.completed && ctx.Err() == nil {
			var artifacts []string
			if !state.isoDeleted {
				artifacts = append(artifacts, fmt.Sprintf("installer ISO %q", state.datastoreISOPath))
			}
			if !state.consoleLogDeleted {
				artifacts = append(artifacts, fmt.Sprintf("console log %q", state.datastoreConsoleLogPath))
			}
			if len(artifacts) == 0 {
				fmt.Printf("Debug artifacts left in place: VM %q\n", i.cfg.VCenter.Name)
				return
			}
			fmt.Printf("Debug artifacts left in place: VM %q, %s\n", i.cfg.VCenter.Name, strings.Join(artifacts, ", "))
		}
	}()

	vm, err = createVM(ctx, client.Client, i.cfg, placement, datastoreISOPath, datastoreConsoleLogPath)
	if err != nil {
		if ctx.Err() == nil {
			if cleanupErr := deleteDatastoreISO(ctx, placement, remoteISOPath); cleanupErr != nil {
				fmt.Printf("Warning: could not delete temporary datastore ISO %q after VM creation failure: %v\n", datastoreISOPath, cleanupErr)
			} else {
				state.isoDeleted = true
			}
		}
		fmt.Printf("vCenter installation failed for %s: %v\n", i.cfg.DisplayName, err)
		return false
	}
	state.vm = vm
	state.vmCreated = true

	consoleStreamer := newDatastoreConsoleStreamer(placement.Datastore, placement.Host, remoteConsoleLogPath, datastoreConsoleLogPath, os.Stdout)
	if err := powerOnAndWaitForInstaller(ctx, vm, consoleStreamer); err != nil {
		fmt.Printf("vCenter installation failed for %s: %v\n", i.cfg.DisplayName, err)
		return false
	}

	if err := finalizePostInstallDevices(ctx, vm); err != nil {
		fmt.Printf("vCenter installation failed for %s: %v\n", i.cfg.DisplayName, err)
		return false
	}
	if err := applyFinalHardware(ctx, vm, i.cfg); err != nil {
		fmt.Printf("vCenter installation failed for %s: %v\n", i.cfg.DisplayName, err)
		return false
	}
	if err := maybeMarkAsTemplate(ctx, vm, outputType); err != nil {
		fmt.Printf("vCenter installation failed for %s: %v\n", i.cfg.DisplayName, err)
		return false
	}
	if err := deleteDatastoreISO(ctx, placement, remoteISOPath); err != nil {
		fmt.Printf("vCenter installation failed for %s: %v\n", i.cfg.DisplayName, err)
		return false
	}
	state.isoDeleted = true
	if err := deleteDatastoreConsoleLog(ctx, placement, remoteConsoleLogPath); err != nil {
		fmt.Printf("vCenter installation failed for %s: %v\n", i.cfg.DisplayName, err)
		return false
	}
	state.consoleLogDeleted = true
	state.completed = true

	fmt.Printf("\nOK vCenter %s created successfully for %s\n", outputNoun, i.cfg.DisplayName)
	fmt.Printf("  %s: %s\n", outputField, i.cfg.VCenter.Name)
	fmt.Printf("  Datacenter: %s\n", i.cfg.VCenter.Datacenter)
	fmt.Printf("  ESXi host: %s\n", i.cfg.VCenter.ESXiHost)
	fmt.Printf("  Datastore: %s\n", i.cfg.VCenter.Datastore)
	fmt.Printf("  Network: %s\n", i.cfg.VCenter.Network)
	fmt.Printf("  Boot firmware: %s\n", common.NormalizeBootFirmware(i.cfg.Hardware.BootFirmware))
	return true
}

func connectAndResolvePlacement(ctx context.Context, cfg ConnectionConfig) (*govmomi.Client, *placement, error) {
	fmt.Println("Connecting to vCenter and validating placement...")
	client, err := connect(ctx, cfg)
	if err != nil {
		return nil, nil, err
	}

	placement, err := ResolvePlacement(ctx, client.Client, cfg)
	if err != nil {
		_ = client.Logout(context.Background())
		return nil, nil, err
	}
	if err := validateTargetNameAvailable(ctx, client.Client, cfg, placement); err != nil {
		_ = client.Logout(context.Background())
		return nil, nil, err
	}

	fmt.Println("OK vCenter placement validated")
	return client, placement, nil
}

func connectAndResolveUploadPlacement(ctx context.Context, cfg ConnectionConfig) (*govmomi.Client, *placement, error) {
	fmt.Println("Connecting to vCenter and validating datastore placement...")
	client, err := connect(ctx, cfg)
	if err != nil {
		return nil, nil, err
	}

	placement, err := ResolveUploadPlacement(ctx, client.Client, cfg)
	if err != nil {
		_ = client.Logout(context.Background())
		return nil, nil, err
	}

	fmt.Println("OK vCenter datastore placement validated")
	return client, placement, nil
}

func validateTargetNameAvailable(ctx context.Context, client *vim25.Client, cfg ConnectionConfig, placement *placement) error {
	targetPath, err := targetInventoryPath(ctx, client, cfg, placement.Folder)
	if err != nil {
		return fmt.Errorf("resolve inventory path for VM folder %q: %w", cfg.Folder, err)
	}
	targetName := path.Base(targetPath)
	folderPath := path.Dir(targetPath)

	existing, err := object.NewSearchIndex(client).FindByInventoryPath(ctx, targetPath)
	if err != nil {
		return fmt.Errorf("check target VM/template inventory path %q: %w", targetPath, err)
	}
	if existing != nil {
		return fmt.Errorf("target VM/template name %q already exists in folder %q at inventory path %q as %s", targetName, folderPath, targetPath, refString(existing.Reference()))
	}
	return nil
}

func targetInventoryPath(ctx context.Context, client *vim25.Client, cfg ConnectionConfig, folder *object.Folder) (string, error) {
	targetName := strings.TrimSpace(cfg.Name)
	if targetName == "" {
		targetName = common.FallbackName
	}

	folderPath, err := resolvedFolderInventoryPath(ctx, client, folder)
	if err != nil {
		return "", err
	}
	return path.Join(folderPath, targetName), nil
}

func resolvedFolderInventoryPath(ctx context.Context, client *vim25.Client, folder *object.Folder) (string, error) {
	if inventoryPath := strings.TrimSpace(folder.InventoryPath); inventoryPath != "" {
		return inventoryPath, nil
	}

	inventoryPath, err := find.InventoryPath(ctx, client, folder.Reference())
	if err != nil {
		return "", err
	}
	inventoryPath = strings.TrimSpace(inventoryPath)
	if inventoryPath == "" {
		return "", errors.New("resolved empty folder inventory path")
	}
	folder.SetInventoryPath(inventoryPath)
	return inventoryPath, nil
}

type templateMarker interface {
	MarkAsTemplate(context.Context) error
}

func maybeMarkAsTemplate(ctx context.Context, vm templateMarker, outputType string) error {
	if common.NormalizeVCenterOutputType(outputType) == common.VCenterOutputTypeVM {
		return nil
	}
	return vm.MarkAsTemplate(ctx)
}

func vCenterOutputNoun(outputType string) string {
	if common.NormalizeVCenterOutputType(outputType) == common.VCenterOutputTypeVM {
		return "VM"
	}
	return common.VCenterOutputTypeTemplate
}

func vCenterOutputField(outputType string) string {
	if common.NormalizeVCenterOutputType(outputType) == common.VCenterOutputTypeVM {
		return "VM"
	}
	return "Template"
}

func (i *Installer) createInstallerISO(ctx context.Context) (string, error) {
	if err := i.ensureTempDir(); err != nil {
		return "", err
	}
	fmt.Println("Creating remastered autoinstall ISO...")
	outputPath := filepath.Join(i.tempDir, fmt.Sprintf("installer-%s.iso", common.SafeName(i.cfg.VCenter.Name)))
	if err := RemasterUbuntuISOWithNoCloud(ctx, i.cfg.UbuntuISO, outputPath, i.cfg.UserData, i.cfg.DisplayName, i.tempDir); err != nil {
		return "", err
	}
	fmt.Printf("OK remastered installer ISO created: %s\n", outputPath)
	return outputPath, nil
}

func (i *Installer) ensureTempDir() error {
	if i.tempDir != "" {
		return nil
	}
	tmpDir, err := os.MkdirTemp("", "ubuntu-vcenter-installer-")
	if err != nil {
		return fmt.Errorf("create temporary directory: %w", err)
	}
	i.tempDir = tmpDir
	fmt.Printf("Created temporary directory: %s\n", tmpDir)
	return nil
}

func connect(ctx context.Context, cfg ConnectionConfig) (*govmomi.Client, error) {
	u, err := BuildURL(cfg)
	if err != nil {
		return nil, err
	}
	client, err := govmomi.NewClient(ctx, u, cfg.Insecure)
	if err != nil {
		return nil, fmt.Errorf("connect to vCenter %s: %w", sanitizeURL(u), err)
	}
	return client, nil
}

func BuildURL(cfg ConnectionConfig) (*url.URL, error) {
	rawHost := strings.TrimSpace(cfg.Host)
	if rawHost == "" {
		return nil, fmt.Errorf("--vcenter-host is required")
	}
	if !strings.Contains(rawHost, "://") {
		rawHost = "https://" + rawHost
	}

	u, err := url.Parse(rawHost)
	if err != nil {
		return nil, fmt.Errorf("parse --vcenter-host: %w", err)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("--vcenter-host must include a host")
	}
	if u.Path == "" || u.Path == "/" {
		u.Path = "/sdk"
	}
	u.User = url.UserPassword(cfg.Username, cfg.Password)
	return u, nil
}

func sanitizeURL(u *url.URL) string {
	if u == nil {
		return ""
	}
	clean := *u
	clean.User = nil
	return clean.String()
}

func ResolvePlacement(ctx context.Context, client *vim25.Client, cfg ConnectionConfig) (*placement, error) {
	finder := find.NewFinder(client, true)

	datacenter, err := finder.Datacenter(ctx, cfg.Datacenter)
	if err != nil {
		return nil, fmt.Errorf("find datacenter %q: %w", cfg.Datacenter, err)
	}
	finder.SetDatacenter(datacenter)

	host, err := finder.HostSystem(ctx, cfg.ESXiHost)
	if err != nil {
		return nil, fmt.Errorf("find ESXi host %q: %w", cfg.ESXiHost, err)
	}

	datastore, err := finder.Datastore(ctx, cfg.Datastore)
	if err != nil {
		return nil, fmt.Errorf("find datastore %q: %w", cfg.Datastore, err)
	}
	if ok, err := hostHasReference(ctx, host, "datastore", datastore.Reference()); err != nil {
		return nil, fmt.Errorf("validate datastore %q on host %q: %w", cfg.Datastore, cfg.ESXiHost, err)
	} else if !ok {
		return nil, fmt.Errorf("datastore %q is not attached to host %q", cfg.Datastore, cfg.ESXiHost)
	}

	folder, err := findVMFolder(ctx, finder, datacenter, cfg.Folder)
	if err != nil {
		return nil, fmt.Errorf("find VM folder %q: %w", cfg.Folder, err)
	}

	network, err := finder.Network(ctx, cfg.Network)
	if err != nil {
		return nil, fmt.Errorf("find network %q: %w", cfg.Network, err)
	}
	if ok, err := hostHasReference(ctx, host, "network", network.Reference()); err != nil {
		return nil, fmt.Errorf("validate network %q on host %q: %w", cfg.Network, cfg.ESXiHost, err)
	} else if !ok {
		return nil, fmt.Errorf("network %q is not available on host %q", cfg.Network, cfg.ESXiHost)
	}

	pool, err := host.ResourcePool(ctx)
	if err != nil {
		return nil, fmt.Errorf("find resource pool for host %q: %w", cfg.ESXiHost, err)
	}

	return &placement{
		Datacenter:   datacenter,
		Host:         host,
		Datastore:    datastore,
		Folder:       folder,
		Network:      network,
		ResourcePool: pool,
	}, nil
}

func ResolveUploadPlacement(ctx context.Context, client *vim25.Client, cfg ConnectionConfig) (*placement, error) {
	finder := find.NewFinder(client, true)

	datacenter, err := finder.Datacenter(ctx, cfg.Datacenter)
	if err != nil {
		return nil, fmt.Errorf("find datacenter %q: %w", cfg.Datacenter, err)
	}
	finder.SetDatacenter(datacenter)

	host, err := finder.HostSystem(ctx, cfg.ESXiHost)
	if err != nil {
		return nil, fmt.Errorf("find ESXi host %q: %w", cfg.ESXiHost, err)
	}

	datastore, err := finder.Datastore(ctx, cfg.Datastore)
	if err != nil {
		return nil, fmt.Errorf("find datastore %q: %w", cfg.Datastore, err)
	}
	if ok, err := hostHasReference(ctx, host, "datastore", datastore.Reference()); err != nil {
		return nil, fmt.Errorf("validate datastore %q on host %q: %w", cfg.Datastore, cfg.ESXiHost, err)
	} else if !ok {
		return nil, fmt.Errorf("datastore %q is not attached to host %q", cfg.Datastore, cfg.ESXiHost)
	}

	return &placement{
		Datacenter: datacenter,
		Host:       host,
		Datastore:  datastore,
	}, nil
}

func hostHasReference(ctx context.Context, host *object.HostSystem, property string, ref types.ManagedObjectReference) (bool, error) {
	var hostMO mo.HostSystem
	if err := host.Properties(ctx, host.Reference(), []string{property}, &hostMO); err != nil {
		return false, err
	}

	var refs []types.ManagedObjectReference
	switch property {
	case "datastore":
		refs = hostMO.Datastore
	case "network":
		refs = hostMO.Network
	default:
		return false, fmt.Errorf("unsupported host reference property %q", property)
	}

	for _, candidate := range refs {
		if candidate == ref {
			return true, nil
		}
	}
	return false, nil
}

func findVMFolder(ctx context.Context, finder *find.Finder, datacenter *object.Datacenter, folderPath string) (*object.Folder, error) {
	folderPath = strings.TrimSpace(folderPath)
	if folderPath == "" {
		return nil, fmt.Errorf("folder path is required")
	}

	if strings.HasPrefix(folderPath, "/") {
		return finder.Folder(ctx, folderPath)
	}

	folders, err := datacenter.Folders(ctx)
	if err != nil {
		return nil, err
	}
	if folderPath == "vm" {
		return folders.VmFolder, nil
	}

	if folder, err := finder.Folder(ctx, folderPath); err == nil {
		return folder, nil
	}

	dcName, err := datacenter.ObjectName(ctx)
	if err != nil {
		return nil, err
	}
	return finder.Folder(ctx, path.Join("/", dcName, "vm", folderPath))
}

func uploadInstallerISO(ctx context.Context, placement *placement, localISOPath, templateName string) (string, string, error) {
	remotePath := fmt.Sprintf("ubuntu-vm-template-builder-%s-%s.iso", common.SafeName(templateName), time.Now().UTC().Format("20060102T150405Z"))
	datastorePath := placement.Datastore.Path(remotePath)

	fmt.Printf("Uploading installer ISO to datastore: %s\n", datastorePath)
	if err := uploadDatastoreFile(ctx, placement, localISOPath, remotePath); err != nil {
		return "", "", fmt.Errorf("upload installer ISO to datastore: %w", err)
	}
	fmt.Printf("OK installer ISO uploaded: %s\n", datastorePath)
	return remotePath, datastorePath, nil
}

func uploadDatastoreFile(ctx context.Context, placement *placement, localPath, remotePath string) error {
	if err := placement.Datastore.UploadFile(ctx, localPath, remotePath, nil); err == nil {
		return nil
	} else {
		fmt.Printf("Warning: vCenter datastore upload endpoint failed: %v\n", err)
		fmt.Println("Retrying datastore upload through selected ESXi host...")
		if hostErr := placement.Datastore.UploadFile(placement.Datastore.HostContext(ctx, placement.Host), localPath, remotePath, nil); hostErr != nil {
			return fmt.Errorf("vCenter endpoint failed: %v; selected ESXi host endpoint failed: %w", err, hostErr)
		}
	}
	return nil
}

func normalizeUploadDestinationPath(destination string) (string, error) {
	raw := filepath.ToSlash(strings.TrimSpace(destination))
	if raw == "" {
		return "", errors.New("--destination is required")
	}
	var datastorePath object.DatastorePath
	if datastorePath.FromString(raw) {
		return "", errors.New("--destination must be relative to the selected datastore, for example uploads/file.iso")
	}
	if strings.HasSuffix(raw, "/") {
		return "", fmt.Errorf("--destination %q must include a file name", destination)
	}
	remotePath := path.Clean(raw)
	switch {
	case remotePath == "." || remotePath == "/":
		return "", fmt.Errorf("--destination %q must include a file name", destination)
	case path.IsAbs(remotePath), remotePath == "..", strings.HasPrefix(remotePath, "../"):
		return "", fmt.Errorf("--destination %q must be relative to the selected datastore", destination)
	}
	return remotePath, nil
}

func datastoreFileExists(ctx context.Context, placement *placement, remotePath string) (bool, error) {
	exists, err := datastoreFileExistsWithContext(ctx, placement, remotePath)
	if err == nil {
		return exists, nil
	}

	hostExists, hostErr := datastoreFileExistsWithContext(placement.Datastore.HostContext(ctx, placement.Host), placement, remotePath)
	if hostErr == nil {
		return hostExists, nil
	}
	if isMissingDatastoreFile(err) && (isMissingDatastoreFile(hostErr) || isDatastoreEndpointConnectionError(hostErr)) {
		return false, nil
	}
	if !isMissingDatastoreFile(err) && isMissingDatastoreFile(hostErr) {
		return false, nil
	}
	if isMissingDatastoreFile(err) {
		return false, fmt.Errorf("vCenter endpoint reported missing file; selected ESXi host endpoint failed: %w", hostErr)
	}
	return false, fmt.Errorf("vCenter endpoint failed: %v; selected ESXi host endpoint failed: %w", err, hostErr)
}

func datastoreFileExistsWithContext(ctx context.Context, placement *placement, remotePath string) (bool, error) {
	reader, _, err := placement.Datastore.Download(ctx, remotePath, nil)
	if err != nil {
		return false, err
	}
	if err := reader.Close(); err != nil {
		return true, err
	}
	return true, nil
}

func isDatastoreEndpointConnectionError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "no route to host") ||
		strings.Contains(msg, "connection reset")
}

func ensureDatastoreParentDirectory(ctx context.Context, placement *placement, remotePath string) error {
	parent := path.Dir(remotePath)
	if parent == "." || parent == "/" {
		return nil
	}

	manager := placement.Datastore.NewFileManager(placement.Datacenter, true)
	datastoreDir := manager.Path(parent).String()
	if err := manager.FileManager.MakeDirectory(ctx, datastoreDir, placement.Datacenter, true); err != nil && !isAlreadyExistsError(err) {
		return fmt.Errorf("create datastore directory %q: %w", datastoreDir, err)
	}
	return nil
}

func isAlreadyExistsError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "already exists") ||
		strings.Contains(msg, "filealreadyexists") ||
		strings.Contains(msg, "file already exists")
}

func buildConsoleLogPaths(placement *placement, templateName string) (string, string) {
	remotePath := fmt.Sprintf("ubuntu-vm-template-builder-%s-%s-console.log", common.SafeName(templateName), time.Now().UTC().Format("20060102T150405Z"))
	return remotePath, placement.Datastore.Path(remotePath)
}

func createVM(ctx context.Context, client *vim25.Client, cfg Config, placement *placement, datastoreISOPath, datastoreConsoleLogPath string) (*object.VirtualMachine, error) {
	diskBytes, err := common.ParseDiskSize(cfg.DiskSize)
	if err != nil {
		return nil, err
	}

	spec, err := BuildVMConfig(ctx, cfg, placement, datastoreISOPath, datastoreConsoleLogPath, diskBytes)
	if err != nil {
		return nil, err
	}

	fmt.Printf("Creating VM %q on host %q...\n", cfg.VCenter.Name, cfg.VCenter.ESXiHost)
	printCreateVMRequestDetails(ctx, cfg, placement, datastoreISOPath, datastoreConsoleLogPath, diskBytes)
	task, err := placement.Folder.CreateVM(ctx, spec, placement.ResourcePool, placement.Host)
	if err != nil {
		printCreateVMFailureDetails(ctx, "CreateVM API call", err, placement)
		return nil, fmt.Errorf("create VM: %w", err)
	}
	info, err := task.WaitForResult(ctx)
	if err != nil {
		printCreateVMFailureDetails(ctx, "CreateVM task", err, placement)
		return nil, fmt.Errorf("wait for VM creation: %w", err)
	}

	vmRef, ok := info.Result.(types.ManagedObjectReference)
	if !ok {
		return nil, fmt.Errorf("create VM returned unexpected result %T", info.Result)
	}
	fmt.Printf("OK VM created: %s\n", cfg.VCenter.Name)
	return object.NewVirtualMachine(client, vmRef), nil
}

func printCreateVMRequestDetails(ctx context.Context, cfg Config, placement *placement, datastoreISOPath, datastoreConsoleLogPath string, diskBytes int64) {
	hardware := cfg.Hardware.Normalize()
	resourcePoolName := objectNameOrUnavailable(ctx, placement.ResourcePool)

	fmt.Println("vCenter CreateVM request details:")
	fmt.Printf("  VM name: %s\n", cfg.VCenter.Name)
	fmt.Printf("  Datacenter: %s (%s)\n", cfg.VCenter.Datacenter, refString(placement.Datacenter.Reference()))
	fmt.Printf("  VM folder: %s\n", objectTargetString(cfg.VCenter.Folder, placement.Folder.InventoryPath, placement.Folder.Reference()))
	fmt.Printf("  ESXi host: %s (%s)\n", cfg.VCenter.ESXiHost, refString(placement.Host.Reference()))
	fmt.Printf("  Resource pool: %s (%s)\n", resourcePoolName, refString(placement.ResourcePool.Reference()))
	fmt.Printf("  Datastore: %s (%s)\n", cfg.VCenter.Datastore, refString(placement.Datastore.Reference()))
	fmt.Printf("  Network: %s\n", objectTargetString(cfg.VCenter.Network, placement.Network.GetInventoryPath(), placement.Network.Reference()))
	fmt.Printf("  Installer ISO: %s\n", datastoreISOPath)
	fmt.Printf("  Console log: %s\n", datastoreConsoleLogPath)
	fmt.Printf("  Hardware: firmware=%s compatibility=%s guest_os_id=%s vcpu=%d memory_mb=%d disk_size=%s disk_bytes=%d scsi=%s nic=%s disk_provisioning=%s reserve_all_guest_memory=%t\n",
		hardware.BootFirmware,
		emptyAsDefault(hardware.VCenter.Compatibility, "vCenter default"),
		hardware.VCenter.GuestOSID,
		hardware.VCPU,
		hardware.MemoryMB,
		hardware.DiskSize,
		diskBytes,
		hardware.VCenter.SCSIController,
		hardware.VCenter.NetworkAdapter,
		hardware.VCenter.DiskProvisioning,
		hardware.VCenter.ReserveAllGuestMemory,
	)
}

func printCreateVMFailureDetails(ctx context.Context, stage string, err error, placement *placement) {
	fmt.Printf("vCenter CreateVM failure details (%s):\n", stage)
	fmt.Printf("  Error: %v\n", err)

	if fault := vSphereFaultFromError(err); fault != nil {
		fmt.Printf("  vSphere fault type: %T\n", fault)
		printVCenterFaultMessages(fault)
		if noPermission, ok := fault.(types.BaseNoPermission); ok {
			printNoPermissionDetails(ctx, noPermission.GetNoPermission(), placement)
		}
	} else {
		fmt.Println("  vSphere fault type: unavailable from response")
	}

	fmt.Println("  Common CreateVM privileges to verify:")
	fmt.Println("    VM folder: VirtualMachine.Inventory.Create")
	fmt.Println("    Resource pool / host or cluster: Resource.AssignVMToPool")
	fmt.Println("    Datastore: Datastore.AllocateSpace, Datastore.FileManagement")
	fmt.Println("    Network / port group: Network.Assign")
	fmt.Println("    VM hardware config: VirtualMachine.Config.AddNewDisk, VirtualMachine.Config.AddRemoveDevice, VirtualMachine.Config.CPUCount, VirtualMachine.Config.Memory, VirtualMachine.Config.Settings")
	fmt.Println("    Later template conversion, if output_type is template: VirtualMachine.Provisioning.MarkAsTemplate")
}

type vSphereFaultError interface {
	Fault() types.BaseMethodFault
}

func vSphereFaultFromError(err error) types.BaseMethodFault {
	var faultErr vSphereFaultError
	if errors.As(err, &faultErr) {
		return faultErr.Fault()
	}
	return nil
}

func printVCenterFaultMessages(fault types.BaseMethodFault) {
	methodFault := fault.GetMethodFault()
	for _, msg := range methodFault.FaultMessage {
		switch {
		case strings.TrimSpace(msg.Message) != "":
			fmt.Printf("  vSphere message: %s\n", strings.TrimSpace(msg.Message))
		case strings.TrimSpace(msg.Key) != "":
			fmt.Printf("  vSphere message key: %s\n", strings.TrimSpace(msg.Key))
		}
	}
}

func printNoPermissionDetails(ctx context.Context, noPermission *types.NoPermission, placement *placement) {
	if noPermission == nil {
		return
	}

	fmt.Println("  vSphere permission fault: NoPermission")
	if noPermission.Object != nil {
		fmt.Printf("  Permission object: %s\n", describePlacementRef(ctx, *noPermission.Object, placement))
	}
	if strings.TrimSpace(noPermission.PrivilegeId) != "" {
		fmt.Printf("  Missing privilege: %s\n", strings.TrimSpace(noPermission.PrivilegeId))
	}
	for _, missing := range noPermission.MissingPrivileges {
		privileges := strings.Join(missing.PrivilegeIds, ", ")
		if strings.TrimSpace(privileges) == "" {
			privileges = "(not provided by vCenter)"
		}
		fmt.Printf("  Missing privileges on %s: %s\n", describePlacementRef(ctx, missing.Entity, placement), privileges)
	}
	if noPermission.Object == nil && strings.TrimSpace(noPermission.PrivilegeId) == "" && len(noPermission.MissingPrivileges) == 0 {
		fmt.Println("  Missing privilege details were not provided by vCenter.")
	}
}

func describePlacementRef(ctx context.Context, ref types.ManagedObjectReference, placement *placement) string {
	known := map[string]string{
		refKey(placement.Datacenter.Reference()):   fmt.Sprintf("datacenter %q", objectNameOrUnavailable(ctx, placement.Datacenter)),
		refKey(placement.Folder.Reference()):       fmt.Sprintf("VM folder %q", objectNameOrUnavailable(ctx, placement.Folder)),
		refKey(placement.Host.Reference()):         fmt.Sprintf("ESXi host %q", objectNameOrUnavailable(ctx, placement.Host)),
		refKey(placement.ResourcePool.Reference()): fmt.Sprintf("resource pool %q", objectNameOrUnavailable(ctx, placement.ResourcePool)),
		refKey(placement.Datastore.Reference()):    fmt.Sprintf("datastore %q", objectNameOrUnavailable(ctx, placement.Datastore)),
		refKey(placement.Network.Reference()):      fmt.Sprintf("network %q", networkNameOrPath(ctx, placement.Network)),
	}
	if label, ok := known[refKey(ref)]; ok {
		return fmt.Sprintf("%s (%s)", label, refString(ref))
	}
	return refString(ref)
}

type objectNamer interface {
	ObjectName(context.Context) (string, error)
}

func objectNameOrUnavailable(ctx context.Context, obj objectNamer) string {
	name, err := obj.ObjectName(ctx)
	if err != nil {
		return fmt.Sprintf("unavailable: %v", err)
	}
	if strings.TrimSpace(name) == "" {
		return "unavailable"
	}
	return name
}

func networkNameOrPath(ctx context.Context, network object.NetworkReference) string {
	if named, ok := network.(objectNamer); ok {
		if name := objectNameOrUnavailable(ctx, named); name != "unavailable" && !strings.HasPrefix(name, "unavailable:") {
			return name
		}
	}
	if path := strings.TrimSpace(network.GetInventoryPath()); path != "" {
		return path
	}
	return "unavailable"
}

func objectTargetString(inputName, inventoryPath string, ref types.ManagedObjectReference) string {
	inputName = strings.TrimSpace(inputName)
	inventoryPath = strings.TrimSpace(inventoryPath)
	switch {
	case inputName != "" && inventoryPath != "" && inputName != inventoryPath:
		return fmt.Sprintf("%s (resolved: %s, %s)", inputName, inventoryPath, refString(ref))
	case inventoryPath != "":
		return fmt.Sprintf("%s (%s)", inventoryPath, refString(ref))
	case inputName != "":
		return fmt.Sprintf("%s (%s)", inputName, refString(ref))
	default:
		return refString(ref)
	}
}

func emptyAsDefault(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func refString(ref types.ManagedObjectReference) string {
	return fmt.Sprintf("%s:%s", ref.Type, ref.Value)
}

func refKey(ref types.ManagedObjectReference) string {
	return refString(ref)
}

func BuildVMConfig(ctx context.Context, cfg Config, placement *placement, datastoreISOPath, datastoreConsoleLogPath string, diskBytes int64) (types.VirtualMachineConfigSpec, error) {
	vcenter := normalizeConnectionConfig(cfg.VCenter, cfg.DisplayName)
	hardware := cfg.Hardware.Normalize()
	if err := hardware.Validate(); err != nil {
		return types.VirtualMachineConfigSpec{}, err
	}

	var devices object.VirtualDeviceList
	scsiDevice, err := devices.CreateSCSIController(hardware.VCenter.SCSIController)
	if err != nil {
		return types.VirtualMachineConfigSpec{}, err
	}
	devices = append(devices, scsiDevice)
	scsiController := scsiDevice.(types.BaseVirtualController)

	datastoreRef := placement.Datastore.Reference()
	diskBacking := &types.VirtualDiskFlatVer2BackingInfo{
		VirtualDeviceFileBackingInfo: types.VirtualDeviceFileBackingInfo{
			Datastore: &datastoreRef,
		},
		DiskMode: string(types.VirtualDiskModePersistent),
	}
	applyDiskProvisioning(diskBacking, hardware.VCenter.DiskProvisioning)
	disk := &types.VirtualDisk{
		VirtualDevice: types.VirtualDevice{
			Key:     devices.NewKey(),
			Backing: diskBacking,
		},
		CapacityInKB: diskBytes / 1024,
	}
	devices.AssignController(disk, scsiController)
	devices = append(devices, disk)

	ideDevice, err := devices.CreateIDEController()
	if err != nil {
		return types.VirtualMachineConfigSpec{}, err
	}
	devices = append(devices, ideDevice)
	cdrom, err := devices.CreateCdrom(ideDevice.(types.BaseVirtualController))
	if err != nil {
		return types.VirtualMachineConfigSpec{}, err
	}
	cdrom = devices.InsertIso(cdrom, datastoreISOPath)
	devices = append(devices, cdrom)

	networkBacking, err := placement.Network.EthernetCardBackingInfo(ctx)
	if err != nil {
		return types.VirtualMachineConfigSpec{}, err
	}
	nic, err := devices.CreateEthernetCard(hardware.VCenter.NetworkAdapter, networkBacking)
	if err != nil {
		return types.VirtualMachineConfigSpec{}, err
	}
	devices = append(devices, nic)

	if strings.TrimSpace(datastoreConsoleLogPath) != "" {
		sioController := &types.VirtualSIOController{}
		sioController.Key = devices.NewKey()
		devices = append(devices, sioController)

		serial, err := devices.CreateSerialPort()
		if err != nil {
			return types.VirtualMachineConfigSpec{}, err
		}
		serial = devices.ConnectSerialPort(serial, datastoreConsoleLogPath, false, "")
		serial.Connectable = &types.VirtualDeviceConnectInfo{
			Connected:      true,
			StartConnected: true,
		}
		devices = append(devices, serial)
	}

	deviceChange, err := devices.ConfigSpec(types.VirtualDeviceConfigSpecOperationAdd)
	if err != nil {
		return types.VirtualMachineConfigSpec{}, err
	}

	firmware := string(types.GuestOsDescriptorFirmwareTypeBios)
	if common.NormalizeBootFirmware(hardware.BootFirmware) == common.BootFirmwareUEFI {
		firmware = string(types.GuestOsDescriptorFirmwareTypeEfi)
	}

	spec := types.VirtualMachineConfigSpec{
		Name:     vcenter.Name,
		GuestId:  hardware.VCenter.GuestOSID,
		NumCPUs:  int32(hardware.VCPU),
		MemoryMB: int64(hardware.MemoryMB),
		Firmware: firmware,
		Files: &types.VirtualMachineFileInfo{
			VmPathName: fmt.Sprintf("[%s]", placement.Datastore.Name()),
		},
		BootOptions: &types.VirtualMachineBootOptions{
			BootOrder: []types.BaseVirtualMachineBootOptionsBootableDevice{
				&types.VirtualMachineBootOptionsBootableCdromDevice{},
				&types.VirtualMachineBootOptionsBootableDiskDevice{DeviceKey: disk.Key},
			},
		},
		DeviceChange: deviceChange,
	}
	if hardware.VCenter.Compatibility != "" {
		spec.Version = hardware.VCenter.Compatibility
	}
	if hardware.VCenter.ReserveAllGuestMemory {
		spec.MemoryReservationLockedToMax = types.NewBool(true)
		spec.MemoryAllocation = &types.ResourceAllocationInfo{
			Reservation: types.NewInt64(int64(hardware.MemoryMB)),
		}
	}
	return spec, nil
}

func applyDiskProvisioning(backing *types.VirtualDiskFlatVer2BackingInfo, provisioning string) {
	switch provisioning {
	case common.VCenterDiskProvisioningThin:
		backing.ThinProvisioned = types.NewBool(true)
		backing.EagerlyScrub = types.NewBool(false)
	case common.VCenterDiskProvisioningThickEagerZeroed:
		backing.ThinProvisioned = types.NewBool(false)
		backing.EagerlyScrub = types.NewBool(true)
	default:
		backing.ThinProvisioned = types.NewBool(false)
		backing.EagerlyScrub = types.NewBool(false)
	}
}

func powerOnAndWaitForInstaller(ctx context.Context, vm *object.VirtualMachine, consoleStreamer *datastoreConsoleStreamer) error {
	fmt.Println("Powering on VM and waiting for the installer to power it off...")
	stopConsoleStream := consoleStreamer.start(ctx)
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), consoleLogFinalPollTimeout)
		defer cancel()
		stopConsoleStream(stopCtx)
	}()

	task, err := vm.PowerOn(ctx)
	if err != nil {
		return fmt.Errorf("power on VM: %w", err)
	}
	if err := task.Wait(ctx); err != nil {
		return fmt.Errorf("wait for power on: %w", err)
	}
	if err := vm.WaitForPowerState(ctx, types.VirtualMachinePowerStatePoweredOff); err != nil {
		return fmt.Errorf("wait for installer poweroff: %w", err)
	}
	fmt.Println("OK installer powered off the VM")
	return nil
}

func finalizePostInstallDevices(ctx context.Context, vm *object.VirtualMachine) error {
	fmt.Println("Finalizing post-install VM devices and boot order...")
	devices, err := vm.Device(ctx)
	if err != nil {
		return fmt.Errorf("read VM devices: %w", err)
	}
	cdromCount := len(devices.SelectByType((*types.VirtualCdrom)(nil)))
	serialCount := len(devices.SelectByType((*types.VirtualSerialPort)(nil)))
	spec, err := buildPostInstallDeviceSpec(devices)
	if err != nil {
		return err
	}
	task, err := vm.Reconfigure(ctx, spec)
	if err != nil {
		return fmt.Errorf("finalize post-install devices: %w", err)
	}
	if err := task.Wait(ctx); err != nil {
		return fmt.Errorf("wait for post-install device finalization: %w", err)
	}
	if err := verifyPostInstallDevices(ctx, vm); err != nil {
		return err
	}
	fmt.Printf("OK post-install VM devices finalized (removed %d CD/DVD drive(s), %d serial port(s); boot order is disk-only)\n", cdromCount, serialCount)
	return nil
}

func buildPostInstallDeviceSpec(devices object.VirtualDeviceList) (types.VirtualMachineConfigSpec, error) {
	diskDevice := firstDiskDevice(devices)
	if diskDevice == nil {
		return types.VirtualMachineConfigSpec{}, errors.New("no virtual disk found for final boot order")
	}

	var removedDevices object.VirtualDeviceList
	for _, device := range devices.SelectByType((*types.VirtualCdrom)(nil)) {
		removedDevices = append(removedDevices, device)
	}
	for _, device := range devices.SelectByType((*types.VirtualSerialPort)(nil)) {
		removedDevices = append(removedDevices, device)
	}

	deviceChange, err := removedDevices.ConfigSpec(types.VirtualDeviceConfigSpecOperationRemove)
	if err != nil {
		return types.VirtualMachineConfigSpec{}, err
	}

	return types.VirtualMachineConfigSpec{
		BootOptions: &types.VirtualMachineBootOptions{
			BootOrder: []types.BaseVirtualMachineBootOptionsBootableDevice{
				&types.VirtualMachineBootOptionsBootableDiskDevice{DeviceKey: diskDevice.Key},
			},
		},
		DeviceChange: deviceChange,
	}, nil
}

func firstDiskDevice(devices object.VirtualDeviceList) *types.VirtualDisk {
	disks := devices.SelectByType((*types.VirtualDisk)(nil))
	if len(disks) == 0 {
		return nil
	}
	return disks[0].(*types.VirtualDisk)
}

func verifyPostInstallDevices(ctx context.Context, vm *object.VirtualMachine) error {
	devices, err := vm.Device(ctx)
	if err != nil {
		return fmt.Errorf("verify post-install devices: read VM devices: %w", err)
	}
	if count := len(devices.SelectByType((*types.VirtualCdrom)(nil))); count > 0 {
		return fmt.Errorf("verify post-install devices: %d CD/DVD drive(s) still attached", count)
	}
	if count := len(devices.SelectByType((*types.VirtualSerialPort)(nil))); count > 0 {
		return fmt.Errorf("verify post-install devices: %d serial port(s) still attached", count)
	}

	diskDevice := firstDiskDevice(devices)
	if diskDevice == nil {
		return errors.New("verify post-install devices: no virtual disk found for final boot order")
	}

	var vmMO mo.VirtualMachine
	if err := vm.Properties(ctx, vm.Reference(), []string{"config.bootOptions"}, &vmMO); err != nil {
		return fmt.Errorf("verify post-install devices: read VM boot options: %w", err)
	}
	if vmMO.Config == nil {
		return errors.New("verify post-install devices: VM config unavailable")
	}
	if err := verifyDiskOnlyBootOrder(vmMO.Config.BootOptions, diskDevice.Key); err != nil {
		return fmt.Errorf("verify post-install devices: %w", err)
	}
	return nil
}

func verifyDiskOnlyBootOrder(bootOptions *types.VirtualMachineBootOptions, diskKey int32) error {
	if bootOptions == nil {
		return errors.New("boot options unavailable")
	}
	if len(bootOptions.BootOrder) != 1 {
		return fmt.Errorf("boot order has %d entries, want exactly one disk entry", len(bootOptions.BootOrder))
	}
	bootDisk, ok := bootOptions.BootOrder[0].(*types.VirtualMachineBootOptionsBootableDiskDevice)
	if !ok {
		return fmt.Errorf("boot order contains %T, want disk boot device", bootOptions.BootOrder[0])
	}
	if bootDisk.DeviceKey != diskKey {
		return fmt.Errorf("boot order disk key = %d, want %d", bootDisk.DeviceKey, diskKey)
	}
	return nil
}

func applyFinalHardware(ctx context.Context, vm *object.VirtualMachine, cfg Config) error {
	spec := types.VirtualMachineConfigSpec{
		NumCPUs:  int32(cfg.Hardware.VCPU),
		MemoryMB: int64(cfg.Hardware.MemoryMB),
	}
	if cfg.Hardware.VCenter.ReserveAllGuestMemory {
		spec.MemoryReservationLockedToMax = types.NewBool(true)
		spec.MemoryAllocation = &types.ResourceAllocationInfo{
			Reservation: types.NewInt64(int64(cfg.Hardware.MemoryMB)),
		}
	}
	task, err := vm.Reconfigure(ctx, spec)
	if err != nil {
		return fmt.Errorf("apply final hardware settings: %w", err)
	}
	if err := task.Wait(ctx); err != nil {
		return fmt.Errorf("wait for final hardware settings: %w", err)
	}
	return nil
}

func deleteDatastoreISO(ctx context.Context, placement *placement, remoteISOPath string) error {
	fmt.Printf("Deleting temporary datastore ISO: %s\n", placement.Datastore.Path(remoteISOPath))
	manager := placement.Datastore.NewFileManager(placement.Datacenter, true)
	if err := manager.Delete(ctx, remoteISOPath); err != nil {
		return fmt.Errorf("delete temporary datastore ISO: %w", err)
	}
	fmt.Println("OK temporary datastore ISO deleted")
	return nil
}

func deleteDatastoreConsoleLog(ctx context.Context, placement *placement, remoteConsoleLogPath string) error {
	fmt.Printf("Deleting temporary datastore console log: %s\n", placement.Datastore.Path(remoteConsoleLogPath))
	manager := placement.Datastore.NewFileManager(placement.Datacenter, true)
	if err := manager.Delete(ctx, remoteConsoleLogPath); err != nil {
		return fmt.Errorf("delete temporary datastore console log: %w", err)
	}
	fmt.Println("OK temporary datastore console log deleted")
	return nil
}

func cleanupInterruptedVCenterBuild(ctx context.Context, state *vCenterBuildState) {
	if state == nil {
		return
	}
	fmt.Println("Interrupt received; cleaning up vCenter resources...")
	if state.client == nil || state.placement == nil {
		fmt.Println("No vCenter placement was resolved yet; no remote resources to clean up")
		return
	}

	vm := state.vm
	if vm == nil {
		var err error
		vm, err = findTargetVMByInventoryPath(ctx, state.client, state.cfg, state.placement)
		if err != nil {
			fmt.Printf("Warning: could not find interrupted vCenter VM %q for cleanup: %v\n", state.cfg.Name, err)
		}
	}
	if vm != nil {
		if err := destroyInterruptedVM(ctx, vm, state.cfg.Name); err != nil {
			fmt.Printf("Warning: could not delete interrupted vCenter VM %q: %v\n", state.cfg.Name, err)
		} else {
			state.vmCreated = false
			state.vm = nil
		}
	} else {
		fmt.Printf("No interrupted vCenter VM named %q was found for cleanup\n", state.cfg.Name)
	}

	if state.remoteISOPath != "" && !state.isoDeleted {
		if err := deleteDatastoreFileForInterrupt(ctx, state.placement, state.remoteISOPath, state.datastoreISOPath, "installer ISO"); err != nil {
			fmt.Printf("Warning: could not delete interrupted vCenter installer ISO %q: %v\n", state.datastoreISOPath, err)
		} else {
			state.isoDeleted = true
		}
	}
	if state.remoteConsoleLogPath != "" && !state.consoleLogDeleted {
		if err := deleteDatastoreFileForInterrupt(ctx, state.placement, state.remoteConsoleLogPath, state.datastoreConsoleLogPath, "console log"); err != nil {
			fmt.Printf("Warning: could not delete interrupted vCenter console log %q: %v\n", state.datastoreConsoleLogPath, err)
		} else {
			state.consoleLogDeleted = true
		}
	}
}

func findTargetVMByInventoryPath(ctx context.Context, client *vim25.Client, cfg ConnectionConfig, placement *placement) (*object.VirtualMachine, error) {
	targetPath, err := targetInventoryPath(ctx, client, cfg, placement.Folder)
	if err != nil {
		return nil, err
	}

	found, err := object.NewSearchIndex(client).FindByInventoryPath(ctx, targetPath)
	if err != nil {
		return nil, fmt.Errorf("search inventory path %q: %w", targetPath, err)
	}
	if found == nil {
		return nil, nil
	}
	if found.Reference().Type != "VirtualMachine" {
		return nil, fmt.Errorf("inventory path %q resolved to %s, not a VM", targetPath, refString(found.Reference()))
	}
	return object.NewVirtualMachine(client, found.Reference()), nil
}

func destroyInterruptedVM(ctx context.Context, vm *object.VirtualMachine, name string) error {
	powerState, err := vm.PowerState(ctx)
	if err != nil {
		fmt.Printf("Warning: could not read interrupted vCenter VM %q power state before cleanup: %v\n", name, err)
	} else if powerState != types.VirtualMachinePowerStatePoweredOff {
		fmt.Printf("Powering off interrupted vCenter VM %q...\n", name)
		task, err := vm.PowerOff(ctx)
		if err != nil {
			return fmt.Errorf("power off VM: %w", err)
		}
		if err := task.Wait(ctx); err != nil {
			return fmt.Errorf("wait for VM power off: %w", err)
		}
		fmt.Printf("OK interrupted vCenter VM powered off: %s\n", name)
	}

	fmt.Printf("Deleting interrupted vCenter VM %q...\n", name)
	task, err := vm.Destroy(ctx)
	if err != nil {
		return fmt.Errorf("destroy VM: %w", err)
	}
	if err := task.Wait(ctx); err != nil {
		return fmt.Errorf("wait for VM destroy: %w", err)
	}
	fmt.Printf("OK interrupted vCenter VM deleted: %s\n", name)
	return nil
}

func deleteDatastoreFileForInterrupt(ctx context.Context, placement *placement, remotePath, displayPath, label string) error {
	if strings.TrimSpace(displayPath) == "" {
		displayPath = placement.Datastore.Path(remotePath)
	}
	fmt.Printf("Deleting interrupted vCenter %s: %s\n", label, displayPath)
	manager := placement.Datastore.NewFileManager(placement.Datacenter, true)
	if err := manager.Delete(ctx, remotePath); err != nil {
		if isMissingDatastoreFile(err) {
			fmt.Printf("OK interrupted vCenter %s already absent: %s\n", label, displayPath)
			return nil
		}
		return err
	}
	fmt.Printf("OK interrupted vCenter %s deleted: %s\n", label, displayPath)
	return nil
}

func isMissingDatastoreFile(err error) bool {
	if err == nil {
		return false
	}
	return isMissingConsoleLog(err)
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
