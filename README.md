# Ubuntu VM Template Builder

Builds reproducible Ubuntu VM template disk images in QEMU, vCenter VMs and
templates directly on ESXi hardware, or Proxmox VE VMs and templates on a
selected node, using a cloud-init autoinstall file that you provide directly.

The `qemu` command remasters the Ubuntu ISO with builder support scripts,
creates a NoCloud seed ISO containing your `user-data`, creates the destination
disk image, extracts the Ubuntu ISO kernel and initrd, and boots QEMU with
`autoinstall ---`. The `vcenter` and `proxmox` commands
remaster the Ubuntu ISO with NoCloud seed data, boot a temporary VM on the
selected virtualization host, wait for the installer to power off, and leave
the guest as a VM or convert it to a platform template.

It is intended to make Ubuntu VM template creation faster and repeatable: keep
the autoinstall cloud-config in version control, run the builder, and import the
resulting image into your virtualization platform as a template.

## Requirements

- Go 1.26 or newer
- `qemu-system-x86_64`
- `qemu-img` when creating `qcow2` or `vmdk` images
- OVMF UEFI firmware for the default `boot_firmware: uefi` hardware config
- Accessible `/dev/kvm`, because QEMU is launched with `--enable-kvm`
- `xorriso` when using any build backend
- `apt-get` and the Ubuntu archive keyring when using `--install-extra-packages`
- An Ubuntu live-server ISO containing `/casper/vmlinuz` and `/casper/initrd`

## Usage

Check host prerequisites:

```bash
./ubuntu-vm-template-builder qemu prerequisites
./ubuntu-vm-template-builder vcenter prerequisites
./ubuntu-vm-template-builder proxmox prerequisites
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
  --hardware-config hardware.qemu.yaml \
  --install-extra-packages extra-packages.yaml
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
  --template-name ubuntu-24.04-template \
  --install-extra-packages extra-packages.yaml
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

Build a Proxmox VE VM or template:

```bash
./ubuntu-vm-template-builder proxmox \
  build \
  --iso /path/to/ubuntu-24.04.3-live-server-amd64.iso \
  --user-data autoinstall.uefi.example.yaml \
  --hardware-config hardware.proxmox.yaml \
  --proxmox-host pve.example.com:8006 \
  --proxmox-token-id 'root@pam!builder' \
  --proxmox-token-secret 'secret' \
  --proxmox-insecure \
  --proxmox-node pve1 \
  --proxmox-iso-storage local \
  --proxmox-disk-storage vms \
  --proxmox-bridge vmbr0 \
  --template-name ubuntu-24.04-template \
  --options options.proxmox.yaml \
  --cloud-init-options cloud-init.proxmox.yaml \
  --install-extra-packages extra-packages.yaml
```

For `proxmox`, `--image` is not used. The output is a remote Proxmox VE VM or
template, named by `--template-name`, or by the hostname in `--user-data` when
`--template-name` is omitted. API-token authentication is the only supported
authentication mode. `--proxmox-iso-storage` must allow `iso` content and is
used for the temporary remastered installer ISO. `--proxmox-disk-storage` must
allow `images` content and is used for the VM disk, EFI vars, and optional
Cloud-Init drive. Before creating or uploading remote resources, the build
checks the selected storage content types and available capacity for the
remastered ISO, disk, EFI vars, and optional Cloud-Init drive. During
installation it streams the guest serial console through Proxmox's websocket
console API on a best-effort basis.

Available backend commands are currently `qemu`, `vcenter`, and `proxmox`. Each
backend provides `build`, `prerequisites`, and `hardware-config-example`;
`vcenter` also provides `upload`.

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
  compatibility: ""
  guest_os_id: ubuntu64Guest
  reserve_all_guest_memory: false
  output_type: template
proxmox:
  bridge: vmbr0
  network_adapter: virtio
  scsi_controller: virtio-scsi-pci
  disk_interface: scsi
  disk_format: raw
  cpu_type: host
  machine: q35
  ostype: l26
  efi_type: 4m
  pre_enrolled_keys: false
  output_type: template
```

