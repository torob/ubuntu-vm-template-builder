package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestMatrixConfigDefaultsAndExpansion(t *testing.T) {
	cfg := matrixConfig{
		UbuntuVersions: []ubuntuVersion{
			{Name: "24.04", ISOURL: "https://releases.ubuntu.com/24.04/ubuntu-24.04-live-server-amd64.iso"},
			{Name: "26.04", ISOURL: "https://releases.ubuntu.com/26.04/ubuntu-26.04-live-server-amd64.iso"},
		},
	}
	if err := cfg.normalizeAndValidate(); err != nil {
		t.Fatalf("normalizeAndValidate returned error: %v", err)
	}
	if cfg.ISODir != defaultISODir {
		t.Fatalf("ISODir = %q, want %q", cfg.ISODir, defaultISODir)
	}
	if cfg.WorkDir != defaultWorkDir {
		t.Fatalf("WorkDir = %q, want %q", cfg.WorkDir, defaultWorkDir)
	}
	if cfg.DiskSize != defaultDiskSize {
		t.Fatalf("DiskSize = %q, want %q", cfg.DiskSize, defaultDiskSize)
	}
	if cfg.Concurrency != 1 {
		t.Fatalf("Concurrency = %d, want 1", cfg.Concurrency)
	}
	if cfg.Keep != defaultKeep {
		t.Fatalf("Keep = %q, want %q", cfg.Keep, defaultKeep)
	}

	cases := cfg.expandCases()
	if len(cases) != 12 {
		t.Fatalf("expanded case count = %d, want 12", len(cases))
	}
	if cases[0].ID() != "ubuntu-24.04-raw-uefi" {
		t.Fatalf("first case ID = %q", cases[0].ID())
	}
	if cases[11].ID() != "ubuntu-26.04-vmdk-bios" {
		t.Fatalf("last case ID = %q", cases[11].ID())
	}
}

func TestMatrixConfigValidationErrors(t *testing.T) {
	tests := []struct {
		name string
		cfg  matrixConfig
		want string
	}{
		{
			name: "missing versions",
			cfg:  matrixConfig{},
			want: "ubuntu_versions",
		},
		{
			name: "missing URL",
			cfg: matrixConfig{
				UbuntuVersions: []ubuntuVersion{{Name: "26.04"}},
			},
			want: "iso_url",
		},
		{
			name: "invalid disk",
			cfg: matrixConfig{
				DiskFormats:    []string{"vdi"},
				UbuntuVersions: []ubuntuVersion{{Name: "26.04", ISOURL: "https://example.com/ubuntu.iso"}},
			},
			want: "unsupported disk format",
		},
		{
			name: "invalid boot",
			cfg: matrixConfig{
				BootModes:      []string{"legacy"},
				UbuntuVersions: []ubuntuVersion{{Name: "26.04", ISOURL: "https://example.com/ubuntu.iso"}},
			},
			want: "unsupported boot mode",
		},
		{
			name: "invalid keep",
			cfg: matrixConfig{
				Keep:           "sometimes",
				UbuntuVersions: []ubuntuVersion{{Name: "26.04", ISOURL: "https://example.com/ubuntu.iso"}},
			},
			want: "unsupported keep policy",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.cfg.normalizeAndValidate()
			if err == nil {
				t.Fatal("normalizeAndValidate returned nil error")
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error %q does not contain %q", err.Error(), test.want)
			}
		})
	}
}

func TestLoadMatrixConfig(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "matrix.yaml")
	data := []byte(`
iso_dir: cache
work_dir: runs
disk_size: 25G
concurrency: 3
boot_modes: [uefi]
disk_formats: [qcow2]
keep: all
ubuntu_versions:
  - name: "26.04"
    iso_url: "https://example.com/ubuntu.iso"
    sha256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
`)
	if err := os.WriteFile(configPath, data, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := loadMatrixConfig(configPath)
	if err != nil {
		t.Fatalf("loadMatrixConfig returned error: %v", err)
	}
	if err := cfg.normalizeAndValidate(); err != nil {
		t.Fatalf("normalizeAndValidate returned error: %v", err)
	}
	if cfg.ISODir != "cache" || cfg.WorkDir != "runs" || cfg.DiskSize != "25G" || cfg.Concurrency != 3 {
		t.Fatalf("unexpected config after validation: %+v", cfg)
	}
	if len(cfg.expandCases()) != 1 {
		t.Fatalf("expanded cases = %d, want 1", len(cfg.expandCases()))
	}
}

func TestGenerateUserDataUEFI(t *testing.T) {
	data := generateUserData(bootModeUEFI, "e2e-host", "ssh-ed25519 AAAATEST e2e")
	for _, want := range []string{
		"hostname: e2e-host",
		"username: e2euser",
		"allow-pw: true",
		"flag: boot",
		"path: /boot/efi",
		"grub-install --target=x86_64-efi",
		"--removable",
		"ssh-ed25519 AAAATEST e2e",
	} {
		if !strings.Contains(data, want) {
			t.Fatalf("UEFI user-data missing %q in:\n%s", want, data)
		}
	}
	if strings.Contains(data, "bios_grub") {
		t.Fatalf("UEFI user-data contains BIOS partition:\n%s", data)
	}
}

func TestGenerateUserDataBIOS(t *testing.T) {
	data := generateUserData(bootModeBIOS, "e2e-host", "ssh-ed25519 AAAATEST e2e")
	for _, want := range []string{
		"hostname: e2e-host",
		"flag: bios_grub",
		"grub_device: true",
		"ssh-ed25519 AAAATEST e2e",
	} {
		if !strings.Contains(data, want) {
			t.Fatalf("BIOS user-data missing %q in:\n%s", want, data)
		}
	}
	if strings.Contains(data, "/boot/efi") || strings.Contains(data, "--removable") {
		t.Fatalf("BIOS user-data contains UEFI-only config:\n%s", data)
	}
}

func TestGenerateUserDataIsValidYAML(t *testing.T) {
	for _, mode := range []string{bootModeUEFI, bootModeBIOS} {
		t.Run(mode, func(t *testing.T) {
			var root yaml.Node
			data := generateUserData(mode, "e2e-host", "ssh-ed25519 AAAATEST e2e")
			if err := yaml.Unmarshal([]byte(data), &root); err != nil {
				t.Fatalf("generated user-data is not valid YAML: %v\n%s", err, data)
			}
			if len(root.Content) == 0 || root.Content[0].Kind != yaml.MappingNode {
				t.Fatalf("generated user-data root is not a mapping:\n%s", data)
			}
		})
	}
}

func TestSafeHostname(t *testing.T) {
	got := safeHostname("Ubuntu 26.04_QCOW2_UEFI")
	if got != "ubuntu-26-04-qcow2-uefi" {
		t.Fatalf("safeHostname = %q", got)
	}

	long := safeHostname(strings.Repeat("a", 80))
	if len(long) > 63 {
		t.Fatalf("safeHostname length = %d, want <= 63", len(long))
	}
}

func TestShellQuote(t *testing.T) {
	got := shellQuote("can't")
	want := `'can'"'"'t'`
	if got != want {
		t.Fatalf("shellQuote = %q, want %q", got, want)
	}
}
