# HA Remote Add-on

This add-on is the local Home Assistant tunnel agent.

## Config

- `pairing_code`: generated in the dashboard
- `log_level`: `info` by default. Use `debug` for pairing/tunnel/request diagnostics or `trace` for per-frame/per-chunk logs.

The following settings are hidden by default. Enable **Show unused optional
configuration options** in Home Assistant to display them:

- `ha_host`: advanced setting for the local Home Assistant IP address or hostname. When omitted, it defaults to `homeassistant`.
- `ha_port`: advanced setting for the local Home Assistant HTTP port. When omitted, the add-on tries `8123` first and then `80`.
- `trusted_proxies_config`: copy the example below into your Home Assistant `configuration.yaml` if Home Assistant asks you to configure trusted proxies.

## Local Home Assistant address

By default the add-on connects to the installed Home Assistant instance at:

```text
http://homeassistant:8123
```

If `ha_port` is omitted or empty, the add-on tries `8123` first and then `80`. If neither port answers during startup, it falls back to `8123`.

If you changed the Home Assistant port in the Home Assistant web interface and automatic detection does not find it, set `ha_port` to the new port. If the internal hostname does not work in your setup, set `ha_host` to the local IP address of your Home Assistant system. Enter only the IP address or hostname, without `http://` and without a port.

## Trusted proxies

If Home Assistant shows a trusted proxy error, add this to `configuration.yaml` and restart Home Assistant:

```yaml
http:
  use_x_forwarded_for: true
  trusted_proxies:
    - 172.30.33.0/24
```
