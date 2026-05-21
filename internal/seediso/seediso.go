package seediso

import (
	"bytes"
	"context"
	"embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"ubuntu-vm-template-builder/internal/common"
	"ubuntu-vm-template-builder/internal/isoutil"
	"ubuntu-vm-template-builder/internal/offlineapt"
)

//go:embed scripts/*.sh
var supportScripts embed.FS

const (
	NoCloudKernelArg        = "ds=nocloud;s=/cdrom/nocloud/"
	GrubNoCloudKernelArg    = "ds=nocloud\\;s=/cdrom/nocloud/"
	ConsoleTTY0KernelArg    = "console=tty0"
	ConsoleTTYS0KernelArg   = "console=ttyS0,115200n8"
	GrubTimeoutStyleSetting = "set timeout_style=hidden"
	GrubTimeoutSetting      = "set timeout=0"
	SyslinuxPromptSetting   = "prompt 0"
	SyslinuxTimeoutSetting  = "timeout 1"
	BuilderISOPath          = "/ubuntu-vm-template-builder"
	ScriptsISOPath          = BuilderISOPath + "/scripts"
)

const (
	installOfflinePackagesScriptName   = "install-offline-packages.sh"
	cleanupInstalledGRUBScriptName     = "cleanup-installed-grub.sh"
	prepareCloudInitTemplateScriptName = "prepare-cloud-init-template.sh"
)

const (
	cleanupInstalledGRUBScriptISOPath     = ScriptsISOPath + "/" + cleanupInstalledGRUBScriptName
	prepareCloudInitTemplateScriptISOPath = ScriptsISOPath + "/" + prepareCloudInitTemplateScriptName
)

var InstalledGuestGRUBCleanupScript = mustReadSupportScript(cleanupInstalledGRUBScriptName)

type Options struct {
	ExtraPackages offlineapt.InstallConfig
}

func RemasterUbuntuISOWithNoCloud(ctx context.Context, sourceISO, outputISO string, userData []byte, displayName, workDir string, offlineRepoPath string, options Options) error {
	seedDir, err := CreateNoCloudSeedDir(workDir, userData, displayName, options)
	if err != nil {
		return err
	}

	bootMappings, err := prepareBootConfigMappings(sourceISO, workDir)
	if err != nil {
		return err
	}

	supportMappings, err := SupportFileMappings(workDir, options)
	if err != nil {
		return err
	}

	mappings := append([]isoutil.FileMapping{}, supportMappings...)
	mappings = append(mappings, isoutil.FileMapping{LocalPath: seedDir, ISOPath: "/nocloud"})
	if strings.TrimSpace(offlineRepoPath) != "" {
		mappings = append(mappings, isoutil.FileMapping{LocalPath: offlineRepoPath, ISOPath: offlineapt.ISORepoPath})
	}
	mappings = append(mappings, bootMappings...)

	return isoutil.RemasterISO(ctx, sourceISO, outputISO, mappings)
}

func RemasterUbuntuISOWithSupport(ctx context.Context, sourceISO, outputISO, workDir, offlineRepoPath string, options Options) error {
	mappings, err := SupportFileMappings(workDir, options)
	if err != nil {
		return err
	}
	if strings.TrimSpace(offlineRepoPath) != "" {
		mappings = append(mappings, isoutil.FileMapping{LocalPath: offlineRepoPath, ISOPath: offlineapt.ISORepoPath})
	}
	return isoutil.RemasterISO(ctx, sourceISO, outputISO, mappings)
}

func SupportFileMappings(workDir string, options Options) ([]isoutil.FileMapping, error) {
	baseDir := filepath.Join(workDir, "builder-support")
	if err := os.RemoveAll(baseDir); err != nil {
		return nil, fmt.Errorf("reset builder support directory: %w", err)
	}

	scriptsDir := filepath.Join(baseDir, "scripts")
	if err := os.MkdirAll(scriptsDir, 0o700); err != nil {
		return nil, fmt.Errorf("create builder support scripts directory: %w", err)
	}
	for _, name := range []string{
		installOfflinePackagesScriptName,
		cleanupInstalledGRUBScriptName,
		prepareCloudInitTemplateScriptName,
	} {
		data, err := supportScripts.ReadFile("scripts/" + name)
		if err != nil {
			return nil, fmt.Errorf("read embedded support script %s: %w", name, err)
		}
		if err := os.WriteFile(filepath.Join(scriptsDir, name), data, 0o644); err != nil {
			return nil, fmt.Errorf("write builder support script %s: %w", name, err)
		}
	}

	mappings := []isoutil.FileMapping{{LocalPath: scriptsDir, ISOPath: ScriptsISOPath}}
	if options.ExtraPackages.Enabled() {
		configDir := filepath.Join(baseDir, "offline-apt-install")
		if err := offlineapt.WriteInstallConfigDir(configDir, options.ExtraPackages); err != nil {
			return nil, err
		}
		mappings = append(mappings, isoutil.FileMapping{LocalPath: configDir, ISOPath: offlineapt.ISOInstallConfigPath})
	}
	return mappings, nil
}