Print a complete backend-specific example:

```bash
./ubuntu-vm-template-builder qemu hardware-config-example > hardware.qemu.yaml
./ubuntu-vm-template-builder vcenter hardware-config-example > hardware.vcenter.yaml
./ubuntu-vm-template-builder proxmox hardware-config-example > hardware.proxmox.yaml
```

Common hardware config options:

| Option | Required | Default | Description |
| --- | --- | --- | --- |
| `boot_firmware` | No | `uefi` | Guest firmware. Supported values are `uefi` and `bios`. |
| `disk_size` | Yes | none | Guest disk size, such as `20G`, `51200M`, or `1T`. |
| `vcpu` | No | `2` | Number of virtual CPUs. |
| `memory_mb` | No | `2048` | Guest memory in MiB. |

QEMU hardware config options:

| Option | Required | Default | Description |
| --- | --- | --- | --- |
| `qemu.cpu_model` | No | `host` | CPU model passed to QEMU. |
| `qemu.disk_interface` | No | `virtio` | Disk interface. Supported values are `virtio`, `ide`, `scsi`, and `sata`. |
| `qemu.iso_interface` | No | `virtio` | Installer ISO interface. Supported values are `virtio`, `ide`, `scsi`, and `sata`. |

vCenter hardware config options:

| Option | Required | Default | Description |
| --- | --- | --- | --- |
| `vcenter.scsi_controller` | No | `pvscsi` | Disk controller type. Supported values are `pvscsi`, `lsilogic`, `buslogic`, and `lsilogic-sas`. |
| `vcenter.network_adapter` | No | `vmxnet3` | NIC type. Supported values are `vmxnet3`, `vmxnet2`, `vmxnet`, `e1000`, and `e1000e`. |
| `vcenter.network` | Yes, unless `--vcenter-network` is set | none | vCenter network name or inventory path. The CLI flag overrides this value. |
| `vcenter.disk_provisioning` | No | `thick_provision_lazy_zeroed` | Disk provisioning. Supported values are `thin`, `thick_provision_lazy_zeroed`, and `thick_provision_eager_zeroed`. |
| `vcenter.compatibility` | No | vCenter default | VM hardware compatibility/version, such as `vmx-21`. Leave empty to let vCenter choose. |
| `vcenter.guest_os_id` | No | `ubuntu64Guest` | vSphere Guest OS identifier, such as `ubuntu64Guest`. Use the API identifier, not the human-readable UI label. |
| `vcenter.reserve_all_guest_memory` | No | `false` | Reserves all configured guest memory in vCenter. |
| `vcenter.output_type` | No | `template` | Final output type. Supported values are `template` and `vm`. |

`vcenter.compatibility` accepts the vSphere VM hardware version string, not the
human-readable UI label. Leave it empty to use the vCenter/ESXi default. The
builder validates the format as `vmx-N`; vCenter validates whether that version
is supported by the selected ESXi host.

Common compatibility values:

| Value | Notes |
| --- | --- |
| `""` | Do not set VM hardware version; let vCenter choose. |
| `vmx-19` | Example hardware version supported by recent vSphere 7.x environments. |
| `vmx-20` | Example hardware version supported by vSphere 8.x environments. |
| `vmx-21` | Example hardware version supported by newer vSphere 8.x environments. |

Use the exact value shown by your vCenter compatibility option or API. If an
older ESXi host rejects the value, leave it empty or choose a lower `vmx-N`
version supported by that host.

`vcenter.guest_os_id` accepts the vSphere Guest OS API identifier. It is not the
human-readable UI label. For Ubuntu templates, the default `ubuntu64Guest` is
usually the right value.

Common guest OS IDs:

