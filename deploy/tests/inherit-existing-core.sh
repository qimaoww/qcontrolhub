#!/bin/sh
set -eu

test_root=$(mktemp -d "${TMPDIR:-/tmp}/qcontrolhub-inherit-test.XXXXXX")
trap 'rm -rf "$test_root"' EXIT HUP INT TERM

mkdir -p "$test_root/bin" "$test_root/state" "$test_root/core"
# shellcheck source=../existing-core-mapping.sh
. "$(dirname -- "$0")/../existing-core-mapping.sh"

cat > "$test_root/bin/systemctl" <<'EOF'
#!/bin/sh
set -eu
command=$1
shift
case "$command" in
  is-active)
    [ "$(cat "$FAKE_SYSTEMCTL_ACTIVE")" = active ]
    ;;
  show)
    service=$1
    shift
    property=""
    for argument in "$@"; do
      case "$argument" in --property=*) property=${argument#--property=} ;; esac
    done
    case "$property" in
      ExecStart) cat "$FAKE_SYSTEMCTL_EXEC_START" ;;
      LoadState) cat "$FAKE_SYSTEMCTL_LOAD_STATE" ;;
      ActiveState) cat "$FAKE_SYSTEMCTL_QAGENT_ACTIVE_STATE" ;;
      FragmentPath) cat "$FAKE_SYSTEMCTL_FRAGMENT_PATH" ;;
      *) exit 1 ;;
    esac
    ;;
  *) exit 1 ;;
esac
EOF
chmod 0755 "$test_root/bin/systemctl"

export PATH="$test_root/bin:/usr/bin:/bin"
export FAKE_SYSTEMCTL_ACTIVE="$test_root/state/active"
export FAKE_SYSTEMCTL_EXEC_START="$test_root/state/exec-start"
export FAKE_SYSTEMCTL_LOAD_STATE="$test_root/state/load-state"
export FAKE_SYSTEMCTL_QAGENT_ACTIVE_STATE="$test_root/state/qagent-active-state"
export FAKE_SYSTEMCTL_FRAGMENT_PATH="$test_root/state/fragment-path"
printf '%s\n' active > "$FAKE_SYSTEMCTL_ACTIVE"
printf '%s\n' loaded > "$FAKE_SYSTEMCTL_LOAD_STATE"
printf '%s\n' inactive > "$FAKE_SYSTEMCTL_QAGENT_ACTIVE_STATE"

binary="$test_root/core/xray"
config="$test_root/core/config.json"
wrapper="$test_root/core/xray-wrapper"
printf '%s\n' '{"inbounds":[],"outbounds":[]}' > "$config"
cat > "$binary" <<'EOF'
#!/bin/sh
[ "$1 $2 $3" = "run -test -config" ]
grep -q '"inbounds"' "$4"
EOF
chmod 0755 "$binary"
cat > "$wrapper" <<'EOF'
#!/bin/sh
exit 0
EOF
chmod 0755 "$wrapper"

write_exec_start() {
  executable=$1
  shift
  printf '{ path=%s ; argv[]=%s ; ignore_errors=no ; start_time=[n/a] ; stop_time=[n/a] ; pid=1 ; code=(null) ; status=0/0 }\n' \
    "$executable" "$*" > "$FAKE_SYSTEMCTL_EXEC_START"
}

expect_rejected() {
  label=$1
  shift
  if "$@" >/dev/null 2>&1; then
    printf '%s\n' "expected rejection: $label" >&2
    exit 1
  fi
}

write_exec_start "$binary" "$binary" run -config "$config"
service_uses_paths xray.service "$binary" "$config" xray || {
  printf '%s\n' 'safe single-file ExecStart was rejected' >&2
  exit 1
}

write_exec_start "$wrapper" "$wrapper" "$binary" run -config "$config"
expect_rejected wrapper service_uses_paths xray.service "$binary" "$config" xray

write_exec_start "$binary-wrapper" "$binary-wrapper" run -config "$config"
expect_rejected executable-prefix service_uses_paths xray.service "$binary" "$config" xray

write_exec_start "$binary" "$binary" run -config "$config-other"
expect_rejected config-prefix service_uses_paths xray.service "$binary" "$config" xray

write_exec_start "$binary" "$binary" run -confdir "$(dirname -- "$config")"
expect_rejected config-directory service_uses_paths xray.service "$binary" "$config" xray

