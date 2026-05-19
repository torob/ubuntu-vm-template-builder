package vcenter

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"ubuntu-vm-template-builder/internal/common"
)

const (
	NoCloudKernelArg        = "ds=nocloud;s=/cdrom/nocloud/"
	GrubNoCloudKernelArg    = "ds=nocloud\\;s=/cdrom/nocloud/"
	ConsoleTTY0KernelArg    = "console=tty0"
	ConsoleTTYS0KernelArg   = "console=ttyS0,115200n8"
	grubTimeoutStyleSetting = "set timeout_style=hidden"
	grubTimeoutSetting      = "set timeout=0"
	syslinuxPromptSetting   = "prompt 0"
	syslinuxTimeoutSetting  = "timeout 1"
)

var installedGuestGRUBCleanupScript = strings.Join([]string{
	`set -eu`,
	`file=${GRUB_DEFAULT_FILE:-/etc/default/grub}`,
	`[ -f "$file" ] || exit 0`,
	`line=$(grep -m1 "^GRUB_CMDLINE_LINUX_DEFAULT=" "$file" || true)`,
	`[ -n "$line" ] || exit 0`,
	`value=${line#GRUB_CMDLINE_LINUX_DEFAULT=}`,
	`value=${value#\"}`,
	`value=${value%\"}`,
	`clean=""`,
	`for arg in $value; do case "$arg" in console=tty0|console=ttyS0,115200n8|autoinstall|ds=nocloud\;s=/cdrom/nocloud/|ds=nocloud\\\;s=/cdrom/nocloud/) continue ;; esac; if [ -n "$clean" ]; then clean="$clean $arg"; else clean="$arg"; fi; done`,
	`escaped=$(printf "%s\n" "$clean" | sed "s/[\/&]/\\\\&/g")`,
	`sed -i "s|^GRUB_CMDLINE_LINUX_DEFAULT=.*|GRUB_CMDLINE_LINUX_DEFAULT=\"$escaped\"|" "$file"`,
}, "; ")

type isoFileMapping struct {
	LocalPath string
	ISOPath   string
}

