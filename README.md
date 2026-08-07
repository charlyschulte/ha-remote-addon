# HA Remote Home Assistant Add-on Repository

This repository contains the HA Remote add-on for Home Assistant.

## Install

1. Open Home Assistant.
2. Go to **Settings** -> **Apps**.
3. Open the menu in the top right.
4. Choose **Repositories**.
5. Add this repository URL:

   ```text
   https://github.com/charlyschulte/ha-remote-addon
   ```

6. Install **HA Remote**.
7. Paste the pairing code from the HA Remote dashboard and start the add-on.

The add-on connects to the local Home Assistant instance automatically. It tries `homeassistant:8123` first and then `homeassistant:80`. If you use a different host or port, enable **Show unused optional configuration options** in the add-on configuration to reveal the advanced `ha_host`, `ha_port`, and `trusted_proxies_config` fields.

## Add-ons

- `ha-remote`: outbound tunnel agent for HA Remote.
