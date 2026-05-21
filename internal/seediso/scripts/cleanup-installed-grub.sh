#!/bin/sh
set -eu

target=${1:-/target}
target=${target%/}
[ -n "$target" ] || target=/

if [ -n "${GRUB_DEFAULT_FILE:-}" ]; then
  file=$GRUB_DEFAULT_FILE
else
  file="$target/etc/default/grub"
fi

[ -f "$file" ] || exit 0

line=$(grep -m1 "^GRUB_CMDLINE_LINUX_DEFAULT=" "$file" || true)
[ -n "$line" ] || exit 0

value=${line#GRUB_CMDLINE_LINUX_DEFAULT=}
value=${value#\"}
value=${value%\"}
clean=""
for arg in $value; do
  case "$arg" in
    console=tty0|console=ttyS0,115200n8|autoinstall|ds=nocloud\;s=/cdrom/nocloud/|ds=nocloud\\\;s=/cdrom/nocloud/)
      continue
      ;;
  esac
  if [ -n "$clean" ]; then
    clean="$clean $arg"
  else
    clean="$arg"
  fi
done

escaped=$(printf "%s\n" "$clean" | sed "s/[\/&]/\\\\&/g")
sed -i "s|^GRUB_CMDLINE_LINUX_DEFAULT=.*|GRUB_CMDLINE_LINUX_DEFAULT=\"$escaped\"|" "$file"

[ "${SKIP_UPDATE_GRUB:-0}" != "1" ] || exit 0

if command -v curtin >/dev/null 2>&1 && [ "$target" != "/" ]; then
  curtin in-target --target="$target" -- update-grub
elif [ "$target" = "/" ]; then
  update-grub
else
  chroot "$target" update-grub
fi
