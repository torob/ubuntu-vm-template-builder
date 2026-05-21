package seediso

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"ubuntu-vm-template-builder/internal/common"
	"ubuntu-vm-template-builder/internal/isoutil"
	"ubuntu-vm-template-builder/internal/offlineapt"
)

func TestSupportFileMappingsIncludeScriptsAndOfflineInstallConfig(t *testing.T) {
	install := offlineapt.InstallConfig{
		Packages: []string{"git"},
		Sources:  []offlineapt.RepositorySource{{Suite: "noble", Components: []string{"main"}}},
	}

	mappings, err := SupportFileMappings(t.TempDir(), Options{ExtraPackages: install})
	if err != nil {
		t.Fatalf("SupportFileMappings returned error: %v", err)
	}

	scriptDir := localPathForISOPath(t, mappings, ScriptsISOPath)
	configDir := localPathForISOPath(t, mappings, offlineapt.ISOInstallConfigPath)
	for _, name := range []string{
		installOfflinePackagesScriptName,
		cleanupInstalledGRUBScriptName,
		prepareCloudInitTemplateScriptName,
	} {
		data, err := os.ReadFile(filepath.Join(scriptDir, name))
		if err != nil {
			t.Fatalf("read support script %s: %v", name, err)
		}
		if !strings.HasPrefix(string(data), "#!/bin/sh\n") {
			t.Fatalf("support script %s missing shell header:\n%s", name, data)
		}
	}

	packages, err := os.ReadFile(filepath.Join(configDir, "packages"))
	if err != nil {
		t.Fatalf("read offline install package config: %v", err)
	}
	if string(packages) != "git\n" {
		t.Fatalf("packages config = %q", packages)
	}
}

func TestTransformUserDataUsesInjectedSupportScripts(t *testing.T) {
	original := []byte(`#cloud-config
autoinstall:
  version: 1
  late-commands:
    - echo user command
`)
	install := offlineapt.InstallConfig{
		Packages: []string{"git"},
		Sources:  []offlineapt.RepositorySource{{Suite: "noble", Components: []string{"main"}}},
	}

	transformed, err := TransformUserDataWithOptions(original, Options{ExtraPackages: install})
	if err != nil {
		t.Fatalf("TransformUserDataWithOptions returned error: %v", err)
	}
	text := string(transformed)
	for _, want := range []string{
		"/cdrom/ubuntu-vm-template-builder/scripts/install-offline-packages.sh",
		"/cdrom/ubuntu-vm-template-builder/scripts/prepare-cloud-init-template.sh",
		"/cdrom/ubuntu-vm-template-builder/scripts/cleanup-installed-grub.sh",
		"echo user command",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("transformed user-data missing %q in:\n%s", want, text)
		}
	}
	for _, absent := range []string{
		"cloud-init.disabled",
		"GRUB_CMDLINE_LINUX_DEFAULT",
		"ssh_deletekeys",
		"curtin in-target --target=/target -- apt-get",
	} {
		if strings.Contains(text, absent) {
			t.Fatalf("transformed user-data contains inline script detail %q:\n%s", absent, text)
		}
	}

	autoinstall, err := common.ParseAutoinstallMapping(transformed)
	if err != nil {
		t.Fatalf("parse transformed user-data: %v", err)
	}
	commands := lateCommandValues(t, autoinstall)
	wantCommands := append(append([]string{}, offlineapt.InstallLateCommands(install)...), "echo user command")
	wantCommands = append(wantCommands, BuilderSupportLateCommands()...)
	if strings.Join(commands, "\n") != strings.Join(wantCommands, "\n") {
		t.Fatalf("late-commands = %#v, want %#v", commands, wantCommands)
	}
}

func localPathForISOPath(t *testing.T, mappings []isoutil.FileMapping, isoPath string) string {
	t.Helper()
	for _, mapping := range mappings {
		if mapping.ISOPath == isoPath {
			return mapping.LocalPath
		}
	}
	t.Fatalf("mapping for ISO path %s not found: %#v", isoPath, mappings)
	return ""
}

func lateCommandValues(t *testing.T, autoinstall *yaml.Node) []string {
	t.Helper()
	lateCommands := common.MappingValue(autoinstall, "late-commands")
	if lateCommands == nil || lateCommands.Kind != yaml.SequenceNode {
		t.Fatalf("late-commands = %#v, want sequence", lateCommands)
	}
	var commands []string
	for _, node := range lateCommands.Content {
		if node.Kind != yaml.ScalarNode {
			t.Fatalf("late-command node = %#v, want scalar", node)
		}
		commands = append(commands, node.Value)
	}
	return commands
}
