# Changelog

## 0.2.0

- Pages with large responses (history, logbook, HACS, big dashboards) no
  longer hang.
- One large download (camera stream, backup, video) no longer blocks the rest
  of Home Assistant: streamed downloads use flow control.
- The tunnel sends keepalive pings and detects dead connections, so it no
  longer drops every few minutes behind proxies with idle timeouts.
- Redirects from Home Assistant (e.g. My Home Assistant links, HACS) are
  passed to the browser instead of being followed inside the add-on.
- Encoded characters in URLs (`%2F`) are preserved.
- Websockets open without delaying other requests; terminal add-ons that use
  websocket subprotocols work through the tunnel.
- New option **Allow forwarding to other services**: subdomains configured in
  the HA Remote dashboard can forward to other devices in your local network.
- Clearer log messages, faster reconnects and a clean shutdown when the
  add-on is stopped.
