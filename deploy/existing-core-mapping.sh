#!/bin/sh

mapped_engines=""
mapped_xray_binary=""
mapped_xray_config=""
mapped_xray_service=""
mapped_singbox_binary=""
mapped_singbox_config=""
mapped_singbox_config_directory=""
mapped_singbox_service_binary=""
mapped_singbox_service=""

xray_service_candidates="xray.service"
xray_binary_candidates="/usr/local/bin/xray /usr/bin/xray"
xray_config_candidates="/usr/local/etc/xray/config.json /etc/xray/config.json"
singbox_service_candidates="sing-box.service singbox.service"
singbox_binary_candidates="/usr/local/bin/sing-box /usr/bin/sing-box"
singbox_config_candidates="/etc/sing-box/config.json /usr/local/etc/sing-box/config.json"

append_csv() {
  current=$1
  value=$2
  if [ -n "$current" ]; then
    printf '%s,%s' "$current" "$value"
  else
    printf '%s' "$value"
  fi
}

protected_regular_file() {
  candidate=$1
  executable=${2:-false}
  [ -f "$candidate" ] && [ ! -L "$candidate" ] || return 1
  [ "$(stat -c '%u' "$candidate" 2>/dev/null)" = 0 ] || return 1
  permissions=$(stat -c '%a' "$candidate" 2>/dev/null) || return 1
  [ $((0$permissions & 022)) -eq 0 ] || return 1
  if [ "$executable" = true ]; then [ -x "$candidate" ] || return 1; fi
  parent=$(dirname -- "$candidate")
  [ -d "$parent" ] && [ ! -L "$parent" ] || return 1
  [ "$(stat -c '%u' "$parent" 2>/dev/null)" = 0 ] || return 1
  permissions=$(stat -c '%a' "$parent" 2>/dev/null) || return 1
  [ $((0$permissions & 022)) -eq 0 ]
}

