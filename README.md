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
- Accessible `/dev/kvm`, because QEMU is launched with `--enable-kvm`
- An Ubuntu live-server ISO containing `/casper/vmlinuz` and `/casper/initrd`

## Usage

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
  --disk-format raw \
  --user-data autoinstall.example.yaml
```

Supported disk formats are `raw`, `qcow2`, and `vmdk`.

## User Data

The `--user-data` file is passed through unchanged into the seed ISO. Put all
guest configuration there, including hostname, user identity, password hash, SSH
keys, networking, storage, locale, and timezone.

The program validates that the file is readable, non-empty, valid YAML, and has a
top-level `autoinstall` mapping before creating any disk image.

See `autoinstall.example.yaml` for a minimal example.
