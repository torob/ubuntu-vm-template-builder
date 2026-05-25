#!/bin/sh
set -eu

target=${1:?target path is required}
repo_src=${2:?offline repository path is required}
config_dir=${3:?offline install config directory is required}

target=${target%/}
[ -n "$target" ] || target=/

guest_repo_path=/var/lib/ubuntu-vm-template-builder/offline-apt
source_list_path=/etc/apt/sources.list.d/ubuntu-vm-template-builder-offline.list
source_parts_path=/tmp/ubuntu-vm-template-builder-empty-sources.d

repo_dst="$target$guest_repo_path"
source_list="$target$source_list_path"
source_parts="$target$source_parts_path"

cleanup() {
  rm -f "$source_list"
  rm -rf "$source_parts" "$repo_dst"
  rmdir "$target/var/lib/ubuntu-vm-template-builder" 2>/dev/null || true
}
trap cleanup EXIT HUP INT TERM

run_in_target() {
  if command -v curtin >/dev/null 2>&1 && [ "$target" != "/" ]; then
    curtin in-target --target="$target" -- "$@"
  elif [ "$target" = "/" ]; then
    "$@"
  else
    chroot "$target" "$@"
  fi
}

[ -d "$repo_src" ] || {
  echo "missing embedded offline APT repo: $repo_src" >&2
  ls -la /cdrom 2>/dev/null || true
  ls -la /cdrom/ubuntu-vm-template-builder 2>/dev/null || true
  exit 1
}
[ -r "$config_dir/packages" ] || {
  echo "missing offline APT package config: $config_dir/packages" >&2
  exit 1
}
[ -r "$config_dir/sources.list" ] || {
  echo "missing offline APT sources config: $config_dir/sources.list" >&2
  exit 1
}
[ -r "$config_dir/required-indexes" ] || {
  echo "missing offline APT index config: $config_dir/required-indexes" >&2
  exit 1
}

rm -rf "$repo_dst"
mkdir -p "$repo_dst" "$(dirname "$source_list")" "$source_parts"
cp -a "$repo_src/." "$repo_dst/"
chmod -R a+rX "$repo_dst"

find "$repo_dst/pool" -type f -name '*.deb' | grep -q . || {
  echo "embedded offline APT repo has no .deb files" >&2
  find "$repo_dst" -maxdepth 5 -type f | sort >&2 || true
  exit 1
}

while IFS= read -r required || [ -n "$required" ]; do
  [ -n "$required" ] || continue
  case "$required" in
    /*|../*|*/../*|*'
'*)
      echo "invalid offline APT required index path: $required" >&2
      exit 1
      ;;
  esac
  [ -r "$repo_dst/$required" ] || {
    echo "missing offline APT required index: $required" >&2
    find "$repo_dst/dists" -maxdepth 4 -type f | sort >&2 || true
    exit 1
  }
done < "$config_dir/required-indexes"

cp "$config_dir/sources.list" "$source_list"

set --
while IFS= read -r package || [ -n "$package" ]; do
  [ -n "$package" ] || continue
  set -- "$@" "$package"
done < "$config_dir/packages"
[ "$#" -gt 0 ] || {
  echo "offline APT package config contains no packages" >&2
  exit 1
}

run_in_target apt-get \
  -o "Dir::Etc::sourcelist=$source_list_path" \
  -o "Dir::Etc::sourceparts=$source_parts_path" \
  -o Apt::Get::List-Cleanup=0 \
  -o Acquire::Languages=none \
  -o Acquire::IndexTargets::deb::DEP-11::DefaultEnabled=false \
  -o Acquire::IndexTargets::deb::CNF::DefaultEnabled=false \
  update

run_in_target apt-get \
  -o "Dir::Etc::sourcelist=$source_list_path" \
  -o "Dir::Etc::sourceparts=$source_parts_path" \
  -o Apt::Get::List-Cleanup=0 \
  -o Acquire::Languages=none \
  -o Acquire::IndexTargets::deb::DEP-11::DefaultEnabled=false \
  -o Acquire::IndexTargets::deb::CNF::DefaultEnabled=false \
  -y install "$@"
