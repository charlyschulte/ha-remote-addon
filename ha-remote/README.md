# HA Remote Add-on

This add-on is the local Home Assistant tunnel agent.

## Config

- `pairing_code`: generated in the dashboard

The tunnel endpoints are fixed for the hosted service:

- Edge tunnel: `https://api.home.ctech.media`
- Pair API: `https://home.ctech.media/api/agent/pair`
- Local Home Assistant: `http://homeassistant:8123`

`homeassistant` is the internal add-on network name for the installed Home Assistant Core instance. It does not depend on the user's LAN hostname or IP address.

## Behavior

- Pairs with the dashboard API
- Obtains a signed tunnel token
- Opens a persistent websocket tunnel to edge proxy
- Forwards HTTP and WebSocket requests to local Home Assistant
- Strips forwarded proxy headers before local HA requests
