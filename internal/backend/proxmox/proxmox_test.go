package proxmox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"ubuntu-vm-template-builder/internal/common"
)

func TestBuildURLDefaultsAPIPath(t *testing.T) {
	u, err := BuildURL(ConnectionConfig{Host: "pve.example.com:8006"})
	if err != nil {
		t.Fatalf("BuildURL returned error: %v", err)
	}
	if u.String() != "https://pve.example.com:8006/api2/json" {
		t.Fatalf("BuildURL = %q", u.String())
	}

	u, err = BuildURL(ConnectionConfig{Host: "https://pve.example.com:8006/custom"})
	if err != nil {
		t.Fatalf("BuildURL with custom path returned error: %v", err)
	}
	if u.String() != "https://pve.example.com:8006/custom/api2/json" {
		t.Fatalf("BuildURL with custom path = %q", u.String())
	}
}

func TestClientAddsTokenAuthorizationHeader(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api2/json/nodes/pve/qemu" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		if got, want := r.Header.Get("Authorization"), "PVEAPIToken=root@pam!builder=secret"; got != want {
			t.Fatalf("Authorization = %q, want %q", got, want)
		}
		writeData(t, w, []map[string]any{})
	}))
	defer server.Close()

	client, err := NewClient(ConnectionConfig{
		Host:        server.URL,
		TokenID:     "root@pam!builder",
		TokenSecret: "secret",
	})
	if err != nil {
		t.Fatalf("NewClient returned error: %v", err)
	}
	if _, err := client.ListVMs(context.Background(), "pve"); err != nil {
		t.Fatalf("ListVMs returned error: %v", err)
	}
}

func TestClientAPIURLDoesNotDoubleEscapePathSegments(t *testing.T) {
	client, err := NewClient(ConnectionConfig{Host: "https://pve.example.com:8006", TokenID: "id", TokenSecret: "secret"})
	if err != nil {
		t.Fatalf("NewClient returned error: %v", err)
	}
	upid := "UPID:node:001:002:task::user@pve!token:"
	u := client.apiURL("/nodes/pve/tasks/" + url.PathEscape(upid) + "/status")
	if strings.Contains(u.String(), "%25") {
		t.Fatalf("apiURL double-escaped UPID path: %s", u.String())
	}
	if !strings.Contains(u.String(), "user@pve%21token") {
		t.Fatalf("apiURL did not keep ! escaped once: %s", u.String())
	}

	volume := "local:iso/file.iso"
	u = client.apiURL("/nodes/pve/storage/local/content/" + url.PathEscape(volume))
	if strings.Contains(u.String(), "%25") || !strings.Contains(u.String(), "local:iso%2Ffile.iso") {
		t.Fatalf("apiURL volume path escaping is unexpected: %s", u.String())
	}
}

func TestFlexibleInt64AcceptsNumbersAndStrings(t *testing.T) {
	for _, input := range []string{`{"value":5900}`, `{"value":"5900"}`} {
		t.Run(input, func(t *testing.T) {
			var got struct {
				Value flexibleInt64 `json:"value"`
			}
			if err := json.Unmarshal([]byte(input), &got); err != nil {
				t.Fatalf("Unmarshal returned error: %v", err)
			}
			if !got.Value.Set || got.Value.Value != 5900 {
				t.Fatalf("decoded value = %+v, want 5900", got.Value)
			}
		})
	}
}

func TestConsoleHandshakeUsesAPITokenID(t *testing.T) {
	client := &Client{tokenID: "root@pam!builder"}
	if got, want := client.consoleHandshake("PVEVNC:ticket"), "root@pam!builder:PVEVNC:ticket\n"; got != want {
		t.Fatalf("consoleHandshake = %q, want %q", got, want)
	}
	client.tokenID = ""
	if got, want := client.consoleHandshake("PVEVNC:ticket"), "PVEVNC:ticket\n"; got != want {
		t.Fatalf("consoleHandshake without token = %q, want %q", got, want)
	}
	if got := client.consoleHandshake(" "); got != "" {
		t.Fatalf("empty ticket handshake = %q, want empty", got)
	}
}

func TestWaitTaskReportsTaskFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeData(t, w, map[string]any{
			"status":     "stopped",
			"exitstatus": "storage error",
		})
	}))
	defer server.Close()

	client, err := NewClient(ConnectionConfig{Host: server.URL, TokenID: "id", TokenSecret: "secret"})
	if err != nil {
		t.Fatalf("NewClient returned error: %v", err)
	}
	err = client.WaitTask(context.Background(), "pve", "UPID:test")
	if err == nil {
		t.Fatal("WaitTask returned nil error for failed task")
	}
	if !strings.Contains(err.Error(), "storage error") {
		t.Fatalf("WaitTask error = %v", err)
	}
}

func TestUploadFileToStorageThroughHTTPClient(t *testing.T) {
	var uploadedName string
	var uploadedContent string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "PVEAPIToken=root@pam!builder=secret" {
			t.Fatalf("missing token authorization header: %q", r.Header.Get("Authorization"))
		}
		switch r.URL.Path {
		case "/api2/json/nodes/pve/storage/local/upload":
			if r.Method != http.MethodPost {
				t.Fatalf("upload method = %s, want POST", r.Method)
			}
			reader, err := r.MultipartReader()
			if err != nil {
				t.Fatalf("MultipartReader: %v", err)
			}
			uploadedName, uploadedContent = readUploadMultipart(t, reader)
			writeData(t, w, "UPID:upload")
		case "/api2/json/nodes/pve/tasks/UPID:upload/status":
			writeData(t, w, map[string]any{"status": "stopped", "exitstatus": "OK"})
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	source := filepath.Join(t.TempDir(), "source.iso")
	if err := os.WriteFile(source, []byte("iso bytes"), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}

	client, err := NewClient(ConnectionConfig{Host: server.URL, TokenID: "root@pam!builder", TokenSecret: "secret"})
	if err != nil {
		t.Fatalf("NewClient returned error: %v", err)
	}
	volumeID, err := client.UploadFile(context.Background(), "pve", "local", StorageUpload{
		SourcePath: source,
		FileName:   "uploaded.iso",
		Content:    UploadContentISO,
	})
	if err != nil {
		t.Fatalf("UploadFile returned error: %v", err)
	}
	if volumeID != "local:iso/uploaded.iso" {
		t.Fatalf("upload volume = %q", volumeID)
	}
	if uploadedName != "uploaded.iso" || uploadedContent != "iso" {
		t.Fatalf("multipart upload name/content = %q/%q", uploadedName, uploadedContent)
	}
}

func TestUploadTemporaryInstallerISORejectsExistingDestination(t *testing.T) {
	source := filepath.Join(t.TempDir(), "source.iso")
	if err := os.WriteFile(source, []byte("iso bytes"), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}

	fake := newFakeAPI()
	fake.volumes["local:iso/existing.iso"] = true
	_, err := uploadTemporaryInstallerISO(context.Background(), fake, testProxmoxConnection(), source, "existing.iso", "local:iso/existing.iso")
	if err == nil {
		t.Fatal("uploadTemporaryInstallerISO returned nil error for existing destination")
	}
	if len(fake.uploads) != 0 || len(fake.deletedVolumes) != 0 {
		t.Fatalf("conflicting temporary upload mutated storage: uploads=%v deletes=%v", fake.uploads, fake.deletedVolumes)
	}
}

func TestBuildCreateVMValuesUsesRequestedHardware(t *testing.T) {
	cfg := testProxmoxConfig()
	cfg.Hardware.BootFirmware = common.BootFirmwareUEFI
	cfg.Hardware.VCPU = 4
	cfg.Hardware.MemoryMB = 8192
	cfg.Hardware.Proxmox.Bridge = "vmbr1"
	cfg.Hardware.Proxmox.NetworkAdapter = "e1000e"
	cfg.Hardware.Proxmox.DiskFormat = "qcow2"
	cfg.Hardware.Proxmox.CPUType = "x86-64-v3"
	cfg.Hardware.Proxmox.PreEnrolledKeys = true
	cfg.Proxmox.Bridge = "vmbr1"

	values, err := BuildCreateVMValues(cfg, 123, "local:iso/installer.iso")
	if err != nil {
		t.Fatalf("BuildCreateVMValues returned error: %v", err)
	}
	want := map[string]string{
		"vmid":     "123",
		"name":     "ubuntu-template",
		"cores":    "4",
		"memory":   "8192",
		"ostype":   "l26",
		"machine":  "q35",
		"cpu":      "x86-64-v3",
		"scsihw":   "virtio-scsi-pci",
		"net0":     "e1000e,bridge=vmbr1",
		"ide2":     "local:iso/installer.iso,media=cdrom",
		"scsi0":    "vms:20,format=qcow2",
		"boot":     "order=ide2;scsi0",
		"serial0":  "socket",
		"vga":      "serial0",
		"bios":     "ovmf",
		"efidisk0": "vms:1,format=qcow2,efitype=4m,pre-enrolled-keys=1",
		"agent":    "1",
		"onboot":   "0",
	}
	for key, wantValue := range want {
		if got := values.Get(key); got != wantValue {
			t.Fatalf("values[%s] = %q, want %q\nall values: %v", key, got, wantValue, values)
		}
	}
}

func TestSelectVMIDAllocatesByDefault(t *testing.T) {
	fake := newFakeAPI()
	fake.nextID = 321
	vmid, err := selectVMID(context.Background(), fake, testProxmoxConnection())
	if err != nil {
		t.Fatalf("selectVMID returned error: %v", err)
	}
	if vmid != 321 || fake.nextIDCalls != 1 {
		t.Fatalf("vmid/calls = %d/%d, want 321/1", vmid, fake.nextIDCalls)
	}

	cfg := testProxmoxConnection()
	cfg.VMID = 654
	vmid, err = selectVMID(context.Background(), fake, cfg)
	if err != nil {
		t.Fatalf("selectVMID with explicit VMID returned error: %v", err)
	}
	if vmid != 654 || fake.nextIDCalls != 1 {
		t.Fatalf("explicit vmid/calls = %d/%d, want 654/1", vmid, fake.nextIDCalls)
	}
}

func TestProxmoxDiskAllocationGiBRoundsUp(t *testing.T) {
	tests := []struct {
		size string
		want int64
	}{
		{size: "20G", want: 20},
		{size: "1T", want: 1024},
		{size: "1536M", want: 2},
		{size: "1", want: 1},
	}
	for _, test := range tests {
		t.Run(test.size, func(t *testing.T) {
			got, err := proxmoxDiskAllocationGiB(test.size)
			if err != nil {
				t.Fatalf("proxmoxDiskAllocationGiB returned error: %v", err)
			}
			if got != test.want {
				t.Fatalf("proxmoxDiskAllocationGiB(%q) = %d, want %d", test.size, got, test.want)
			}
		})
	}
}

func TestPreflightBuildStoragesAllowsSplitStorages(t *testing.T) {
	fake := newFakeAPI()
	cfg := testProxmoxConnection()
	buildCfg := testProxmoxConfig()
	isoPath := writeTempFile(t, "installer.iso", 1024)

	if err := preflightBuildStorages(context.Background(), fake, cfg, buildCfg, isoPath); err != nil {
		t.Fatalf("preflightBuildStorages returned error: %v", err)
	}
	if !reflect.DeepEqual(fake.storageStatusCalls, []string{"local", "vms"}) && !reflect.DeepEqual(fake.storageStatusCalls, []string{"vms", "local"}) {
		t.Fatalf("storage status calls = %v", fake.storageStatusCalls)
	}
}

func TestPreflightBuildStoragesAggregatesSameStorageCapacity(t *testing.T) {
	fake := newFakeAPI()
	cfg := testProxmoxConnection()
	cfg.DiskStorage = cfg.ISOStorage
	fake.storageStatuses["local"] = StorageStatus{
		Active:  1,
		Enabled: 1,
		Content: "iso,images",
		Avail:   flexibleInt64{Value: 20 * 1024 * 1024 * 1024, Set: true},
	}
	buildCfg := testProxmoxConfig()
	isoPath := writeTempFile(t, "installer.iso", 1024)

	err := preflightBuildStorages(context.Background(), fake, cfg, buildCfg, isoPath)
	if err == nil {
		t.Fatal("preflightBuildStorages returned nil error for insufficient aggregated capacity")
	}
	if !strings.Contains(err.Error(), "insufficient capacity") {
		t.Fatalf("preflight error = %v", err)
	}
}

func TestPreflightBuildStoragesRejectsMissingContentTypes(t *testing.T) {
	buildCfg := testProxmoxConfig()
	isoPath := writeTempFile(t, "installer.iso", 1024)

	fake := newFakeAPI()
	fake.storageStatuses["local"] = StorageStatus{Active: 1, Enabled: 1, Content: "images", Avail: flexibleInt64{Value: 1 << 40, Set: true}}
	err := preflightBuildStorages(context.Background(), fake, testProxmoxConnection(), buildCfg, isoPath)
	if err == nil || !strings.Contains(err.Error(), "must allow \"iso\" content") {
		t.Fatalf("ISO content preflight error = %v", err)
	}

	fake = newFakeAPI()
	fake.storageStatuses["vms"] = StorageStatus{Active: 1, Enabled: 1, Content: "iso", Avail: flexibleInt64{Value: 1 << 40, Set: true}}
	err = preflightBuildStorages(context.Background(), fake, testProxmoxConnection(), buildCfg, isoPath)
	if err == nil || !strings.Contains(err.Error(), "must allow \"images\" content") {
		t.Fatalf("images content preflight error = %v", err)
	}
}

func TestPreflightBuildStoragesRejectsInsufficientCapacity(t *testing.T) {
	buildCfg := testProxmoxConfig()
	isoPath := writeTempFile(t, "installer.iso", 2048)

	fake := newFakeAPI()
	fake.storageStatuses["local"] = StorageStatus{Active: 1, Enabled: 1, Content: "iso", Avail: flexibleInt64{Value: 1024, Set: true}}
	err := preflightBuildStorages(context.Background(), fake, testProxmoxConnection(), buildCfg, isoPath)
	if err == nil || !strings.Contains(err.Error(), "insufficient capacity") {
		t.Fatalf("ISO capacity preflight error = %v", err)
	}

	fake = newFakeAPI()
	fake.storageStatuses["vms"] = StorageStatus{Active: 1, Enabled: 1, Content: "images", Avail: flexibleInt64{Value: 1024, Set: true}}
	err = preflightBuildStorages(context.Background(), fake, testProxmoxConnection(), buildCfg, isoPath)
	if err == nil || !strings.Contains(err.Error(), "insufficient capacity") {
		t.Fatalf("disk capacity preflight error = %v", err)
	}
}

func TestInstallFlowBuildsTemplateAndDeletesTempISO(t *testing.T) {
	restore := setFastPollIntervals()
	defer restore()

	fake := newFakeAPI()
	fake.statuses = []string{"running", "stopped"}
	installer := &Installer{cfg: testProxmoxConfig()}
	err := installer.installWithClient(context.Background(), fake, func(context.Context) (string, error) {
		return writeTempFile(t, "installer.iso", 1024), nil
	})
	if err != nil {
		t.Fatalf("installWithClient returned error: %v", err)
	}
	if fake.createValues.Get("vmid") != "100" {
		t.Fatalf("create vmid = %q, want 100", fake.createValues.Get("vmid"))
	}
	if fake.templateCalls != 1 {
		t.Fatalf("templateCalls = %d, want 1", fake.templateCalls)
	}
	if len(fake.deletedVolumes) != 1 || !strings.HasPrefix(fake.deletedVolumes[0], "local:iso/ubuntu-vm-template-builder-ubuntu-template-") {
		t.Fatalf("deleted volumes = %v", fake.deletedVolumes)
	}
	if fake.createValues.Get("scsi0") != "vms:20,format=raw" || fake.createValues.Get("efidisk0") != "vms:1,format=raw,efitype=4m,pre-enrolled-keys=0" {
		t.Fatalf("create storage values = %v", fake.createValues)
	}
	assertCallsContainInOrder(t, fake.calls, []string{
		"validate-node",
		"list-vms",
		"storage-status:local",
		"storage-status:vms",
		"volume-exists",
		"upload",
		"next-id",
		"create-vm",
		"start-vm",
		"stream-console",
		"update-config",
		"template-vm",
		"delete-volume",
	})
}

func TestInstallFlowBuildsVMWithoutTemplate(t *testing.T) {
	restore := setFastPollIntervals()
	defer restore()

	fake := newFakeAPI()
	cfg := testProxmoxConfig()
	cfg.Hardware.Proxmox.OutputType = common.ProxmoxOutputTypeVM
	installer := &Installer{cfg: cfg}
	err := installer.installWithClient(context.Background(), fake, func(context.Context) (string, error) {
		return writeTempFile(t, "installer.iso", 1024), nil
	})
	if err != nil {
		t.Fatalf("installWithClient returned error: %v", err)
	}
	if fake.templateCalls != 0 {
		t.Fatalf("templateCalls = %d, want 0", fake.templateCalls)
	}
	if fake.updateValues.Get("delete") != "ide2,serial0" || fake.updateValues.Get("boot") != "order=scsi0" {
		t.Fatalf("finalize values = %v", fake.updateValues)
	}
}

func TestInstallFlowReportsTaskFailure(t *testing.T) {
	fake := newFakeAPI()
	fake.taskErrors["create"] = errors.New("create failed")
	installer := &Installer{cfg: testProxmoxConfig()}
	err := installer.installWithClient(context.Background(), fake, func(context.Context) (string, error) {
		return writeTempFile(t, "installer.iso", 1024), nil
	})
	if err == nil {
		t.Fatal("installWithClient returned nil error for failed create task")
	}
	if !strings.Contains(err.Error(), "wait for VM creation") {
		t.Fatalf("installWithClient error = %v", err)
	}
	if fake.startCalls != 0 || fake.templateCalls != 0 {
		t.Fatalf("flow continued after create failure: start=%d template=%d", fake.startCalls, fake.templateCalls)
	}
}

func TestCleanupInterruptedBuildDeletesOnlyRecordedResources(t *testing.T) {
	fake := newFakeAPI()
	fake.statuses = []string{"running"}
	fake.volumes["local:iso/created.iso"] = true
	fake.volumes["local:iso/preexisting.iso"] = true

	state := &buildState{
		cfg:             testProxmoxConnection(),
		client:          fake,
		vmid:            222,
		vmCreated:       true,
		tempISOVolumeID: "local:iso/created.iso",
	}
	cleanupInterruptedBuild(context.Background(), state)

	if !reflect.DeepEqual(fake.stoppedVMs, []int{222}) {
		t.Fatalf("stopped VMs = %v, want [222]", fake.stoppedVMs)
	}
	if !reflect.DeepEqual(fake.deletedVMs, []int{222}) {
		t.Fatalf("deleted VMs = %v, want [222]", fake.deletedVMs)
	}
	if !reflect.DeepEqual(fake.deletedVolumes, []string{"local:iso/created.iso"}) {
		t.Fatalf("deleted volumes = %v", fake.deletedVolumes)
	}
	if !fake.volumes["local:iso/preexisting.iso"] {
		t.Fatal("cleanup deleted preexisting volume")
	}
	if state.vmCreated || !state.tempISODeleted {
		t.Fatalf("cleanup state = %#v", state)
	}
}

func writeData(t *testing.T, w http.ResponseWriter, data any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{"data": data}); err != nil {
		t.Fatalf("write JSON response: %v", err)
	}
}

