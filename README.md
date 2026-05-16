# Ubuntu Server Autoinstaller

Automates Ubuntu Server image installation in QEMU using a cloud-init
autoinstall file that you provide directly.

The tool creates a NoCloud seed ISO containing your `user-data`, creates the
destination disk image, extracts the Ubuntu ISO kernel and initrd, and boots QEMU
with `autoinstall ---`.

## Requirements

- Go 1.26 or newer
- `qemu-system-x86_64`
- `qemu-img` when creating `qcow2` or `vmdk` images
- OVMF UEFI firmware for the default `--boot-mode uefi`
- Accessible `/dev/kvm`, because QEMU is launched with `--enable-kvm`
- An Ubuntu live-server ISO containing `/casper/vmlinuz` and `/casper/initrd`

## Usage

Check host prerequisites:

```bash
./install-ubuntu prerequisites
```

Aliases are `prereqs`, `prerequests`, and `prequests`. The command prints the
host requirements it checks and suggests OS-specific install or permission fixes
for missing requirements.

Run an install:

```bash
go run . \
  --iso /path/to/ubuntu-24.04.3-live-server-amd64.iso \
  --image /path/to/output.img \
  --disk-size 20G \
  --user-data autoinstall.example.yaml
```

Build a binary:

```bash
go build -o install-ubuntu .
```

Then run:

```bash
./install-ubuntu \
  --iso /path/to/ubuntu-24.04.3-live-server-amd64.iso \
  --image /path/to/output.img \
  --disk-size 20G \
  --boot-mode uefi \
  --disk-format raw \
  --user-data autoinstall.example.yaml
```

Supported disk formats are `raw`, `qcow2`, and `vmdk`.
Supported boot modes are `uefi` and `bios`; `uefi` is the default. Use
`--boot-mode bios` if the host does not have OVMF installed or you need a BIOS
installed image. UEFI installs use a temporary OVMF variables file during QEMU
installation; the disk image is the only persistent output file.

## User Data

The `--user-data` file is passed through unchanged into the seed ISO. Put all
guest configuration there, including hostname, user identity, password hash, SSH
keys, networking, storage, locale, and timezone.

The program validates that the file is readable, non-empty, valid YAML, and has a
top-level `autoinstall` mapping before creating any disk image.

For `--boot-mode uefi`, the program also validates that the user-data can create
a portable single-file image. If you provide a custom `storage.config`, it must
define a GPT EFI System Partition formatted as FAT and mounted at `/boot/efi`.
The user-data must also include a fallback UEFI bootloader late-command, such as
`grub-install --removable`, so the disk does not depend on QEMU's temporary UEFI
NVRAM state.

See `autoinstall.example.yaml` for a minimal example.
