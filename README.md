# Ubuntu VM Template Builder

Builds reproducible Ubuntu VM template disk images in QEMU, or vCenter VMs and
templates directly on ESXi hardware, using a cloud-init autoinstall file that you
provide directly.

The `qemu` command creates a NoCloud seed ISO containing your `user-data`,
creates the destination disk image, extracts the Ubuntu ISO kernel and initrd,
and boots QEMU with `autoinstall ---`. The `vcenter` command
remasters the Ubuntu ISO with NoCloud seed data, boots a temporary VM on the
selected ESXi host, waits for the installer to power off, and leaves the guest
as a VM or converts it to a vCenter template.

It is intended to make Ubuntu VM template creation faster and repeatable: keep
the autoinstall cloud-config in version control, run the builder, and import the
resulting image into your virtualization platform as a template.

## Requirements

- Go 1.26 or newer
- `qemu-system-x86_64`
- `qemu-img` when creating `qcow2` or `vmdk` images
- OVMF UEFI firmware for the default `boot_firmware: uefi` hardware config
- Accessible `/dev/kvm`, because QEMU is launched with `--enable-kvm`
- `xorriso` when using the `vcenter` command
- An Ubuntu live-server ISO containing `/casper/vmlinuz` and `/casper/initrd`

## Usage

Check host prerequisites:

```bash
./ubuntu-vm-template-builder qemu prerequisites
./ubuntu-vm-template-builder vcenter prerequisites
```

Aliases under each backend are `prereqs`, `prerequests`, and `prequests`. Each
command prints only the host requirements for that backend and suggests
OS-specific install or permission fixes for missing requirements.

Run a QEMU install:

```bash
go run . \
  qemu \
  build \
  --iso /path/to/ubuntu-24.04.3-live-server-amd64.iso \
  --image /path/to/output.img \
  --user-data autoinstall.uefi.example.yaml \
  --hardware-config hardware.qemu.yaml
```

Build a binary:

```bash
go build -o ubuntu-vm-template-builder .
```

Then run:

```bash
./ubuntu-vm-template-builder qemu \
  build \
  --iso /path/to/ubuntu-24.04.3-live-server-amd64.iso \
  --image /path/to/output.img \
  --disk-format raw \
  --user-data autoinstall.uefi.example.yaml \
  --hardware-config hardware.qemu.yaml
```

Build a vCenter VM or template:

```bash
./ubuntu-vm-template-builder vcenter \
  build \
  --iso /path/to/ubuntu-24.04.3-live-server-amd64.iso \
  --user-data autoinstall.uefi.example.yaml \
  --hardware-config hardware.vcenter.yaml \
  --vcenter-host vc.example.com \
  --vcenter-username administrator@vsphere.local \
  --vcenter-password 'secret' \
  --vcenter-insecure \
  --vcenter-datacenter DC0 \
  --vcenter-esxi-host esxi-01.example.com \
  --vcenter-datastore datastore1 \
  --vcenter-folder /DC0/vm/Templates \
  --vcenter-network 'VM Network' \
  --template-name ubuntu-24.04-template
```

For `vcenter`, `--image` is not used. The output is a remote vCenter VM or
template, named by `--template-name`, or by the hostname in `--user-data` when
`--template-name` is omitted. During installation the vCenter backend streams
the guest console through a temporary datastore-backed serial log file; the log
file is deleted after the VM/template is created successfully and left in place
on failure for debugging.

Upload an arbitrary file to a vCenter datastore:

```bash
./ubuntu-vm-template-builder vcenter \
  upload \
  --source /path/to/local-file.iso \
  --destination uploads/local-file.iso \
  --vcenter-host vc.example.com \
  --vcenter-username administrator@vsphere.local \
  --vcenter-password 'secret' \
  --vcenter-insecure \
  --vcenter-datacenter DC0 \
  --vcenter-esxi-host esxi-01.example.com \
  --vcenter-datastore datastore1
```

`vcenter upload` creates parent datastore directories automatically and fails if
the destination already exists unless `--overwrite` is passed.

Available backend commands are currently `qemu` and `vcenter`. Each backend
provides `build`, `prerequisites`, and `hardware-config-example`; `vcenter` also
provides `upload`. A `proxmox` command is intentionally not implemented yet.

## Hardware Config

