package isoutil

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

type FileMapping struct {
	LocalPath string
	ISOPath   string
}

func RemasterISO(ctx context.Context, sourceISO, outputISO string, mappings []FileMapping) error {
	args := []string{
		"-indev", sourceISO,
		"-outdev", outputISO,
	}
	for _, mapping := range mappings {
		args = append(args, "-map", mapping.LocalPath, mapping.ISOPath)
	}
	args = append(args, "-boot_image", "any", "replay")

	cmd := exec.CommandContext(ctx, "xorriso", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("xorriso remaster failed: %v (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}
