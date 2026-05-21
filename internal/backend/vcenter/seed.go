package vcenter

import (
	"context"

	"ubuntu-vm-template-builder/internal/seediso"
)

const (
	NoCloudKernelArg        = seediso.NoCloudKernelArg
	GrubNoCloudKernelArg    = seediso.GrubNoCloudKernelArg
	ConsoleTTY0KernelArg    = seediso.ConsoleTTY0KernelArg
	ConsoleTTYS0KernelArg   = seediso.ConsoleTTYS0KernelArg
	grubTimeoutStyleSetting = seediso.GrubTimeoutStyleSetting
	grubTimeoutSetting      = seediso.GrubTimeoutSetting
	syslinuxPromptSetting   = seediso.SyslinuxPromptSetting
	syslinuxTimeoutSetting  = seediso.SyslinuxTimeoutSetting
)

var installedGuestGRUBCleanupScript = seediso.InstalledGuestGRUBCleanupScript

type SeedOptions = seediso.Options

func RemasterUbuntuISOWithNoCloud(ctx context.Context, sourceISO, outputISO string, userData []byte, displayName, workDir string, offlineRepoPath string, options SeedOptions) error {
	return seediso.RemasterUbuntuISOWithNoCloud(ctx, sourceISO, outputISO, userData, displayName, workDir, offlineRepoPath, options)
}

func CreateNoCloudSeedDir(workDir string, userData []byte, displayName string, options SeedOptions) (string, error) {
	return seediso.CreateNoCloudSeedDir(workDir, userData, displayName, options)
}

func TransformUserData(userData []byte) ([]byte, error) {
	return seediso.TransformUserData(userData)
}

func TransformUserDataWithOptions(userData []byte, options SeedOptions) ([]byte, error) {
	return seediso.TransformUserDataWithOptions(userData, options)
}

func installedGuestGRUBCleanupLateCommands() []string {
	return seediso.InstalledGuestGRUBCleanupLateCommands()
}

func AddAutoinstallKernelArgs(data []byte) ([]byte, bool) {
	return seediso.AddAutoinstallKernelArgs(data)
}

func PatchBootConfig(isoPath string, data []byte) ([]byte, bool) {
	return seediso.PatchBootConfig(isoPath, data)
}

func AddGRUBAutoinstallKernelArgs(data []byte) ([]byte, bool) {
	return seediso.AddGRUBAutoinstallKernelArgs(data)
}

func AddSyslinuxAutoinstallKernelArgs(data []byte) ([]byte, bool) {
	return seediso.AddSyslinuxAutoinstallKernelArgs(data)
}
