#!/bin/sh
set -eu

OPTIONS_FILE="/data/options.json"
EDGE_URL="https://api.home.ctech.media"
PAIR_API_URL="https://home.ctech.media/api/agent/pair"
HA_BASE_URL="http://homeassistant:8123"

if [ ! -r "${OPTIONS_FILE}" ]; then
  echo "Unable to read ${OPTIONS_FILE}. Check Home Assistant add-on permissions and rebuild the add-on image."
  exit 1
fi

json_string() {
  key="$1"
  sed -n "s/.*\"${key}\"[[:space:]]*:[[:space:]]*\"\\([^\"]*\\)\".*/\\1/p" "${OPTIONS_FILE}" | head -n 1
}

json_value() {
  key="$1"
  sed -n "s/.*\"${key}\"[[:space:]]*:[[:space:]]*\\([^,}]*\\).*/\\1/p" "${OPTIONS_FILE}" | head -n 1 | tr -d '" '
}

PAIRING_CODE="$(json_string pairing_code)"
LOG_LEVEL="$(json_string log_level)"
HA_HOST="$(json_string ha_host)"
HA_PORT="$(json_value ha_port)"
ALLOW_CUSTOM_TARGETS="$(json_value allow_custom_targets)"

if [ -z "${LOG_LEVEL}" ]; then
  LOG_LEVEL="info"
fi
if [ "${ALLOW_CUSTOM_TARGETS}" != "false" ]; then
  ALLOW_CUSTOM_TARGETS="true"
fi
if [ -z "${HA_HOST}" ]; then
  HA_HOST="homeassistant"
fi
HA_HOST="${HA_HOST#http://}"
HA_HOST="${HA_HOST#https://}"
HA_HOST="${HA_HOST%%/*}"
port_responds() {
  port="$1"
  if wget -q -T 2 -O /dev/null "http://${HA_HOST}:${port}/" >/dev/null 2>&1; then
    return 0
  fi
  status="$?"
  [ "${status}" -eq 8 ]
}

if [ -n "${HA_PORT}" ]; then
  case "${HA_PORT}" in
  ''|*[!0-9]*)
    echo "Invalid ha_port '${HA_PORT}', falling back to 8123"
    HA_PORT="8123"
    ;;
  esac
  if [ "${HA_PORT}" -lt 1 ] || [ "${HA_PORT}" -gt 65535 ]; then
    echo "Invalid ha_port '${HA_PORT}', falling back to 8123"
    HA_PORT="8123"
  fi
  echo "ha_port=manual ${HA_PORT}"
  HA_PORT_CANDIDATES=""
else
  # The add-on may start before Home Assistant is up; wait up to 90s for
  # either port instead of settling on a guess. The agent re-checks both
  # ports later if Home Assistant refuses connections.
  HA_PORT_CANDIDATES="8123,80"
  HA_PORT=""
  attempt=0
  while [ -z "${HA_PORT}" ] && [ "${attempt}" -lt 30 ]; do
    if port_responds 8123; then
      HA_PORT="8123"
    elif port_responds 80; then
      HA_PORT="80"
    else
      attempt=$((attempt + 1))
      sleep 3
    fi
  done
  if [ -n "${HA_PORT}" ]; then
    echo "ha_port=automatic ${HA_PORT}"
  else
    HA_PORT="8123"
    echo "ha_port=automatic fallback 8123; Home Assistant did not answer on port 8123 or 80 yet, the agent keeps checking"
  fi
fi
HA_BASE_URL="http://${HA_HOST}:${HA_PORT}"

echo "Starting HA Remote agent"
echo "edge_url=configured"
echo "pair_api_url=configured"
echo "ha_base_url=${HA_BASE_URL}"
echo "log_level=${LOG_LEVEL}"
echo "allow_custom_targets=${ALLOW_CUSTOM_TARGETS}"
echo "trusted_proxies example for Home Assistant configuration.yaml:"
echo "http:"
echo "  use_x_forwarded_for: true"
echo "  trusted_proxies:"
echo "    - 172.30.33.0/24"
if [ -n "${PAIRING_CODE}" ]; then
  echo "pairing_code=configured"
else
  echo "pairing_code=empty; using saved tunnel token if available"
fi

exec /usr/local/bin/agent \
  -pairing-code "${PAIRING_CODE}" \
  -edge-url "${EDGE_URL}" \
  -pair-api "${PAIR_API_URL}" \
  -ha-base-url "${HA_BASE_URL}" \
  -log-level "${LOG_LEVEL}" \
  -allow-custom-targets="${ALLOW_CUSTOM_TARGETS}" \
  -ha-port-candidates "${HA_PORT_CANDIDATES}" \
  -token-file "/data/tunnel-token.json"