func readUploadMultipart(t *testing.T, reader *multipart.Reader) (string, string) {
	t.Helper()
	var fileName, content string
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("NextPart: %v", err)
		}
		data, err := io.ReadAll(part)
		if err != nil {
			t.Fatalf("read part %s: %v", part.FormName(), err)
		}
		switch part.FormName() {
		case "filename":
			fileName = part.FileName()
			if string(data) != "iso bytes" {
				t.Fatalf("uploaded file data = %q", data)
			}
		case "content":
			content = string(data)
		}
	}
	return fileName, content
}

func testProxmoxConfig() Config {
	hardware := common.DefaultHardwareConfig()
	hardware.BootFirmware = common.BootFirmwareUEFI
	hardware.DiskSize = "20G"
	hardware.Proxmox.Bridge = "vmbr0"
	return Config{
		UbuntuISO:   "ubuntu.iso",
		UserData:    []byte("#cloud-config\nautoinstall:\n  version: 1\n"),
		DiskSize:    "20G",
		DisplayName: "ubuntu-template",
		Hardware:    hardware,
		Proxmox:     testProxmoxConnection(),
	}
}

func testProxmoxConnection() ConnectionConfig {
	return ConnectionConfig{
		Host:        "https://pve.example.com:8006",
		TokenID:     "root@pam!builder",
		TokenSecret: "secret",
		Node:        "pve",
		ISOStorage:  "local",
		DiskStorage: "vms",
		Bridge:      "vmbr0",
		Name:        "ubuntu-template",
	}
}