func CreateNoCloudSeedDir(workDir string, userData []byte, displayName string, options Options) (string, error) {
	seedDir := filepath.Join(workDir, "nocloud")
	if err := os.MkdirAll(seedDir, 0o700); err != nil {
		return "", fmt.Errorf("create NoCloud seed directory: %w", err)
	}

	transformedUserData, err := TransformUserDataWithOptions(userData, options)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(seedDir, "user-data"), transformedUserData, 0o600); err != nil {
		return "", fmt.Errorf("write NoCloud user-data: %w", err)
	}

	instanceName := common.SafeName(displayName)
	metaData := fmt.Sprintf("instance-id: iid-%s\nlocal-hostname: %s\n", instanceName, instanceName)
	if err := os.WriteFile(filepath.Join(seedDir, "meta-data"), []byte(metaData), 0o600); err != nil {
		return "", fmt.Errorf("write NoCloud meta-data: %w", err)
	}

	return seedDir, nil
}

func TransformUserData(userData []byte) ([]byte, error) {
	return TransformUserDataWithOptions(userData, Options{})
}

func TransformUserDataWithOptions(userData []byte, options Options) ([]byte, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(userData, &root); err != nil {
		return nil, fmt.Errorf("parse YAML: %w", err)
	}

	autoinstall, err := common.AutoinstallMappingFromRoot(&root)
	if err != nil {
		return nil, err
	}
	common.SetMappingScalar(autoinstall, "shutdown", "poweroff")
	if err := offlineapt.PrependInstallLateCommands(autoinstall, options.ExtraPackages); err != nil {
		return nil, err
	}
	if err := appendBuilderSupportLateCommands(autoinstall); err != nil {
		return nil, err
	}

	var out bytes.Buffer
	encoder := yaml.NewEncoder(&out)
	encoder.SetIndent(2)
	if err := encoder.Encode(&root); err != nil {
		_ = encoder.Close()
		return nil, fmt.Errorf("encode YAML: %w", err)
	}
	if err := encoder.Close(); err != nil {
		return nil, fmt.Errorf("finish YAML encoding: %w", err)
	}

	return common.EnsureCloudConfigHeader(out.Bytes()), nil
}

func appendBuilderSupportLateCommands(autoinstall *yaml.Node) error {
	lateCommands := common.MappingValue(autoinstall, "late-commands")
	if lateCommands == nil {
		lateCommands = &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		autoinstall.Content = append(autoinstall.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "late-commands"},
			lateCommands,
		)
	} else if lateCommands.Kind != yaml.SequenceNode {
		return errors.New("autoinstall.late-commands must be a sequence when present")
	}

	for _, command := range BuilderSupportLateCommands() {
		lateCommands.Content = append(lateCommands.Content, &yaml.Node{
			Kind:  yaml.ScalarNode,
			Tag:   "!!str",
			Value: command,
		})
	}
	return nil
}

func BuilderSupportLateCommands() []string {
	var commands []string
	commands = append(commands, PrepareCloudInitTemplateLateCommands()...)
	commands = append(commands, InstalledGuestGRUBCleanupLateCommands()...)
	return commands
}

func PrepareCloudInitTemplateLateCommands() []string {
	return []string{scriptLateCommand(prepareCloudInitTemplateScriptISOPath)}
}

func InstalledGuestGRUBCleanupLateCommands() []string {
	return []string{scriptLateCommand(cleanupInstalledGRUBScriptISOPath)}
}

func scriptLateCommand(isoPath string) string {
	return "sh /cdrom" + isoPath + " /target"
}

func mustReadSupportScript(name string) string {
	data, err := supportScripts.ReadFile("scripts/" + name)
	if err != nil {
		panic(err)
	}
	return string(data)
}

func prepareBootConfigMappings(sourceISO, workDir string) ([]isoutil.FileMapping, error) {
	bootConfigPaths := []string{
		"/boot/grub/grub.cfg",
		"/boot/grub/loopback.cfg",
		"/isolinux/txt.cfg",
		"/syslinux/txt.cfg",
	}

	bootDir := filepath.Join(workDir, "boot-config")
	if err := os.MkdirAll(bootDir, 0o700); err != nil {
		return nil, fmt.Errorf("create boot config work directory: %w", err)
	}

	var mappings []isoutil.FileMapping
	foundConfig := false
	for _, isoPath := range bootConfigPaths {
		data, err := common.ReadISOFile(sourceISO, isoPath)
		if err != nil {
			continue
		}
		foundConfig = true

		updated, changed := PatchBootConfig(isoPath, data)
		if !changed {
			continue
		}

		localPath := filepath.Join(bootDir, common.SafeName(strings.Trim(isoPath, "/")))
		if err := os.WriteFile(localPath, updated, 0o600); err != nil {
			return nil, fmt.Errorf("write patched boot config %s: %w", isoPath, err)
		}
		mappings = append(mappings, isoutil.FileMapping{LocalPath: localPath, ISOPath: isoPath})
	}

	if !foundConfig {
		return nil, errors.New("no supported Ubuntu ISO boot config found to patch for autoinstall")
	}
	return mappings, nil
}