| Value | Guest OS selection |
| --- | --- |
| `ubuntu64Guest` | Ubuntu Linux 64-bit. |
| `ubuntuGuest` | Ubuntu Linux 32-bit. |
| `debian12_64Guest` | Debian GNU/Linux 12 64-bit. |
| `debian11_64Guest` | Debian GNU/Linux 11 64-bit. |
| `rhel9_64Guest` | Red Hat Enterprise Linux 9 64-bit. |
| `rhel8_64Guest` | Red Hat Enterprise Linux 8 64-bit. |
| `centos9_64Guest` | CentOS 9 64-bit. |
| `sles15_64Guest` | SUSE Linux Enterprise Server 15 64-bit. |
| `opensuse64Guest` | openSUSE 64-bit. |
| `fedora64Guest` | Fedora Linux 64-bit. |
| `vmwarePhoton64Guest` | VMware Photon OS 64-bit. |
| `genericLinuxGuest` | Generic Linux. |
| `otherLinux64Guest` | Other Linux 64-bit. |

The full set of guest OS IDs depends on the vSphere API version exposed by your
vCenter/ESXi environment. Use an ID your vCenter supports; otherwise CreateVM
will fail validation.

Use `boot_firmware: bios` if the host does not have OVMF installed or you need a
BIOS-installed image.

For vCenter, the target network can be set as `vcenter.network` in the hardware
config or with `--vcenter-network`; the CLI flag overrides the config value.
Set `vcenter.output_type: vm` to leave the installed guest as a powered-off VM
instead of converting it to a template. The default is `template`.

Proxmox hardware config options:

| Option | Required | Default | Description |
| --- | --- | --- | --- |
| `proxmox.bridge` | Yes, unless `--proxmox-bridge` is set | none | Proxmox bridge name, such as `vmbr0`. The CLI flag overrides this value. |
| `proxmox.network_adapter` | No | `virtio` | NIC model. Supported values are `virtio`, `e1000`, `e1000e`, `rtl8139`, and `vmxnet3`. |
| `proxmox.scsi_controller` | No | `virtio-scsi-pci` | SCSI controller. Supported values are `virtio-scsi-pci`, `virtio-scsi-single`, `lsi`, `lsi53c810`, `megasas`, and `pvscsi`. |
| `proxmox.disk_interface` | No | `scsi` | Guest disk bus. Supported values are `scsi`, `sata`, `virtio`, and `ide`. |
| `proxmox.disk_format` | No | `raw` | Disk format requested from Proxmox. Supported values are `raw`, `qcow2`, and `vmdk`; storage capabilities still apply. |
| `proxmox.cpu_type` | No | `host` | CPU type passed to Proxmox, such as `host` or `x86-64-v3`. |
| `proxmox.machine` | No | `q35` | Proxmox machine type. |
| `proxmox.ostype` | No | `l26` | Proxmox guest OS type. |
| `proxmox.efi_type` | No | `4m` | EFI vars disk type when `boot_firmware: uefi`; supported values are `2m` and `4m`. |
| `proxmox.pre_enrolled_keys` | No | `false` | Whether the EFI vars disk is created with pre-enrolled secure boot keys. |
| `proxmox.output_type` | No | `template` | Final output type. Supported values are `template` and `vm`. |

For Proxmox, `--proxmox-vmid` is optional. When it is omitted, the builder asks
Proxmox for the next available VMID. Set `proxmox.output_type: vm` to leave the
installed guest as a powered-off VM instead of converting it to a template. The
default is `template`.

### Proxmox VM Options and Cloud-Init Options

`proxmox build` accepts two optional typed YAML files:

- `--options` applies VM Options-tab settings after installation finishes.
- `--cloud-init-options` creates a Cloud-Init drive on `--proxmox-disk-storage`
  and applies Proxmox Cloud-Init settings after installation finishes.

Both files are strict: unknown keys fail before the build touches Proxmox. Boot
order, installer ISO attachment, serial console, disk, EFI, network creation,
and template conversion stay controlled by the builder.

`options.proxmox.yaml` structure:

