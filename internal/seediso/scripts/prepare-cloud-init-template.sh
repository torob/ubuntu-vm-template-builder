#!/bin/sh
set -eu

target=${1:?target path is required}
target=${target%/}
[ -n "$target" ] || target=/

run_in_target() {
  if command -v curtin >/dev/null 2>&1 && [ "$target" != "/" ]; then
    curtin in-target --target="$target" -- "$@"
  elif [ "$target" = "/" ]; then
    "$@"
  else
    chroot "$target" "$@"
  fi
}

mkdir -p "$target/etc/cloud/cloud.cfg.d"
cat > "$target/etc/cloud/cloud.cfg.d/90-template-ssh-hostkeys.cfg" <<'EOF'
ssh_deletekeys: true
ssh_genkeytypes:
  - rsa
  - ecdsa
  - ed25519
ssh_quiet_keygen: true
ssh_publish_hostkeys:
  enabled: false
EOF

rm -f "$target"/etc/ssh/ssh_host_*key*
run_in_target cloud-init clean --logs --machine-id
rm -f \
  "$target/etc/cloud/cloud-init.disabled" \
  "$target/etc/cloud/cloud.cfg.d/00-subiquity-disable-cloudinit-networking.cfg" \
  "$target/etc/cloud/cloud.cfg.d/99-installer.cfg"