func AddAutoinstallKernelArgs(data []byte) ([]byte, bool) {
	return AddGRUBAutoinstallKernelArgs(data)
}

func PatchBootConfig(isoPath string, data []byte) ([]byte, bool) {
	if strings.Contains(isoPath, "/grub/") {
		return AddGRUBAutoinstallKernelArgs(data)
	}
	return AddSyslinuxAutoinstallKernelArgs(data)
}

func AddGRUBAutoinstallKernelArgs(data []byte) ([]byte, bool) {
	text := string(data)
	trailingNewline := strings.HasSuffix(text, "\n")
	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	changed := false
	sawTimeoutStyle := false
	sawTimeout := false

	for idx, line := range lines {
		trimmed := strings.TrimSpace(line)
		indent := line[:len(line)-len(strings.TrimLeft(line, " \t"))]

		switch {
		case strings.HasPrefix(trimmed, "set timeout_style="):
			sawTimeoutStyle = true
			updated := indent + GrubTimeoutStyleSetting
			if updated != line {
				lines[idx] = updated
				changed = true
			}
			continue
		case strings.HasPrefix(trimmed, "set timeout="):
			sawTimeout = true
			updated := indent + GrubTimeoutSetting
			if updated != line {
				lines[idx] = updated
				changed = true
			}
			continue
		case isInstallerBootLine(line):
			updated := ensureAutoinstallArgs(line, GrubNoCloudKernelArg)
			if updated != line {
				lines[idx] = updated
				changed = true
			}
		}
	}

	var prefix []string
	if !sawTimeoutStyle {
		prefix = append(prefix, GrubTimeoutStyleSetting)
	}
	if !sawTimeout {
		prefix = append(prefix, GrubTimeoutSetting)
	}
	if len(prefix) > 0 {
		lines = append(prefix, lines...)
		changed = true
	}

	if !changed {
		return data, false
	}
	out := strings.Join(lines, "\n")
	if trailingNewline {
		out += "\n"
	}
	return []byte(out), true
}

func AddSyslinuxAutoinstallKernelArgs(data []byte) ([]byte, bool) {
	text := string(data)
	trailingNewline := strings.HasSuffix(text, "\n")
	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	changed := false
	sawPrompt := false
	sawTimeout := false

	for idx, line := range lines {
		trimmed := strings.TrimSpace(line)
		indent := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
		switch {
		case strings.HasPrefix(strings.ToLower(trimmed), "prompt "):
			sawPrompt = true
			updated := indent + SyslinuxPromptSetting
			if updated != line {
				lines[idx] = updated
				changed = true
			}
		case strings.HasPrefix(strings.ToLower(trimmed), "timeout "):
			sawTimeout = true
			updated := indent + SyslinuxTimeoutSetting
			if updated != line {
				lines[idx] = updated
				changed = true
			}
		case isInstallerBootLine(line):
			updated := ensureAutoinstallArgs(line, NoCloudKernelArg)
			if updated != line {
				lines[idx] = updated
				changed = true
			}
		}
	}

	var prefix []string
	if !sawPrompt {
		prefix = append(prefix, SyslinuxPromptSetting)
	}
	if !sawTimeout {
		prefix = append(prefix, SyslinuxTimeoutSetting)
	}
	if len(prefix) > 0 {
		lines = append(prefix, lines...)
		changed = true
	}

	if !changed {
		return data, false
	}
	out := strings.Join(lines, "\n")
	if trailingNewline {
		out += "\n"
	}
	return []byte(out), true
}

func isInstallerBootLine(line string) bool {
	trimmed := strings.TrimSpace(line)
	lower := strings.ToLower(trimmed)
	return strings.HasPrefix(lower, "linux ") ||
		strings.HasPrefix(lower, "linuxefi ") ||
		strings.HasPrefix(lower, "append ") ||
		strings.Contains(lower, "/casper/vmlinuz")
}

func ensureAutoinstallArgs(line, nocloudArg string) string {
	if strings.Contains(line, "autoinstall") && strings.Contains(line, nocloudArg) && strings.Contains(line, ConsoleTTYS0KernelArg) {
		return line
	}

	insertArgs := []string{"autoinstall", nocloudArg, ConsoleTTY0KernelArg, ConsoleTTYS0KernelArg}
	for _, arg := range insertArgs {
		if strings.Contains(line, arg) {
			continue
		}
		line = insertBeforeTripleDash(line, arg)
	}
	return line
}

func insertBeforeTripleDash(line, arg string) string {
	if idx := strings.Index(line, " ---"); idx >= 0 {
		return line[:idx] + " " + arg + line[idx:]
	}
	return line + " " + arg
}