write_exec_start "$binary" "$binary" run -config "$config" -config "$config-other"
expect_rejected multiple-configs service_uses_paths xray.service "$binary" "$config" xray

write_exec_start "$binary" "$binary" run -config "$config"
printf '{ path=%s ; argv[]=%s run -config %s ; ignore_errors=no }\n' "$binary" "$binary" "$config" >> "$FAKE_SYSTEMCTL_EXEC_START"
expect_rejected multiple-exec-start service_uses_paths xray.service "$binary" "$config" xray

printf '%s\n' inactive > "$FAKE_SYSTEMCTL_ACTIVE"
write_exec_start "$binary" "$binary" run -config "$config"
expect_rejected inactive-generic-unit service_uses_paths xray.service "$binary" "$config" xray
printf '%s\n' active > "$FAKE_SYSTEMCTL_ACTIVE"

sing_binary="$test_root/core/sing-box"
sing_config="$test_root/core/sing-box.json"
sing_config_directory="$test_root/core/conf.d"
mkdir -m 0700 "$sing_config_directory"
cat > "$sing_binary" <<'EOF'
#!/bin/sh
[ "$1 $2" = "check -c" ]
grep -q '"inbounds"' "$3"
if [ "$#" -eq 5 ]; then [ "$4" = -C ] && [ -d "$5" ]; fi
EOF
chmod 0755 "$sing_binary"
printf '%s\n' '{"inbounds":[]}' > "$sing_config"
printf '%s\n' '{"outbounds":[]}' > "$sing_config_directory/10-outbounds.json"
chmod 0600 "$sing_config" "$sing_config_directory/10-outbounds.json"
write_exec_start "$sing_binary" "$sing_binary" run -c "$sing_config" -C "$sing_config_directory"
service_uses_paths sing-box.service "$sing_binary" "$sing_config" sing-box || {
  printf '%s\n' 'safe sing-box file plus config-directory ExecStart was rejected' >&2
  exit 1
}
[ "$matched_config_directory" = "$sing_config_directory" ] || {
  printf '%s\n' 'sing-box config directory was not captured exactly' >&2
  exit 1
}
write_exec_start "$sing_binary" "$sing_binary" run -c "$sing_config" -C "$sing_config_directory" --unknown
expect_rejected sing-box-unknown-extra service_uses_paths sing-box.service "$sing_binary" "$sing_config" sing-box
write_exec_start "$sing_binary" "$sing_binary" run -c "$sing_config" -C "$sing_config_directory-other"
expect_rejected sing-box-missing-directory service_uses_paths sing-box.service "$sing_binary" "$sing_config" sing-box
ln -s "$sing_config_directory/10-outbounds.json" "$sing_config_directory/20-linked.json"
write_exec_start "$sing_binary" "$sing_binary" run -c "$sing_config" -C "$sing_config_directory"
expect_rejected sing-box-symlinked-directory-entry service_uses_paths sing-box.service "$sing_binary" "$sing_config" sing-box
rm "$sing_config_directory/20-linked.json"

forwarder="$test_root/core/sing-box-forwarder"
service_link="$test_root/core/sing-box-link"
printf '%s\n' '#!/bin/sh' 'exec /usr/bin/true "$@"' > "$forwarder"
chmod 0700 "$forwarder"
ln -s "$forwarder" "$service_link"
[ "$(resolve_fixed_singbox_binary "$service_link")" = /usr/bin/true ] || {
  printf '%s\n' 'fixed sing-box exec forwarder was not resolved safely' >&2
  exit 1
}
printf '%s\n' '#!/bin/sh' 'echo unsafe' 'exec /usr/bin/true "$@"' > "$forwarder"
expect_rejected arbitrary-sing-box-wrapper resolve_fixed_singbox_binary "$service_link"
direct_service_link="$test_root/core/sing-box-direct-link"
ln -s /usr/bin/true "$direct_service_link"
[ "$(resolve_fixed_singbox_binary "$direct_service_link")" = /usr/bin/true ] || {
  printf '%s\n' 'direct sing-box binary symlink was not resolved safely' >&2
  exit 1
}