func writeTempFile(t *testing.T, name string, size int64) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	file, err := os.Create(path)
	if err != nil {
		t.Fatalf("create temp file: %v", err)
	}
	if err := file.Truncate(size); err != nil {
		_ = file.Close()
		t.Fatalf("truncate temp file: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close temp file: %v", err)
	}
	return path
}

func setFastPollIntervals() func() {
	oldTask := taskPollInterval
	oldPower := vmPowerPollInterval
	taskPollInterval = time.Millisecond
	vmPowerPollInterval = time.Millisecond
	return func() {
		taskPollInterval = oldTask
		vmPowerPollInterval = oldPower
	}
}

func assertCallsContainInOrder(t *testing.T, calls, want []string) {
	t.Helper()
	idx := 0
	for _, call := range calls {
		if idx < len(want) && call == want[idx] {
			idx++
		}
	}
	if idx != len(want) {
		t.Fatalf("calls %v do not contain sequence %v", calls, want)
	}
}

type fakeAPI struct {
	calls              []string
	vms                []VMInfo
	nextID             int
	nextIDCalls        int
	storageStatuses    map[string]StorageStatus
	storageStatusCalls []string
	volumes            map[string]bool
	uploads            []StorageUpload
	deletedVolumes     []string
	createValues       url.Values
	updateValues       url.Values
	templateCalls      int
	startCalls         int
	statuses           []string
	taskErrors         map[string]error
	stoppedVMs         []int
	deletedVMs         []int
}