protected_directory_chain() {
  chain_directory=$1
  case "$chain_directory" in /*) ;; *) return 1 ;; esac
  while :; do
    [ -d "$chain_directory" ] && [ ! -L "$chain_directory" ] || return 1
    chain_permissions=$(stat -c '%a' "$chain_directory" 2>/dev/null) || return 1
    chain_mode=$((0$chain_permissions))
    if [ $((chain_mode & 022)) -ne 0 ]; then
      [ $((chain_mode & 01000)) -ne 0 ] || return 1
      [ "$(stat -c '%u' "$chain_directory" 2>/dev/null)" = 0 ] || return 1
      return 0
    fi
    [ "$(stat -c '%u' "$chain_directory" 2>/dev/null)" = 0 ] || return 1
    [ "$chain_directory" != / ] || return 0
    chain_directory=$(dirname -- "$chain_directory")
  done
}

protected_config_directory() {
  config_directory=$1
  primary=$2
  protected_directory_chain "$config_directory" || return 1
  total=$(wc -c < "$primary") || return 1
  for config_candidate in "$config_directory"/*.json; do
    [ -e "$config_candidate" ] || continue
    protected_regular_file "$config_candidate" false || return 1
    size=$(wc -c < "$config_candidate") || return 1
    total=$((total + size))
    [ "$total" -le 2097152 ] || return 1
  done
}

resolve_fixed_singbox_binary() {
  service_binary=$1
  if [ ! -L "$service_binary" ]; then
    protected_directory_chain "$(dirname -- "$service_binary")" || return 1
    protected_regular_file "$service_binary" true || return 1
    [ "$(head -c 2 "$service_binary" 2>/dev/null)" != '#!' ] || return 1
    printf '%s\n' "$service_binary"
    return 0
  fi
  protected_directory_chain "$(dirname -- "$service_binary")" || return 1
  wrapper=$(readlink -- "$service_binary" 2>/dev/null) || return 1
  case "$wrapper" in /*) ;; *) wrapper=$(dirname -- "$service_binary")/$wrapper ;; esac
  wrapper_parent=$(cd -P -- "$(dirname -- "$wrapper")" 2>/dev/null && pwd -P) || return 1
  wrapper=$wrapper_parent/$(basename -- "$wrapper")
  [ ! -L "$wrapper" ] || return 1
  protected_directory_chain "$(dirname -- "$wrapper")" || return 1
  protected_regular_file "$wrapper" true || return 1
  if [ "$(head -c 2 "$wrapper" 2>/dev/null)" != '#!' ]; then
    printf '%s\n' "$wrapper"
    return 0
  fi
  first=$(sed -n '1p' "$wrapper")
  second=$(sed -n '2p' "$wrapper")
  third=$(sed -n '3p' "$wrapper")
  [ "$first" = '#!/bin/sh' ] && [ -z "$third" ] || return 1
  prefix='exec '
  suffix=' "$@"'
  case "$second" in "$prefix"*"$suffix") ;; *) return 1 ;; esac
  real_binary=${second#"$prefix"}
  real_binary=${real_binary%"$suffix"}
  case "$real_binary" in /*) ;; *) return 1 ;; esac
  case "$real_binary" in *[[:space:]]*) return 1 ;; esac
  protected_directory_chain "$(dirname -- "$real_binary")" || return 1
  protected_regular_file "$real_binary" true || return 1
  [ "$(head -c 2 "$real_binary" 2>/dev/null)" != '#!' ] || return 1
  printf '%s\n' "$real_binary"
}

single_exec_start_argv() {
  exec_start=$1
  case "$exec_start" in
    *'
'*|*'} {'*|*'; path='*) return 1 ;;
  esac
  prefix='{ path='
  case "$exec_start" in "$prefix"*) ;; *) return 1 ;; esac
  remainder=${exec_start#"$prefix"}
  executable=${remainder%%' ; argv[]='*}
  [ "$remainder" != "$executable" ] || return 1
  remainder=${remainder#"$executable ; argv[]="}
  argv=${remainder%%' ; ignore_errors='*}
  [ "$remainder" != "$argv" ] || return 1
  metadata=${remainder#"$argv"}
  case "$metadata" in ' ; ignore_errors='*' }') ;; *) return 1 ;; esac
  metadata_without_closing=${metadata%\}}
  [ "$metadata_without_closing" != "$metadata" ] || return 1
  case "$metadata_without_closing" in *'{'*|*'}'*) return 1 ;; esac
  [ -n "$executable" ] && [ -n "$argv" ] || return 1
  printf '%s\n%s\n' "$executable" "$argv"
}

service_uses_paths() {
  service=$1
  binary=$2
  config=$3
  engine=$4
  systemctl is-active --quiet "$service" 2>/dev/null || return 1
  exec_start=$(systemctl show "$service" --property=ExecStart --value 2>/dev/null) || return 1
  parsed=$(single_exec_start_argv "$exec_start") || return 1
  executable=$(printf '%s\n' "$parsed" | sed -n '1p')
  argv=$(printf '%s\n' "$parsed" | sed -n '2p')
  [ "$executable" = "$binary" ] || return 1
  matched_config_directory=""
  case "$engine:$argv" in
    "xray:$binary run -config $config"|"xray:$binary run -c $config"|\
    "sing-box:$binary run -c $config"|"sing-box:$binary run --config $config") return 0 ;;
  esac
  if [ "$engine" = sing-box ]; then
    prefix="$binary run -c $config -C "
    case "$argv" in "$prefix"*) directory=${argv#"$prefix"} ;; *) return 1 ;; esac
    case "$directory" in /*) ;; *) return 1 ;; esac
    case "$directory" in *[[:space:]]*) return 1 ;; esac
    protected_config_directory "$directory" "$config" || return 1
    matched_config_directory=$directory
    return 0
  fi
  return 1
}

qagent_core_service_is_safe_to_disable() {
  engine=$1
  service="qagent-$engine.service"
  expected_fragment=${2:-/etc/systemd/system/$service}
  load_state=$(systemctl show "$service" --property=LoadState --value 2>/dev/null) || return 1
  [ "$load_state" = not-found ] && return 0
  [ "$load_state" = loaded ] || return 1
  active_state=$(systemctl show "$service" --property=ActiveState --value 2>/dev/null) || return 1
  case "$active_state" in inactive|failed) ;; *) return 1 ;; esac
  fragment_path=$(systemctl show "$service" --property=FragmentPath --value 2>/dev/null) || return 1
  [ "$fragment_path" = "$expected_fragment" ] || return 1
  protected_regular_file "$fragment_path" false || return 1
  grep -q '^Description=.* managed by QAgent$' "$fragment_path"
}

skip_core_service() {
  engine=$1
  case ",${QCH_SKIP_CORE_SERVICES:-}," in
    *",$engine,"*) return 0 ;;
    *) return 1 ;;
  esac
}

require_skipped_core_service_inactive() {
  engine=$1
  skip_core_service "$engine" || return 0
  if systemctl is-active --quiet "qagent-$engine.service"; then
    printf '%s\n' "refusing to alter active qagent-$engine.service while mapping another service" >&2
    return 1
  fi
}

inspect_existing_candidate() {
  engine=$1
  binary=$2
  config=$3
  case "$engine" in
    xray)
      QCH_XRAY_BINARY=$binary QCH_XRAY_CONFIG=$config \
        "$work_dir/qagent" inspect-existing xray >/dev/null 2>&1
      ;;
    sing-box)
      QCH_SING_BOX_BINARY=$binary QCH_SING_BOX_CONFIG=$config \
        QCH_SING_BOX_CONFIG_DIRECTORY=${4:-} QCH_SING_BOX_SERVICE_BINARY=${5:-$binary} \
        "$work_dir/qagent" inspect-existing sing-box >/dev/null 2>&1
      ;;
    *) return 1 ;;
  esac
}

discover_existing_xray() {

  match_count=0
  found_binary=""
  found_config=""
  found_service=""
  for service in $xray_service_candidates; do
    for binary in $xray_binary_candidates; do
      protected_regular_file "$binary" true || continue
      for config in $xray_config_candidates; do
        protected_regular_file "$config" false || continue
        [ "$(wc -c < "$config")" -le 2097152 ] || continue
        service_uses_paths "$service" "$binary" "$config" xray || continue
        inspect_existing_candidate xray "$binary" "$config" || continue
        match_count=$((match_count + 1))
        found_binary=$binary
        found_config=$config
        found_service=$service
      done
    done
  done
  [ "$match_count" -eq 1 ] || {
    [ "$match_count" -le 1 ] || printf '%s\n' 'ambiguous existing Xray services were left unmanaged' >&2
    return 1
  }
  if ! qagent_core_service_is_safe_to_disable xray; then
    printf '%s\n' 'refusing installation while qagent-xray.service is active or ambiguous' >&2
    return 2
  fi
  mapped_xray_binary=$found_binary
  mapped_xray_config=$found_config
  mapped_xray_service=$found_service
  mapped_engines=$(append_csv "$mapped_engines" xray)
  printf '%s\n' "detected existing Xray service: $found_service ($found_config)"
}

discover_existing_singbox() {
  match_count=0
  found_binary=""
  found_config=""
  found_config_directory=""
  found_service_binary=""
  found_service=""
  for service in $singbox_service_candidates; do
    for service_binary in $singbox_binary_candidates; do
      resolved_binary=$(resolve_fixed_singbox_binary "$service_binary") || continue
      for config in $singbox_config_candidates; do
        protected_regular_file "$config" false || continue
        [ "$(wc -c < "$config")" -le 2097152 ] || continue
        service_uses_paths "$service" "$service_binary" "$config" sing-box || continue
        config_directory=$matched_config_directory
        inspect_existing_candidate sing-box "$resolved_binary" "$config" "$config_directory" "$service_binary" || continue
        match_count=$((match_count + 1))
        found_binary=$resolved_binary
        found_config=$config
        found_config_directory=$config_directory
        found_service_binary=$service_binary
        found_service=$service
      done
    done
  done
  [ "$match_count" -eq 1 ] || {
    [ "$match_count" -le 1 ] || printf '%s\n' 'ambiguous existing sing-box services were left unmanaged' >&2
    return 1
  }
  if ! qagent_core_service_is_safe_to_disable sing-box; then
    printf '%s\n' 'refusing installation while qagent-sing-box.service is active or ambiguous' >&2
    return 2
  fi
  mapped_singbox_binary=$found_binary
  mapped_singbox_config=$found_config
  mapped_singbox_config_directory=$found_config_directory
  mapped_singbox_service_binary=$found_service_binary
  mapped_singbox_service=$found_service
  mapped_engines=$(append_csv "$mapped_engines" sing-box)
  printf '%s\n' "detected existing sing-box service: $found_service ($found_config)"
}
