#!/bin/sh
# install-agent.sh — QControlHub agent 一键安装（root 执行，无需预装仓库）
#
# 用法：
#   bash deploy/remote/install-agent.sh <control-plane-url|ip[:port]> <add-node-credential> [agent-name]
#
# 示例：
#   QCH_TLS_CA_FILE=/etc/qcontrolhub/control-plane-ca.pem \
#   bash deploy/remote/install-agent.sh https://192.168.31.205:8443 <token> shanghai-edge-01
#
# 从控制面 GET /api/v1/agent-binary 下载 agent 可执行文件，引导核心服务，
# 写入 /etc/qcontrolhub/agent.env，安装 systemd 单元并启动。
set -eu

[ "$(id -u)" -eq 0 ] || { printf '%s\n' 'install-agent.sh must run as root' >&2; exit 1; }

control="${1:?usage: install-agent.sh <control-plane-url|ip[:port]> <add-node-credential> [agent-name]}"
token="${2:?usage: install-agent.sh <control-plane-url|ip[:port]> <add-node-credential> [agent-name]}"
name="${3:-$(hostname)}"
ca_file="${QCH_TLS_CA_FILE:-}"
allow_insecure_live="${QCH_ALLOW_INSECURE_LIVE:-false}"

case "$control" in
  http://*|https://*|ws://*|wss://*) server_url="$control" ;;
  *) server_url="http://$control" ;;
esac
case "$server_url" in
  ws://*) http_origin="http://${server_url#ws://}" ;;
  wss://*) http_origin="https://${server_url#wss://}" ;;
  http://*|https://*) http_origin="$server_url" ;;
  *) printf '%s\n' 'invalid control-plane URL' >&2; exit 1 ;;
esac

work_dir=$(mktemp -d "${TMPDIR:-/tmp}/qcontrolhub-agent.XXXXXX")
trap 'rm -rf "$work_dir"' EXIT HUP INT TERM
repository_dir="$work_dir/qcontrolhub"
mkdir -p "$repository_dir/deploy/systemd" "$repository_dir/examples/configs"

download() {
  source_path=$1
  destination=$2
  if [ -n "$ca_file" ]; then
    curl --fail --silent --show-error --cacert "$ca_file" -H "X-QControlHub-Enrollment: $token" "$http_origin$source_path" -o "$destination"
  else
    curl --fail --silent --show-error -H "X-QControlHub-Enrollment: $token" "$http_origin$source_path" -o "$destination"
  fi
}

echo '== 1/6 下载安装资源 =='
for asset in \
  deploy/bootstrap-core-services.sh \
  deploy/existing-core-mapping.sh \
  deploy/systemd/qagent.service \
  deploy/systemd/qagent-core-journal.conf \
  deploy/systemd/qagent-mihomo.service \
  deploy/systemd/qagent-xray.service \
  deploy/systemd/qagent-sing-box.service \
  deploy/systemd/qagent-shadowsocks-rust.service \
  examples/configs/mihomo-minimal.yaml \
  examples/configs/xray-minimal.json \
  examples/configs/sing-box-minimal.json \
  examples/configs/shadowsocks-rust-minimal.json
do
  download "/install-assets/$asset" "$repository_dir/$asset"
done
. "$repository_dir/deploy/existing-core-mapping.sh"

echo "== 2/6 下载 agent 二进制（控制面 GET /api/v1/agent-binary）=="
download /api/v1/agent-binary "$work_dir/qagent"
[ -s "$work_dir/qagent" ] || { printf '%s\n' 'downloaded agent binary is empty' >&2; exit 1; }
chmod 0755 "$work_dir/qagent"

echo '== 3/6 检测现有核心并引导其余服务 =='
run_discovery() {
  label=$1
  shift
  if "$@"; then
    return 0
  else
    result=$?
  fi
  [ "$result" -eq 1 ] || {
    printf '%s\n' "unsafe $label service state; installation stopped without changing services" >&2
    exit "$result"
  }
}
run_discovery Xray discover_existing_xray
run_discovery sing-box discover_existing_singbox
QCH_SKIP_CORE_SERVICES="$mapped_engines" bash "$repository_dir/deploy/bootstrap-core-services.sh"

echo '== 4/6 写入 agent 环境文件 =='
mkdir -p /usr/local/lib/qagent
install -m 0755 "$work_dir/qagent" /usr/local/lib/qagent/qagent
ln -sfn /usr/local/lib/qagent/qagent /usr/local/bin/qagent
mkdir -p /etc/qcontrolhub /var/lib/qcontrolhub
umask 077
{
  printf '%s\n' "QCH_SERVER_URL=$server_url"
  if [ -n "$ca_file" ]; then printf '%s\n' "QCH_TLS_CA_FILE=$ca_file"; fi
  case "$server_url" in
    http://*|ws://*) printf '%s\n' 'QCH_ALLOW_HTTP=true' "QCH_ALLOW_INSECURE_LIVE=$allow_insecure_live" ;;
  esac
  printf '%s\n' "QCH_ENROLLMENT_TOKEN=$token"
  printf '%s\n' "QCH_AGENT_NAME=$name"
  printf '%s\n' 'QCH_AGENT_LABELS=region=cn-east'
  printf '%s\n' 'QCH_AGENT_STATE=/var/lib/qcontrolhub/agent-state.json'
  printf '%s\n' 'QCH_AGENT_ENGINES=mihomo,xray,sing-box,ss-rust'
  if [ -n "$mapped_xray_config" ]; then
    printf '%s\n' \
      "QCH_EXISTING_XRAY_BINARY=$mapped_xray_binary" \
      "QCH_EXISTING_XRAY_CONFIG=$mapped_xray_config" \
      "QCH_EXISTING_XRAY_SERVICE=$mapped_xray_service"
  fi
  if [ -n "$mapped_singbox_config" ]; then
    printf '%s\n' \
      "QCH_EXISTING_SING_BOX_BINARY=$mapped_singbox_binary" \
      "QCH_EXISTING_SING_BOX_CONFIG=$mapped_singbox_config" \
      "QCH_EXISTING_SING_BOX_CONFIG_DIRECTORY=$mapped_singbox_config_directory" \
      "QCH_EXISTING_SING_BOX_SERVICE_BINARY=$mapped_singbox_service_binary" \
      "QCH_EXISTING_SING_BOX_SERVICE=$mapped_singbox_service"
  fi
} > /etc/qcontrolhub/agent.env
chmod 0600 /etc/qcontrolhub/agent.env

echo '== 5/6 安装 systemd 单元 =='
install -m 0644 "$repository_dir/deploy/systemd/qagent.service" /etc/systemd/system/qagent.service
systemctl daemon-reload

echo '== 6/6 启动 agent =='
systemctl enable qagent.service >/dev/null
# restart also starts an inactive unit and guarantees repeated installation
# replaces the running process with the freshly downloaded binary.
systemctl restart qagent.service
sleep 3
systemctl --no-pager status qagent.service | head -n 10

# 添加节点凭证只用于下载和注册；无论首次还是覆盖安装，只要身份文件
# 存在就立即从环境文件移除，避免添加节点凭证残留。
if [ -s /var/lib/qcontrolhub/agent-state.json ]; then
  sed -i '/^QCH_ENROLLMENT_TOKEN=/d' /etc/qcontrolhub/agent.env
  chmod 0600 /etc/qcontrolhub/agent.env
fi