func newFakeAPI() *fakeAPI {
	return &fakeAPI{
		nextID: 100,
		storageStatuses: map[string]StorageStatus{
			"local": {Active: 1, Enabled: 1, Content: "iso", Avail: flexibleInt64{Value: 1 << 40, Set: true}},
			"vms":   {Active: 1, Enabled: 1, Content: "images", Avail: flexibleInt64{Value: 1 << 40, Set: true}},
		},
		volumes:    make(map[string]bool),
		taskErrors: make(map[string]error),
	}
}

func (f *fakeAPI) ValidateNode(context.Context, string) error {
	f.calls = append(f.calls, "validate-node")
	return nil
}

func (f *fakeAPI) StorageStatus(_ context.Context, _, storage string) (StorageStatus, error) {
	f.calls = append(f.calls, "storage-status:"+storage)
	f.storageStatusCalls = append(f.storageStatusCalls, storage)
	status, ok := f.storageStatuses[storage]
	if !ok {
		return StorageStatus{}, fmt.Errorf("storage %q not found", storage)
	}
	return status, nil
}

func (f *fakeAPI) ListVMs(context.Context, string) ([]VMInfo, error) {
	f.calls = append(f.calls, "list-vms")
	return append([]VMInfo(nil), f.vms...), nil
}

