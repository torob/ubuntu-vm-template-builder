package proxmox

import (
	"bytes"
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

	"github.com/gorilla/websocket"

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

func TestTransientConsoleStreamErrorClassification(t *testing.T) {
	transient := []error{
		context.Canceled,
		context.DeadlineExceeded,
		io.EOF,
		io.ErrUnexpectedEOF,
		&websocket.CloseError{Code: websocket.CloseNormalClosure, Text: "normal"},
		&websocket.CloseError{Code: websocket.CloseGoingAway, Text: "going away"},
		&websocket.CloseError{Code: websocket.CloseAbnormalClosure, Text: "unexpected EOF"},
		errors.New("websocket: close 1006 (abnormal closure): unexpected EOF"),
	}
	for _, err := range transient {
		if !isTransientConsoleStreamError(err) {
			t.Fatalf("isTransientConsoleStreamError(%v) = false, want true", err)
		}
	}
	if isTransientConsoleStreamError(errors.New("permission denied")) {
		t.Fatal("permission denied was classified as transient")
	}
}

func TestStreamSerialConsoleUntilStoppedReconnectsTransientClose(t *testing.T) {
	restore := setFastPollIntervals()
	defer restore()

	fake := newFakeAPI()
	fake.streamErrors = []error{io.ErrUnexpectedEOF, nil}
	fake.streamOutputs = []string{"first", "second"}
	fake.statuses = []string{"running"}

	var out bytes.Buffer
	if err := streamSerialConsoleUntilStopped(context.Background(), fake, "pve", 100, &out); err != nil {
		t.Fatalf("streamSerialConsoleUntilStopped returned error: %v", err)
	}
	if got := countCalls(fake.calls, "stream-console"); got != 2 {
		t.Fatalf("stream-console calls = %d, want 2; calls=%v", got, fake.calls)
	}
	if got := out.String(); !strings.Contains(got, "first\n") || !strings.Contains(got, "second\n") {
		t.Fatalf("console output after reconnect = %q", got)
	}
}

func TestStreamSerialConsoleUntilStoppedStopsAfterVMStops(t *testing.T) {
	restore := setFastPollIntervals()
	defer restore()

	fake := newFakeAPI()
	fake.streamErrors = []error{&websocket.CloseError{Code: websocket.CloseAbnormalClosure, Text: "unexpected EOF"}}
	fake.statuses = []string{"stopped"}

	if err := streamSerialConsoleUntilStopped(context.Background(), fake, "pve", 100, io.Discard); err != nil {
		t.Fatalf("streamSerialConsoleUntilStopped returned error after stopped VM: %v", err)
	}
	if got := countCalls(fake.calls, "stream-console"); got != 1 {
		t.Fatalf("stream-console calls = %d, want 1; calls=%v", got, fake.calls)
	}
}

func TestStreamSerialConsoleUntilStoppedReturnsNonTransientError(t *testing.T) {
	restore := setFastPollIntervals()
	defer restore()

	fake := newFakeAPI()
	fake.streamErrors = []error{errors.New("permission denied")}

	err := streamSerialConsoleUntilStopped(context.Background(), fake, "pve", 100, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("streamSerialConsoleUntilStopped error = %v, want permission denied", err)
	}
	if got := countCalls(fake.calls, "stream-console"); got != 1 {
		t.Fatalf("stream-console calls = %d, want 1; calls=%v", got, fake.calls)
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
			if r.ContentLength <= int64(len("iso bytes")) {
				t.Fatalf("upload ContentLength = %d, want multipart body length", r.ContentLength)
			}
			if len(r.TransferEncoding) != 0 {
				t.Fatalf("upload used transfer encoding %v, want fixed content length", r.TransferEncoding)
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

func TestStorageUploadBodyDoesNotReadSourceDuringConstruction(t *testing.T) {
	source := &countingReader{}
	body, contentType, contentLength, err := newStorageUploadBody(StorageUpload{
		FileName: "uploaded.iso",
		Content:  UploadContentISO,
	}, source, 5*1024*1024*1024)
	if err != nil {
		t.Fatalf("newStorageUploadBody returned error: %v", err)
	}
	if source.reads != 0 {
		t.Fatalf("source reads during body construction = %d, want 0", source.reads)
	}
	if !strings.HasPrefix(contentType, "multipart/form-data; boundary=") {
		t.Fatalf("content type = %q, want multipart/form-data", contentType)
	}
	if contentLength <= 5*1024*1024*1024 {
		t.Fatalf("content length = %d, want source size plus multipart framing", contentLength)
	}
	_ = body
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

func TestLoadOptionsConfigStrictAndSerializesExplicitFalse(t *testing.T) {
	path := writeTempYAML(t, "options.yaml", `
start_at_boot: false
qemu_guest_agent:
  enabled: false
protection: false
tablet: true
tags:
  - ubuntu
  - template
description: example template
`)
	options, err := LoadOptionsConfig(path)
	if err != nil {
		t.Fatalf("LoadOptionsConfig returned error: %v", err)
	}
	if !options.Enabled || options.StartAtBoot == nil || *options.StartAtBoot {
		t.Fatalf("loaded options did not preserve explicit false: %+v", options)
	}

	cfg := testProxmoxConfig()
	cfg.Options = options
	values, err := BuildOptionsValues(cfg)
	if err != nil {
		t.Fatalf("BuildOptionsValues returned error: %v", err)
	}
	want := map[string]string{
		"onboot":      "0",
		"agent":       "enabled=0",
		"protection":  "0",
		"tablet":      "1",
		"tags":        "ubuntu;template",
		"description": "example template",
	}
	for key, wantValue := range want {
		if got := values.Get(key); got != wantValue {
			t.Fatalf("values[%s] = %q, want %q; all values: %v", key, got, wantValue, values)
		}
	}
}

func TestLoadOptionsConfigRejectsUnknownField(t *testing.T) {
	path := writeTempYAML(t, "options.yaml", "boot: order=scsi0\n")
	if _, err := LoadOptionsConfig(path); err == nil {
		t.Fatal("LoadOptionsConfig returned nil error for unknown field")
	} else if !strings.Contains(err.Error(), "field boot not found") {
		t.Fatalf("unknown field error = %v", err)
	}
}

func TestBuildOptionsValuesSerializesTypedFields(t *testing.T) {
	cfg := testProxmoxConfig()
	cfg.Options = OptionsConfig{
		StartAtBoot: boolPtr(true),
		Startup: StartupOptions{
			Order:            intPtr(10),
			UpDelaySeconds:   intPtr(30),
			DownDelaySeconds: intPtr(60),
		},
		QEMUGuestAgent: QEMUGuestAgentOptions{
			Enabled:           boolPtr(true),
			FreezeFSOnBackup:  boolPtr(false),
			FstrimClonedDisks: boolPtr(true),
			Type:              "virtio",
		},
		ACPI:               boolPtr(true),
		KVM:                boolPtr(true),
		FreezeCPUAtStartup: boolPtr(false),
		LocalTime:          boolPtr(false),
		RTCStartDate:       "now",
		Hotplug: HotplugOptions{
			Network:   boolPtr(true),
			Disk:      boolPtr(true),
			USB:       boolPtr(true),
			Memory:    boolPtr(false),
			CPU:       boolPtr(false),
			CloudInit: boolPtr(true),
		},
		SMBIOS: SMBIOSOptions{
			ValuesAreBase64: boolPtr(false),
			UUID:            "11111111-2222-3333-4444-555555555555",
			Manufacturer:    "Canonical",
			Product:         "Ubuntu",
			Version:         "24.04",
			Serial:          "serial",
			SKU:             "sku",
			Family:          "linux",
		},
		SPICEEnhancements: SPICEEnhancementsOptions{
			FolderSharing:  boolPtr(true),
			VideoStreaming: "filter",
		},
		VMStateStorage: "vms",
	}

	values, err := BuildOptionsValues(cfg)
	if err != nil {
		t.Fatalf("BuildOptionsValues returned error: %v", err)
	}
	want := map[string]string{
		"onboot":             "1",
		"startup":            "order=10,up=30,down=60",
		"agent":              "enabled=1,freeze-fs-on-backup=0,fstrim_cloned_disks=1,type=virtio",
		"acpi":               "1",
		"kvm":                "1",
		"freeze":             "0",
		"localtime":          "0",
		"startdate":          "now",
		"hotplug":            "network,disk,usb,cloudinit",
		"smbios1":            "base64=0,uuid=11111111-2222-3333-4444-555555555555,manufacturer=Canonical,product=Ubuntu,version=24.04,serial=serial,sku=sku,family=linux",
		"spice_enhancements": "foldersharing=1,videostreaming=filter",
		"vmstatestorage":     "vms",
	}
	for key, wantValue := range want {
		if got := values.Get(key); got != wantValue {
			t.Fatalf("values[%s] = %q, want %q; all values: %v", key, got, wantValue, values)
		}
	}
}

func TestLoadCloudInitOptionsConfigStrictAndSerializes(t *testing.T) {
	path := writeTempYAML(t, "cloud-init.yaml", `
type: nocloud
upgrade: false
user: ubuntu
password: secret
ssh_keys:
  - ssh-ed25519 AAAA first@example
  - ssh-ed25519 BBBB second@example
dns:
  nameservers:
    - 1.1.1.1
    - 8.8.8.8
  search_domains:
    - example.com
network:
  - index: 0
    ipv4: dhcp
    ipv6: auto
  - index: 1
    ipv4: 192.0.2.10/24
    gateway4: 192.0.2.1
custom:
  user: local:snippets/user-data.yaml
  network: local:snippets/network-data.yaml
  meta: local:snippets/meta-data.yaml
  vendor: local:snippets/vendor-data.yaml
`)
	cloudInit, err := LoadCloudInitOptionsConfig(path)
	if err != nil {
		t.Fatalf("LoadCloudInitOptionsConfig returned error: %v", err)
	}

	cfg := testProxmoxConfig()
	cfg.CloudInitOptions = cloudInit
	values, err := BuildCloudInitValues(cfg)
	if err != nil {
		t.Fatalf("BuildCloudInitValues returned error: %v", err)
	}
	want := map[string]string{
		"ide2":         "vms:cloudinit",
		"citype":       "nocloud",
		"ciupgrade":    "0",
		"ciuser":       "ubuntu",
		"cipassword":   "secret",
		"sshkeys":      "ssh-ed25519%20AAAA%20first%40example%0Assh-ed25519%20BBBB%20second%40example",
		"nameserver":   "1.1.1.1 8.8.8.8",
		"searchdomain": "example.com",
		"ipconfig0":    "ip=dhcp,ip6=auto",
		"ipconfig1":    "ip=192.0.2.10/24,gw=192.0.2.1",
		"cicustom":     "meta=local:snippets/meta-data.yaml,network=local:snippets/network-data.yaml,user=local:snippets/user-data.yaml,vendor=local:snippets/vendor-data.yaml",
	}
	for key, wantValue := range want {
		if got := values.Get(key); got != wantValue {
			t.Fatalf("values[%s] = %q, want %q; all values: %v", key, got, wantValue, values)
		}
	}
}

func TestLoadCloudInitOptionsConfigRejectsInvalidNetwork(t *testing.T) {
	path := writeTempYAML(t, "cloud-init.yaml", `
type: nocloud
network:
  - index: 0
    ipv4: dhcp
  - index: 0
    ipv4: dhcp
`)
	if _, err := LoadCloudInitOptionsConfig(path); err == nil {
		t.Fatal("LoadCloudInitOptionsConfig returned nil error for duplicated network index")
	} else if !strings.Contains(err.Error(), "duplicated") {
		t.Fatalf("duplicate network index error = %v", err)
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

func TestPreflightBuildStoragesIncludesCloudInitDisk(t *testing.T) {
	fake := newFakeAPI()
	fake.storageStatuses["vms"] = StorageStatus{
		Active:  1,
		Enabled: 1,
		Content: "images",
		Avail:   flexibleInt64{Value: 20 * 1024 * 1024 * 1024, Set: true},
	}
	buildCfg := testProxmoxConfig()
	buildCfg.CloudInitOptions = CloudInitOptionsConfig{Enabled: true}
	isoPath := writeTempFile(t, "installer.iso", 1024)

	err := preflightBuildStorages(context.Background(), fake, testProxmoxConnection(), buildCfg, isoPath)
	if err == nil {
		t.Fatal("preflightBuildStorages returned nil error when disk storage lacked cloud-init capacity")
	}
	if !strings.Contains(err.Error(), "disk+cloud-init") || !strings.Contains(err.Error(), "insufficient capacity") {
		t.Fatalf("cloud-init capacity preflight error = %v", err)
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

func TestInstallFlowAppliesCloudInitThenOptionsBeforeTemplate(t *testing.T) {
	restore := setFastPollIntervals()
	defer restore()

	fake := newFakeAPI()
	cfg := testProxmoxConfig()
	cfg.CloudInitOptions = CloudInitOptionsConfig{
		Enabled: true,
		Type:    "nocloud",
		Upgrade: boolPtr(false),
		User:    "ubuntu",
		Network: []CloudInitIPConfig{
			{Index: 0, IPv4: "dhcp"},
		},
	}
	cfg.Options = OptionsConfig{
		Enabled:     true,
		StartAtBoot: boolPtr(false),
		Protection:  boolPtr(true),
		Tags:        []string{"ubuntu", "template"},
	}
	installer := &Installer{cfg: cfg}
	err := installer.installWithClient(context.Background(), fake, func(context.Context) (string, error) {
		return writeTempFile(t, "installer.iso", 1024), nil
	})
	if err != nil {
		t.Fatalf("installWithClient returned error: %v", err)
	}
	if len(fake.updateValuesHistory) != 3 {
		t.Fatalf("update history length = %d, want 3: %v", len(fake.updateValuesHistory), fake.updateValuesHistory)
	}
	if got := fake.updateValuesHistory[0].Get("delete"); got != "ide2,serial0" {
		t.Fatalf("finalize delete = %q, want ide2,serial0", got)
	}
	if got := fake.updateValuesHistory[1].Get("ide2"); got != "vms:cloudinit" {
		t.Fatalf("cloud-init ide2 = %q, want vms:cloudinit; values=%v", got, fake.updateValuesHistory[1])
	}
	if got := fake.updateValuesHistory[1].Get("ipconfig0"); got != "ip=dhcp" {
		t.Fatalf("cloud-init ipconfig0 = %q, want ip=dhcp; values=%v", got, fake.updateValuesHistory[1])
	}
	if got := fake.updateValuesHistory[2].Get("protection"); got != "1" {
		t.Fatalf("options protection = %q, want 1; values=%v", got, fake.updateValuesHistory[2])
	}
	assertCallsContainInOrder(t, fake.calls, []string{
		"update-config",
		"wait-task:update",
		"update-config",
		"wait-task:update",
		"update-config",
		"wait-task:update",
		"template-vm",
	})
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

func writeTempYAML(t *testing.T, name, data string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(strings.TrimSpace(data)+"\n"), 0o644); err != nil {
		t.Fatalf("write temp YAML: %v", err)
	}
	return path
}

func boolPtr(value bool) *bool {
	return &value
}

func intPtr(value int) *int {
	return &value
}

func setFastPollIntervals() func() {
	oldTask := taskPollInterval
	oldPower := vmPowerPollInterval
	oldConsoleReconnect := consoleReconnectDelay
	taskPollInterval = time.Millisecond
	vmPowerPollInterval = time.Millisecond
	consoleReconnectDelay = time.Millisecond
	return func() {
		taskPollInterval = oldTask
		vmPowerPollInterval = oldPower
		consoleReconnectDelay = oldConsoleReconnect
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

func countCalls(calls []string, want string) int {
	count := 0
	for _, call := range calls {
		if call == want {
			count++
		}
	}
	return count
}

type countingReader struct {
	reads int
}

func (r *countingReader) Read([]byte) (int, error) {
	r.reads++
	return 0, io.EOF
}

type fakeAPI struct {
	calls               []string
	vms                 []VMInfo
	nextID              int
	nextIDCalls         int
	storageStatuses     map[string]StorageStatus
	storageStatusCalls  []string
	volumes             map[string]bool
	uploads             []StorageUpload
	deletedVolumes      []string
	createValues        url.Values
	updateValues        url.Values
	updateValuesHistory []url.Values
	templateCalls       int
	startCalls          int
	statuses            []string
	taskErrors          map[string]error
	stoppedVMs          []int
	deletedVMs          []int
	streamErrors        []error
	streamOutputs       []string
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
	f.updateValuesHistory = append(f.updateValuesHistory, cloneValues(values))
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
	output := "console"
	if len(f.streamOutputs) > 0 {
		output = f.streamOutputs[0]
		f.streamOutputs = f.streamOutputs[1:]
	}
	if out != nil {
		_, _ = fmt.Fprintln(out, output)
	}
	if len(f.streamErrors) == 0 {
		return nil
	}
	err := f.streamErrors[0]
	f.streamErrors = f.streamErrors[1:]
	return err
}

func cloneValues(values url.Values) url.Values {
	out := make(url.Values, len(values))
	for key, value := range values {
		out[key] = append([]string(nil), value...)
	}
	return out
}