Hardware settings are configured with `--hardware-config hardware.yaml`.
`disk_size` is required. Other common/backend keys default when omitted from the
file.

```yaml
boot_firmware: uefi
disk_size: 20G
vcpu: 2
memory_mb: 2048
qemu:
  cpu_model: host
  disk_interface: virtio
  iso_interface: virtio
vcenter:
  scsi_controller: pvscsi
  network_adapter: vmxnet3
  network: VM Network
  disk_provisioning: thick_provision_lazy_zeroed
  reserve_all_guest_memory: false
  output_type: template
```

Print a complete backend-specific example:

```bash
./ubuntu-vm-template-builder qemu hardware-config-example > hardware.qemu.yaml
./ubuntu-vm-template-builder vcenter hardware-config-example > hardware.vcenter.yaml
```

Common keys are `boot_firmware`, `disk_size`, `vcpu`, and `memory_mb`.
Backend-specific keys live under `qemu` and `vcenter`. Supported boot firmware
values are `uefi` and `bios`; UEFI is the default. Use `boot_firmware: bios` if
the host does not have OVMF installed or you need a BIOS-installed image.

For vCenter, the target network can be set as `vcenter.network` in the hardware
config or with `--vcenter-network`; the CLI flag overrides the config value.
Set `vcenter.output_type: vm` to leave the installed guest as a powered-off VM
instead of converting it to a template. The default is `template`.

Example:

```yaml
# hardware.bios.yaml
boot_firmware: bios
disk_size: 20G
```

```bash
./ubuntu-vm-template-builder qemu \
  build \
  --iso /path/to/ubuntu.iso \
  --image /path/to/output.img \
  --disk-format raw \
  --user-data autoinstall.bios.example.yaml \
  --hardware-config hardware.bios.yaml
```

Build and run with Docker:

```bash
docker build -t ubuntu-vm-template-builder .
```

The image includes the compiled builder, QEMU, `qemu-img`, OVMF firmware, and
`xorriso`.
It still needs access to the host KVM device, and input/output paths must be
mounted into the container. This example mounts the current directory at
`/work` and writes the output image as your host user:

```bash
docker run --rm \
  --device /dev/kvm \
  --user "$(id -u):$(id -g)" \
  --group-add "$(stat -c %g /dev/kvm)" \
  -v "$PWD:/work" \
  ubuntu-vm-template-builder \
  qemu \
  build \
  --iso /work/ubuntu-24.04.3-live-server-amd64.iso \
  --image /work/output.img \
  --disk-format raw \
  --user-data /work/autoinstall.uefi.example.yaml \
  --hardware-config /work/hardware.qemu.yaml
```

You can also check the container runtime prerequisites:

```bash
docker run --rm --device /dev/kvm ubuntu-vm-template-builder qemu prerequisites
```

Supported QEMU disk formats are `raw`, `qcow2`, and `vmdk`.
UEFI installs use a temporary OVMF variables file during QEMU installation; the
disk image is the only persistent output file.

## Releases

Releases are published with the manual `Release` GitHub Actions workflow. Create
and push a SemVer tag first:

```bash
git tag v0.1.0
git push origin v0.1.0
```

Then run the `Release` workflow from the GitHub Actions UI and enter that tag.
The workflow builds the Linux amd64 binary, uploads it and its SHA-256 checksum
to the GitHub Release, and pushes the Docker image to GHCR:

```text
ghcr.io/torob/ubuntu-vm-template-builder:v0.1.0
ghcr.io/torob/ubuntu-vm-template-builder:latest
```

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

For `boot_firmware: uefi`, the QEMU command also validates that the user-data can
create a portable single-file image. If you provide a custom `storage.config`,
it must define a GPT EFI System Partition formatted as FAT and mounted at
`/boot/efi`. The user-data must also include a fallback UEFI bootloader
late-command, such as `grub-install --removable`, so the disk does not depend on
QEMU's temporary UEFI NVRAM state.

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
`./ubuntu-vm-template-builder`, creates one isolated run directory under
`.e2e-runs`, and executes every `ubuntu_versions x disk_formats x boot_modes`
case. Each case generates its own autoinstall YAML and SSH key, installs the
image, boots it with QEMU, then verifies SSH login, hostname, sudo password,
authorized key, and the expected BIOS or UEFI boot layout.

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