func (f *fakeAPI) NextID(context.Context) (int, error) {
	f.calls = append(f.calls, "next-id")
	f.nextIDCalls++
	return f.nextID, nil
}

func (f *fakeAPI) StorageVolumeExists(_ context.Context, _, _, _, volumeID string) (bool, error) {
	f.calls = append(f.calls, "volume-exists")
	return f.volumes[volumeID], nil
}

func (f *fakeAPI) UploadFile(_ context.Context, _, storage string, upload StorageUpload) (string, error) {
	f.calls = append(f.calls, "upload")
	f.uploads = append(f.uploads, upload)
	volumeID := buildVolumeID(storage, upload.Content, upload.FileName)
	f.volumes[volumeID] = true
	return volumeID, nil
}

func (f *fakeAPI) DeleteVolume(_ context.Context, _, _, volumeID string) (string, error) {
	f.calls = append(f.calls, "delete-volume")
	f.deletedVolumes = append(f.deletedVolumes, volumeID)
	delete(f.volumes, volumeID)
	return "delete-volume", nil
}

func (f *fakeAPI) CreateVM(_ context.Context, _ string, values url.Values) (string, error) {
	f.calls = append(f.calls, "create-vm")
	f.createValues = cloneValues(values)
	return "create", nil
}

func (f *fakeAPI) StartVM(context.Context, string, int) (string, error) {
	f.calls = append(f.calls, "start-vm")
	f.startCalls++
	return "start", nil
}

