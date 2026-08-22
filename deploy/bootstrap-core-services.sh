#!/bin/sh
set -eu

if [ "$(id -u)" -ne 0 ]; then
  printf '%s\n' 'bootstrap-core-services.sh must run as root' >&2
  exit 1
fi

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
repository_dir=$(CDPATH= cd -- "$script_dir/.." && pwd)
mapping_library="$script_dir/existing-core-mapping.sh"
[ -r "$mapping_library" ] || { printf '%s\n' "required mapping library is unavailable: $mapping_library" >&2; exit 1; }
. "$mapping_library"

for command_name in cmp getent grep groupadd id install systemctl useradd; do
  command -v "$command_name" >/dev/null 2>&1 || {
    printf '%s\n' "required command is unavailable: $command_name" >&2
    exit 1
  }
done

service_user=qcontrolhub-core
service_group=qcontrolhub-core
if ! getent group "$service_group" >/dev/null 2>&1; then
  groupadd --system "$service_group"
fi
if ! id "$service_user" >/dev/null 2>&1; then
  nologin_shell=/usr/sbin/nologin
  [ -x "$nologin_shell" ] || nologin_shell=/bin/false
  useradd --system --gid "$service_group" --home-dir /nonexistent --shell "$nologin_shell" "$service_user"
fi

ensure_directory() {
  destination=$1
  if [ -L "$destination" ]; then
    printf '%s\n' "refusing symlinked directory: $destination" >&2
    exit 1
  fi
  if [ ! -d "$destination" ]; then
    install -d -o root -g "$service_group" -m 0750 "$destination"
  fi
}

install_managed_unit() {
  source_file=$1
  destination=$2
  if [ -L "$destination" ]; then
    printf '%s\n' "refusing symlinked managed unit: $destination" >&2
    exit 1
  fi
  if [ -e "$destination" ]; then
    if [ ! -f "$destination" ]; then
      printf '%s\n' "refusing non-regular managed unit: $destination" >&2
      exit 1
    fi
    if ! grep -q '^Description=.* managed by QAgent$' "$destination"; then
      printf '%s\n' "preserved non-QAgent unit: $destination"
      return
    fi
    if cmp -s "$source_file" "$destination"; then
      printf '%s\n' "managed unit already current: $destination"
      return
    fi
  fi
  install -o root -g root -m 0644 "$source_file" "$destination"
  printf '%s\n' "installed managed unit: $destination"
}

install_if_missing() {
  source_file=$1
  destination=$2
  owner=$3
  group=$4
  mode=$5
  if [ -L "$destination" ]; then
    printf '%s\n' "refusing symlinked destination: $destination" >&2
    exit 1
  fi
  if [ -e "$destination" ]; then
    printf '%s\n' "preserved existing file: $destination"
    return
  fi
  install -o "$owner" -g "$group" -m "$mode" "$source_file" "$destination"
  printf '%s\n' "installed: $destination"
}

ensure_directory /etc/qagent/mihomo
ensure_directory /etc/qagent/xray
ensure_directory /etc/qagent/sing-box
ensure_directory /etc/qagent/shadowsocks-rust
ensure_directory /usr/local/lib/qagent
ensure_directory /usr/local/lib/qagent/cores

install_if_missing "$repository_dir/examples/configs/mihomo-minimal.yaml" /etc/qagent/mihomo/config.yaml root "$service_group" 0640
install_if_missing "$repository_dir/examples/configs/xray-minimal.json" /etc/qagent/xray/config.json root "$service_group" 0640
install_if_missing "$repository_dir/examples/configs/sing-box-minimal.json" /etc/qagent/sing-box/config.json root "$service_group" 0640
install_if_missing "$repository_dir/examples/configs/shadowsocks-rust-minimal.json" /etc/qagent/shadowsocks-rust/config.json root "$service_group" 0640

enabled_services=""
for engine in mihomo xray sing-box shadowsocks-rust; do
  require_skipped_core_service_inactive "$engine"
  install_managed_unit "$script_dir/systemd/qagent-$engine.service" "/etc/systemd/system/qagent-$engine.service"
  if skip_core_service "$engine"; then
    require_skipped_core_service_inactive "$engine"
    systemctl disable "qagent-$engine.service" >/dev/null 2>&1 || true
    printf '%s\n' "kept existing $engine service; disabled qagent-$engine.service"
  else
    enabled_services="$enabled_services qagent-$engine.service"
  fi
done

journal_config_dir=/etc/systemd/journald@qagent-cores.conf.d
if [ -L "$journal_config_dir" ]; then
  printf '%s\n' "refusing symlinked journal configuration directory: $journal_config_dir" >&2
  exit 1
fi
install -d -o root -g root -m 0755 "$journal_config_dir"
journal_config=$journal_config_dir/10-qcontrolhub-volatile.conf
if [ -L "$journal_config" ]; then
  printf '%s\n' "refusing symlinked journal configuration: $journal_config" >&2
  exit 1
fi
install -o root -g root -m 0644 "$script_dir/systemd/qagent-core-journal.conf" "$journal_config"

systemctl daemon-reload
[ -z "$enabled_services" ] || systemctl enable $enabled_services >/dev/null
printf '%s\n' 'core services are bootstrapped; install each official binary from the QControlHub node page'
