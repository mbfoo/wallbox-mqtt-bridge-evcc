# MQTT Bridge for Wallbox with evcc support

This is a fork of [jagheterfredrik/wallbox-mqtt-bridge](https://github.com/jagheterfredrik/wallbox-mqtt-bridge).
It adds support for **[evcc](https://evcc.io)**.

There is another wallbox-mqtt-bridge fork from [sweber/wallbox-mqtt-bridge](https://github.com/sweber/wallbox-mqtt-bridge/), which basically covers the same functionality as this one, but it doesn't support Wallbox SW version v6.6.x or newer. This fork was tested with Wallbox SW 6.7.38.


The changes were developed with the assistance of [Claude](https://claude.ai). Note I never did program anything using the go programming language. I reviewed the changes, they looked good and I tested them successfully with my Wallbox Pulsar Plus SW 6.7.38.

This version can be used as a drop-on replacement of the [sweber](https://github.com/sweber/wallbox-mqtt-bridge/) fork. If you previously used this with an older Wallbox firmware, you don't have to change anything in your evcc configuration.

---

## Changes in this fork

### Publish control pilot state
- A new **`control_pilot`** sensor is published to MQTT, exposing the IEC 61851 single-letter state (`A`, `B`, `C`, `D`, `E`, `F`) required by evcc to determine charger status.

### Periodic MQTT publishing
- Selected topics can be configured to re-publish their current value at a fixed interval. This prevents evcc from timing out on topics that are legitimately stable for long periods (e.g. `charging_enable` staying `1` while a car is plugged in).
- Three new `[settings]` keys control this behaviour:
  - `interval_updated_topics` — comma-separated list of topic keys to re-publish periodically
  - `interval_updated_topics_seconds` — re-publish interval in seconds (default: `15`)
  - `verbose_output` — log every publish check to stdout for debugging (default: `false`)

### Debug sensor rename
- The debug `control_pilot` entity (which showed the raw integer + string) has been renamed to `control_pilot_raw` to avoid overwriting the new evcc-compatible sensor.


---

## Getting Started

### Prerequisites

1. [Root your Wallbox](https://github.com/jagheterfredrik/wallbox-pwn)
2. Have an MQTT broker available (e.g. [Mosquitto as a Home Assistant add-on](https://www.youtube.com/watch?v=dqTn-Gk4Qeo))


### Manually upgrading from jagheterfredrik or sweber

You need to have an existing installation of the original bridge from [jagheterfredrik/wallbox-mqtt-bridge](https://github.com/jagheterfredrik/wallbox-mqtt-bridge) or the evcc-fork from [sweber/wallbox-mqtt-bridge](https://github.com/sweber/wallbox-mqtt-bridge/) already running on your Wallbox

To check which architecture your Wallbox uses, SSH into it and run:
```sh
uname -m
# armv7l  → use bridge-armhf
# aarch64 → use bridge-arm64
```

Stop the running service, download the new binary, and restart:

```sh
systemctl stop mqtt-bridge

cp /home/root/mqtt-bridge/bridge /home/root/mqtt-bridge/bridge.bak   # optional backup
wget -O /home/root/mqtt-bridge/bridge http://...
chmod +x /home/root/mqtt-bridge/bridge

systemctl start mqtt-bridge
systemctl status mqtt-bridge
```

Your existing `bridge.ini` configuration is preserved. Add the new evcc settings manually if required (see below).

---

## Configuration

The bridge is configured via `/home/root/mqtt-bridge/bridge.ini`. To create or reconfigure it interactively, run:

```sh
cd /home/root/mqtt-bridge/ && ./bridge --config
```

### bridge.ini

```ini
[mqtt]
host     = 192.168.123.123
port     = 1883
username = 
password = 

[settings]
polling_interval_seconds        = 1
device_name                     = Wallbox
debug_sensors                   = false
power_boost_enabled             = false
interval_updated_topics         = charging_enable,charging_power,control_pilot,charging_current_l1,charging_current_l2,charging_current_l3,added_energy
interval_updated_topics_seconds = 15
verbose_output                  = false

```

---

## evcc Configuration

This is an example evcc configuration (still old .yaml format):

```yaml
meters:
- name: wallbox_meter
  type: custom
  power:
    source: mqtt
    topic: wallbox_<serial>/charging_power/state
    timeout: 180s
  energy:
    source: calc
    mul:
      - source: mqtt
        topic: wallbox_<serial>/added_energy/state
        timeout: 180s
      - source: const
        value: 0.001
  currents:
    - source: mqtt
      topic: wallbox_<serial>/charging_current_l1/state
    - source: mqtt
      topic: wallbox_<serial>/charging_current_l2/state
    - source: mqtt
      topic: wallbox_<serial>/charging_current_l3/state

chargers:
- name: wallbox_charger
  type: custom
  status:
    source: mqtt
    topic: wallbox_<serial>/control_pilot/state
  enabled: # charger enabled state (true/false or 0/1)
    source: mqtt
    topic: wallbox_<serial>/charging_enable/state
  enable: # set charger enabled state (true/false or 0/1)
    source: mqtt
    topic: wallbox_<serial>/charging_enable/set
    payload: ${enable:%d}
  maxcurrent: # set charger max current (A)
    source: mqtt
    topic: wallbox_<serial>/max_charging_current/set

loadpoints:
- title: Car
  charger: wallbox_charger
  meter: wallbox_meter
```

## Prevent Wallbox auto updates
Even with Wallbox auto updates set to disabled in the app, my wallbox updated itself one day from 6.4.14 to 6.7.38

Here are instructions to (hopefully) avoid this in future, taken from [here](https://github.com/jagheterfredrik/wallbox-mqtt-bridge/issues/63#issuecomment-4023057641):

### Layer 1: Pin the sources.list to your version
(not required in my case, as the file already contained my version)
```bash
echo "deb https://pulsar-repo.wall-box.com/microchip/6.7.38 morty main" \
  > /home/root/.wallbox/sources.list
```
### Layer 2: Make both sources.list files immutable
```bash
chattr +i /home/root/.wallbox/sources.list
chattr +i /etc/apt/sources.list
```
Even if software_update receives a new sources.list from the Wallbox backend API, it cannot write it to disk. The service fails harmlessly. Verify:

```bash
lsattr /home/root/.wallbox/sources.list
# Should show: ----i--------e-- /home/root/.wallbox/sources.list
```

To reverse (if you ever want to update intentionally):

```bash
chattr -i /home/root/.wallbox/sources.list
chattr -i /etc/apt/sources.list
```
### Layer 3: Disable the automatic_update scheduler
```bash
systemctl disable automatic_update
systemctl stop automatic_update
```

---

## Acknowledgments

All credit for the original project goes to [@jagheterfredrik](https://github.com/jagheterfredrik), [@sweber](https://github.com/sweber/wallbox-mqtt-bridge/) and contributors. This fork only adds evcc integration on top of their work.