func (f *fakeAPI) CurrentVMStatus(context.Context, string, int) (VMStatus, error) {
	f.calls = append(f.calls, "current-status")
	if len(f.statuses) == 0 {
		return VMStatus{Status: "stopped"}, nil
	}
	status := f.statuses[0]
	f.statuses = f.statuses[1:]
	return VMStatus{Status: status}, nil
}

func (f *fakeAPI) UpdateVMConfig(_ context.Context, _ string, _ int, values url.Values) (string, error) {
	f.calls = append(f.calls, "update-config")
	f.updateValues = cloneValues(values)
	return "update", nil
}

func (f *fakeAPI) TemplateVM(context.Context, string, int) (string, error) {
	f.calls = append(f.calls, "template-vm")
	f.templateCalls++
	return "template", nil
}

func (f *fakeAPI) StopVM(_ context.Context, _ string, vmid int) (string, error) {
	f.calls = append(f.calls, "stop-vm")
	f.stoppedVMs = append(f.stoppedVMs, vmid)
	return "stop", nil
}

func (f *fakeAPI) DeleteVM(_ context.Context, _ string, vmid int) (string, error) {
	f.calls = append(f.calls, "delete-vm")
	f.deletedVMs = append(f.deletedVMs, vmid)
	return "delete-vm", nil
}

func (f *fakeAPI) WaitTask(_ context.Context, _, upid string) error {
	f.calls = append(f.calls, "wait-task:"+upid)
	return f.taskErrors[upid]
}

func (f *fakeAPI) StreamSerialConsole(_ context.Context, _ string, _ int, out io.Writer) error {
	f.calls = append(f.calls, "stream-console")
	if out != nil {
		_, _ = fmt.Fprintln(out, "console")
	}
	return nil
}

func cloneValues(values url.Values) url.Values {
	out := make(url.Values, len(values))
	for key, value := range values {
		out[key] = append([]string(nil), value...)
	}
	return out
}
