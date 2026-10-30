# reostream

reostream connects to Reolink cameras over their own protocol, battery cameras
included, and serves every stream as RTSP (port 8561) and HTTP MPEG-TS (port 8560) for
Frigate, go2rtc or Home Assistant. Its page, where cameras are added and set up, opens
in Home Assistant's sidebar.

## Installing

1. In Home Assistant, go to **Settings > Add-ons > Add-on Store**, open the menu at the
   top right, and choose **Repositories**.
2. Add `https://github.com/VoltTech21/reostream-homeassistant` and close the dialog.
3. Find **reostream** in the store, install it, and start it.
4. Turn on **Show in sidebar**, or use **Open Web UI**.

The add-on is built on the device from the published `ghcr.io/volttech21/reostream`
image of the same version, for amd64 and aarch64 (64-bit ARM).

## First run

Open the page from the sidebar. There is no claim token to copy and no password to set:
Home Assistant has already signed you in, and the first visit claims the install for you.
From there, **Add camera** finds wired cameras on your network by scanning for port 9000,
and battery cameras by their UDP search.

## Signing in, and the page on its own port

Inside Home Assistant the page asks for no password of its own. reostream trusts a
request to be from a signed-in Home Assistant user only when it comes from the
Supervisor's ingress proxy (172.30.32.2) and carries the ingress path header; a request
from anywhere else, with or without that header, is treated like any other.

The add-on uses host networking, so the page is also on port 8562 of the Home Assistant
host. There it asks for a password, which the first visit through Home Assistant set to
a random one nobody has seen. To use the page on that port too, set a password you know
under **Settings > Password**; through Home Assistant it does not ask for the old one.

## Streams for Frigate

Every camera added on the page has these streams, on the Home Assistant host's address:

- `rtsp://<home-assistant-ip>:8561/<camera>` (main), `<camera>_sub` and `<camera>_extern`
- `http://<home-assistant-ip>:8560/<camera>.ts`

**Settings > Connect to NVR** lists the exact URLs for your cameras and can merge them
into a Frigate config you paste in. The Frigate add-on reaches them at the host's LAN
address.

## Cameras as Home Assistant devices

reostream relays the cameras' own detections over MQTT, and can announce each camera to
Home Assistant as a device with its sensors. Under **Settings > Events** on reostream's
page, set the MQTT broker Home Assistant uses -- with the Mosquitto broker add-on, the Home
Assistant host's address and port 1883, and a Home Assistant user for the login -- and tick
**Add the cameras to Home Assistant**. A wired camera gets Motion, Person, Vehicle and
Animal sensors; a battery camera gets Motion and its Battery level.

## Ports

Host networking means these are the host's own ports, and nothing else on the host may
use them:

| Port | What |
|---|---|
| 8560 | HTTP MPEG-TS streams, unauthenticated, for recorders |
| 8561 | RTSP, unauthenticated, for recorders |
| 8562 | The page (also opened through Home Assistant) |
| 2121-2131 | FTP receiver for clips cameras upload, when set up |

The streaming ports have no password, as a recorder needs. If the host is reachable from
networks you do not trust, firewall 8560 and 8561 there.

## Data and backups

Everything reostream keeps, its `config.toml` included, is in the add-on's `/data`
directory, which survives updates and is part of Home Assistant backups. That file holds
your cameras' passwords, so encrypt backups you store elsewhere.

## More

The full setup guide, battery cameras, alerts and the config file are documented at
<https://github.com/VoltTech21/reostream/blob/main/docs/setup.md>.