```yaml
start_at_boot: false
startup:
  order: 10
  up_delay_seconds: 30
  down_delay_seconds: 60
qemu_guest_agent:
  enabled: true
  freeze_fs_on_backup: false
  fstrim_cloned_disks: true
  type: virtio # virtio or isa
protection: false
tablet: true
acpi: true
kvm: true
freeze_cpu_at_startup: false
local_time: false
rtc_start_date: now
hotplug:
  network: true
  disk: true
  usb: true
  memory: false
  cpu: false
  cloudinit: true
smbios:
  values_are_base64: false
  uuid: ""
  manufacturer: ""
  product: ""
  version: ""
  serial: ""
  sku: ""
  family: ""
spice_enhancements:
  folder_sharing: false
  video_streaming: off # off, all, or filter
vm_state_storage: ""
tags:
  - ubuntu
  - template
description: |
  Ubuntu template built by ubuntu-vm-template-builder.
```

`cloud-init.proxmox.yaml` structure:

```yaml
type: nocloud # nocloud, configdrive2, or opennebula
upgrade: false
user: ubuntu
password: ""
ssh_keys:
  - ssh-ed25519 AAAA... user@example
dns:
  nameservers:
    - 1.1.1.1
    - 8.8.8.8
  search_domains:
    - example.com
network:
  - index: 0
    ipv4: dhcp
    gateway4: ""
    ipv6: auto
    gateway6: ""
custom:
  user: local:snippets/user-data.yaml
  network: local:snippets/network-data.yaml
  meta: local:snippets/meta-data.yaml
  vendor: local:snippets/vendor-data.yaml
```

`custom` references must already exist in Proxmox storage; the builder does not
upload snippet files. Write `ssh_keys` as normal OpenSSH public keys; the
builder applies Proxmox's required API encoding.

## Extra Offline Packages

`--install-extra-packages` is optional for `qemu build`, `vcenter build`, and
`proxmox build`. When set, the builder asks APT for the exact requested package
closure, downloads those `.deb` files on the host with Ubuntu signature and hash
verification enabled, embeds only those `.deb` files plus Ubuntu's signed
`dists/...` metadata in the remastered installer ISO, and adds late-commands
that run an injected builder script to install from
`/cdrom/ubuntu-vm-template-builder/offline-apt`. Temporary APT files copied
into the target guest are removed before the installer finishes.

Example config:

```yaml
apt_url: http://archive.ubuntu.com/ubuntu
packages:
  - git
  - curl
# optional; defaults shown
components:
  - main
  - restricted
  - universe
  - multiverse
suites:
  - release
  - updates
  - security
```

`apt_url` should point to an Ubuntu mirror that contains the ISO release. The
builder detects the release codename from the ISO and expands `release`,
`updates`, `security`, and `backports` to codename-based suites such as
`noble`, `noble-updates`, `noble-security`, and `noble-backports`.

This feature does not change `autoinstall.packages`. If your user-data already
uses `autoinstall.packages`, the Ubuntu installer will continue to handle those
packages normally. The extra packages listed in `--install-extra-packages` are
installed later from the ISO-local repository, so they can work without guest
internet access as long as all needed dependencies were downloaded by the host.
Inside the guest, APT verifies the embedded repository against Ubuntu's public
archive key using the copied `InRelease` metadata; the package indexes are not
trimmed because that would break Ubuntu's signed hashes.

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

The image includes the compiled builder, QEMU, `qemu-img`, OVMF firmware,
`xorriso`, and the Ubuntu archive keyring.
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
docker run --rm ubuntu-vm-template-builder proxmox prerequisites
```

Supported QEMU disk formats are `raw`, `qcow2`, and `vmdk`.
UEFI installs use a temporary OVMF variables file during QEMU installation; the
disk image is the only persistent output file.
The `vcenter` and `proxmox` commands do not need `/dev/kvm` in the container,
but the container must be able to reach the target API endpoint and mounted
paths must contain the ISO, user-data, and optional config files.

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