printf '%s\n' not-found > "$FAKE_SYSTEMCTL_LOAD_STATE"
qagent_core_service_is_safe_to_disable xray || {
  printf '%s\n' 'first install without a dedicated unit was rejected' >&2
  exit 1
}
printf '%s\n' loaded > "$FAKE_SYSTEMCTL_LOAD_STATE"
printf '%s\n' inactive > "$FAKE_SYSTEMCTL_QAGENT_ACTIVE_STATE"
managed_unit="$test_root/core/qagent-xray.service"
printf '%s\n' 'Description=Xray core managed by QAgent' > "$managed_unit"
chmod 0644 "$managed_unit"
printf '%s\n' "$managed_unit" > "$FAKE_SYSTEMCTL_FRAGMENT_PATH"
qagent_core_service_is_safe_to_disable xray "$managed_unit" || {
  printf '%s\n' 'repeat install with an inactive dedicated unit was rejected' >&2
  exit 1
}
custom_unit="$test_root/core/custom-qagent.service"
printf '%s\n' 'Description=custom unit' > "$custom_unit"
chmod 0644 "$custom_unit"
printf '%s\n' "$custom_unit" > "$FAKE_SYSTEMCTL_FRAGMENT_PATH"
expect_rejected custom-dedicated-unit qagent_core_service_is_safe_to_disable xray "$managed_unit"
printf '%s\n' "$managed_unit" > "$FAKE_SYSTEMCTL_FRAGMENT_PATH"
printf '%s\n' active > "$FAKE_SYSTEMCTL_QAGENT_ACTIVE_STATE"
expect_rejected active-dedicated-unit qagent_core_service_is_safe_to_disable xray "$managed_unit"

export QCH_SKIP_CORE_SERVICES=xray
printf '%s\n' inactive > "$FAKE_SYSTEMCTL_ACTIVE"
require_skipped_core_service_inactive xray || {
  printf '%s\n' 'bootstrap rejected an inactive dedicated unit' >&2
  exit 1
}
# Repeated installation must make the same inactive check without starting or
# stopping either unit.
require_skipped_core_service_inactive xray || {
  printf '%s\n' 'bootstrap rejected a repeated inactive-unit check' >&2
  exit 1
}
printf '%s\n' active > "$FAKE_SYSTEMCTL_ACTIVE"
expect_rejected bootstrap-active-dedicated-unit require_skipped_core_service_inactive xray
unset QCH_SKIP_CORE_SERVICES
printf '%s\n' active > "$FAKE_SYSTEMCTL_ACTIVE"

work_dir="$test_root/work"
mkdir -p "$work_dir"
cat > "$work_dir/qagent" <<'EOF'
#!/bin/sh
case "$1 $2" in
  'inspect-existing xray') exec "$QCH_XRAY_BINARY" run -test -config "$QCH_XRAY_CONFIG" ;;
  'inspect-existing sing-box') exec "$QCH_SING_BOX_BINARY" check -c "$QCH_SING_BOX_CONFIG" -C "$QCH_SING_BOX_CONFIG_DIRECTORY" ;;
  *) exit 1 ;;
esac
EOF
chmod 0755 "$work_dir/qagent"
inspect_existing_candidate xray "$binary" "$config" || {
  printf '%s\n' 'safe filesystem/core candidate validation failed' >&2
  exit 1
}
inspect_existing_candidate sing-box "$sing_binary" "$sing_config" "$sing_config_directory" "$sing_binary" || {
  printf '%s\n' 'safe sing-box config-directory candidate validation failed' >&2
  exit 1
}

singbox_service_candidates=sing-box.service
singbox_binary_candidates=$direct_service_link
singbox_config_candidates=$sing_config
printf '%s\n' active > "$FAKE_SYSTEMCTL_ACTIVE"
printf '%s\n' not-found > "$FAKE_SYSTEMCTL_LOAD_STATE"
printf '%s\n' inactive > "$FAKE_SYSTEMCTL_QAGENT_ACTIVE_STATE"
write_exec_start "$direct_service_link" "$direct_service_link" run -c "$sing_config" -C "$sing_config_directory"
discover_existing_singbox || {
  printf '%s\n' 'safe sing-box config-directory installation discovery failed' >&2
  exit 1
}
[ "$mapped_singbox_binary" = /usr/bin/true ] &&
  [ "$mapped_singbox_config" = "$sing_config" ] &&
  [ "$mapped_singbox_config_directory" = "$sing_config_directory" ] &&
  [ "$mapped_singbox_service_binary" = "$direct_service_link" ] &&
  [ "$mapped_singbox_service" = sing-box.service ] || {
  printf '%s\n' 'sing-box installation discovery did not preserve the exact mapping' >&2
  exit 1
}

printf '%s\n' 'existing core mapping regressions passed'