func RemasterUbuntuISOWithNoCloud(ctx context.Context, sourceISO, outputISO string, userData []byte, displayName, workDir string) error {
	seedDir, err := CreateNoCloudSeedDir(workDir, userData, displayName)
	if err != nil {
		return err
	}

	bootMappings, err := prepareBootConfigMappings(sourceISO, workDir)
	if err != nil {
		return err
	}

	args := []string{
		"-indev", sourceISO,
		"-outdev", outputISO,
		"-map", seedDir, "/nocloud",
	}
	for _, mapping := range bootMappings {
		args = append(args, "-map", mapping.LocalPath, mapping.ISOPath)
	}
	args = append(args, "-boot_image", "any", "replay")

	cmd := exec.CommandContext(ctx, "xorriso", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("xorriso remaster failed: %v (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func CreateNoCloudSeedDir(workDir string, userData []byte, displayName string) (string, error) {
	seedDir := filepath.Join(workDir, "nocloud")
	if err := os.MkdirAll(seedDir, 0o700); err != nil {
		return "", fmt.Errorf("create NoCloud seed directory: %w", err)
	}

	transformedUserData, err := TransformUserData(userData)
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
	var root yaml.Node
	if err := yaml.Unmarshal(userData, &root); err != nil {
		return nil, fmt.Errorf("parse YAML: %w", err)
	}

	autoinstall, err := common.AutoinstallMappingFromRoot(&root)
	if err != nil {
		return nil, err
	}
	common.SetMappingScalar(autoinstall, "shutdown", "poweroff")
	if err := appendInstalledGuestGRUBCleanupLateCommands(autoinstall); err != nil {
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

func appendInstalledGuestGRUBCleanupLateCommands(autoinstall *yaml.Node) error {
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

	for _, command := range installedGuestGRUBCleanupLateCommands() {
		lateCommands.Content = append(lateCommands.Content, &yaml.Node{
			Kind:  yaml.ScalarNode,
			Tag:   "!!str",
			Value: command,
		})
	}
	return nil
}

func installedGuestGRUBCleanupLateCommands() []string {
	return []string{
		"curtin in-target --target=/target -- sh -c '" + installedGuestGRUBCleanupScript + "'",
		"curtin in-target --target=/target -- update-grub",
	}
}

func prepareBootConfigMappings(sourceISO, workDir string) ([]isoFileMapping, error) {
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

	var mappings []isoFileMapping
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
		mappings = append(mappings, isoFileMapping{LocalPath: localPath, ISOPath: isoPath})
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
			updated := indent + grubTimeoutStyleSetting
			if updated != line {
				lines[idx] = updated
				changed = true
			}
			continue
		case strings.HasPrefix(trimmed, "set timeout="):
			sawTimeout = true
			updated := indent + grubTimeoutSetting
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
		prefix = append(prefix, grubTimeoutStyleSetting)
	}
	if !sawTimeout {
		prefix = append(prefix, grubTimeoutSetting)
	}
	if len(prefix) > 0 {
		lines = append(prefix, lines...)
		changed = true
	}

	if !changed {
		return data, false
	}

	updated := strings.Join(lines, "\n")
	if trailingNewline {
		updated += "\n"
	}
	return []byte(updated), true
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
		case strings.HasPrefix(trimmed, "prompt "):
			sawPrompt = true
			updated := indent + syslinuxPromptSetting
			if updated != line {
				lines[idx] = updated
				changed = true
			}
			continue
		case strings.HasPrefix(trimmed, "timeout "):
			sawTimeout = true
			updated := indent + syslinuxTimeoutSetting
			if updated != line {
				lines[idx] = updated
				changed = true
			}
			continue
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
		prefix = append(prefix, syslinuxPromptSetting)
	}
	if !sawTimeout {
		prefix = append(prefix, syslinuxTimeoutSetting)
	}
	if len(prefix) > 0 {
		lines = append(prefix, lines...)
		changed = true
	}

	if !changed {
		return data, false
	}

	updated := strings.Join(lines, "\n")
	if trailingNewline {
		updated += "\n"
	}
	return []byte(updated), true
}

func isInstallerBootLine(line string) bool {
	trimmed := strings.TrimSpace(line)
	if !(strings.HasPrefix(trimmed, "linux") || strings.HasPrefix(trimmed, "linuxefi") || strings.HasPrefix(trimmed, "append ")) {
		return false
	}
	return strings.Contains(trimmed, "/casper/vmlinuz") ||
		strings.Contains(trimmed, "casper/vmlinuz") ||
		strings.Contains(trimmed, "initrd=/casper/initrd")
}

func ensureAutoinstallArgs(line string, noCloudArg string) string {
	line = normalizeNoCloudArg(line, noCloudArg)

	var args []string
	if !strings.Contains(line, "autoinstall") {
		args = append(args, "autoinstall")
	}
	if !strings.Contains(line, "ds=nocloud") {
		args = append(args, noCloudArg)
	}
	if !strings.Contains(line, "console=tty0") {
		args = append(args, ConsoleTTY0KernelArg)
	}
	if !strings.Contains(line, "console=ttyS0") {
		args = append(args, ConsoleTTYS0KernelArg)
	}
	if len(args) == 0 {
		return line
	}

	insertion := strings.Join(args, " ")
	if before, after, ok := strings.Cut(line, "---"); ok {
		return strings.TrimRight(before, " \t") + " " + insertion + " ---" + after
	}
	return strings.TrimRight(line, " \t") + " " + insertion
}

func normalizeNoCloudArg(line string, noCloudArg string) string {
	if noCloudArg == GrubNoCloudKernelArg {
		return strings.ReplaceAll(line, NoCloudKernelArg, GrubNoCloudKernelArg)
	}
	return strings.ReplaceAll(line, GrubNoCloudKernelArg, NoCloudKernelArg)
}
