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
  --user-data autoinstall.uefi.example.yaml
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
  --user-data autoinstall.uefi.example.yaml
```

For a BIOS-installed image, use the BIOS example and select BIOS explicitly:

```bash
./install-ubuntu \
  --iso /path/to/ubuntu-24.04.3-live-server-amd64.iso \
  --image /path/to/output.img \
  --disk-size 20G \
  --boot-mode bios \
  --disk-format raw \
  --user-data autoinstall.bios.example.yaml
```

Supported disk formats are `raw`, `qcow2`, and `vmdk`.
Supported boot modes are `uefi` and `bios`; `uefi` is the default. Use
`--boot-mode bios` if the host does not have OVMF installed or you need a BIOS
installed image. UEFI installs use a temporary OVMF variables file during QEMU
installation; the disk image is the only persistent output file.

## BIOS and UEFI Booting

BIOS and UEFI find the operating system in different ways.

With BIOS boot, the firmware loads boot code from the disk itself. On a GPT disk,
GRUB uses a small `bios_grub` partition to store the BIOS bootloader data it
needs. That makes BIOS images easy to move as a single disk file because the boot
path is contained on the disk.

With UEFI boot, the firmware loads an `.efi` program from an EFI System
Partition, usually mounted at `/boot/efi` inside Ubuntu. A normal Ubuntu install
places bootloader files under a vendor path such as `EFI/ubuntu/` and creates a
firmware NVRAM boot entry that points to that path. That NVRAM entry is not part
of the disk image. In QEMU it lives in the OVMF variables file; in Proxmox VE it
lives in the VM EFI disk; in ESXi it lives in the VM `.nvram` file.

For portable UEFI images, the UEFI example also installs the fallback bootloader
path:

```text
EFI/BOOT/BOOTX64.EFI
```

Fresh UEFI firmware knows to try that fallback path even when it has no saved
Ubuntu NVRAM boot entry. The UEFI example therefore includes this late command:

```yaml
late-commands:
  - curtin in-target --target=/target -- grub-install --target=x86_64-efi --efi-directory=/boot/efi --bootloader-id=ubuntu --removable --recheck
```

`curtin in-target` runs `grub-install` inside the newly installed system under
`/target`. The important option is `--removable`: it writes the fallback
`EFI/BOOT/BOOTX64.EFI` path so the disk can boot in a new QEMU, LXD, Proxmox VE,
or ESXi VM without carrying QEMU's temporary OVMF variables file. This does not
replace Ubuntu's normal `EFI/ubuntu/` files; it adds a portable fallback.

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

See `autoinstall.uefi.example.yaml` for the UEFI example and
`autoinstall.bios.example.yaml` for the BIOS example.

## End-to-End Matrix Tests

The repository includes a Go-based matrix runner for testing generated images
across Ubuntu versions, disk formats, and boot modes:

```bash
go run ./cmd/e2e-matrix --config e2e.config.yaml --dry-run
```

To run the matrix:

```bash
go run ./cmd/e2e-matrix --config e2e.config.yaml
```

The config file provides the ISO download URLs directly:

```yaml
ubuntu_versions:
  - name: "22.04"
    iso_url: "https://releases.ubuntu.com/22.04/ubuntu-22.04.5-live-server-amd64.iso"
    sha256: "9bc6028870aef3f74f4e16b900008179e78b130e6b0b9a140635434a46aa98b0"
  - name: "24.04"
    iso_url: "https://releases.ubuntu.com/24.04/ubuntu-24.04.4-live-server-amd64.iso"
    sha256: "e907d92eeec9df64163a7e454cbc8d7755e8ddc7ed42f99dbc80c40f1a138433"
  - name: "26.04"
    iso_url: "https://releases.ubuntu.com/26.04/ubuntu-26.04-live-server-amd64.iso"
    sha256: "dec49008a71f6098d0bcfc822021f4d042d5f2db279e4d75bdd981304f1ca5d9"
```

The runner downloads and caches ISOs under `.e2e-cache/isos`, builds
`./install-ubuntu`, creates one isolated run directory under `.e2e-runs`, and
executes every `ubuntu_versions x disk_formats x boot_modes` case. Each case
generates its own autoinstall YAML and SSH key, installs the image, boots it with
QEMU, then verifies SSH login, hostname, sudo password, authorized key, and the
expected BIOS or UEFI boot layout.

Before running, it checks the host tools it needs: QEMU, KVM access, `ssh`,
`ssh-keygen`, `qemu-img` when needed, and OVMF when UEFI cases are selected.

Useful flags:

```bash
go run ./cmd/e2e-matrix --config e2e.config.yaml --concurrency 2
go run ./cmd/e2e-matrix --config e2e.config.yaml --keep all
go run ./cmd/e2e-matrix --config e2e.config.yaml --keep none
```

`keep` can be `all`, `failures`, or `none`. The default is `failures`, which
keeps failed case directories with `install.log`, `boot.log`, generated
user-data, SSH key, and disk image for inspection.
