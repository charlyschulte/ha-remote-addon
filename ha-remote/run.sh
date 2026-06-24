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

PAIRING_CODE="$(json_string pairing_code)"
LOG_LEVEL="$(json_string log_level)"

if [ -z "${LOG_LEVEL}" ]; then
  LOG_LEVEL="info"
fi

echo "Starting HA Remote agent"
echo "edge_url=configured"
echo "pair_api_url=configured"
echo "ha_base_url=local Home Assistant instance"
echo "log_level=${LOG_LEVEL}"
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
  -token-file "/data/tunnel-token.json"
