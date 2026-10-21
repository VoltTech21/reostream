# Setup in detail

The README's Quickstart is enough for most installs. This page covers everything around it.

## Ways to run it

| Method | When |
|---|---|
| `docker run` (README Quickstart) | Default. amd64 and arm64 images. |
| `docker compose up` | Same image, using `docker-compose.yml` from this repo. |
| Static binary | No Docker. linux/amd64 and linux/arm64 on every release. |
| systemd | Plain Linux host, with `contrib/reostream.service`. |
| Unraid | Template in `contrib/unraid/`, see below. |
| Build from source | Go 1.26, `go build ./cmd/reostream`. |

### Static binary

```
curl -fsSL -o reostream \
  https://github.com/VoltTech21/reostream/releases/latest/download/reostream-linux-amd64
chmod +x reostream
./reostream -data ./reostream-data
```

It prints the same claim token the container does. No runtime dependencies.

### systemd

Copy the binary to `/usr/local/bin`, create a `reostream` user, and install
`contrib/reostream.service`. It runs with `-data /var/lib/reostream`, which systemd
creates. Read the comment on `KillSignal` and `TimeoutStopSec` before changing either: if
the daemon is killed before it sends its stream-stop messages, the cameras refuse new
connections for several minutes.

## Docker volumes

- The container runs as uid 65532. A named volume (as in `docker-compose.yml`) just works.
  A bind mount to a host directory must be writable by that user:
  `chown -R 65532:65532 ./data` before the first run, or the claim fails to save.
- Mount the whole data directory, not just `config.toml`. Saves are written to a temp
  file and renamed, which needs the directory to be writable.

### Unraid

Until it is in Community Apps, add the template by hand. In the Unraid terminal:

```
mkdir -p /mnt/user/appdata/reostream && chown 65532:65532 /mnt/user/appdata/reostream
wget -O /boot/config/plugins/dockerMan/templates-user/my-reostream.xml \
  https://raw.githubusercontent.com/VoltTech21/reostream/main/contrib/unraid/reostream.xml
```

Then Docker > Add Container, pick `reostream` from the template list, and Apply.
The claim token is in the container's log (Docker tab, click the icon, Logs).

## Claiming

On first start there is no config. The log prints a one-time token
(`XXXX-XXXX-XXXX-XXXX`) and the URL to open. The token is held in memory only, so a
restart before claiming prints a new one.

The claim page fills in a random 16 character password. Keep it unless you have a reason
not to; a password you type must be at least 16 characters. Submitting writes
`/data/config.toml` and the page asks for that password from then on.

### Lost the password

The password is the `password` line under `[control]` in `/data/config.toml`. Change it
there, then restart. With the default named volume:

```sh
docker run --rm -it -v reostream-data:/data alpine vi /data/config.toml
docker restart reostream
```

Only change the line under `[control]`; the ones under `[[camera]]` are camera logins.
With no cameras added yet, it is simpler to start over:

```sh
docker rm -f reostream && docker volume rm reostream-data
```

then run it again and claim with the new token.

## Config file

Cameras can be added from the page or by editing `/data/config.toml`. When editing by
hand, add to the file that is there; do not replace it. A config without a `[control]`
section is treated as unclaimed and sends you back to the claim screen.

```toml
listen = "0.0.0.0:8560"

[rtsp]
listen = "0.0.0.0:8561"

[control]
listen = "0.0.0.0:8562"
password = "$REOSTREAM_CONTROL_PASSWORD"

[[camera]]
name = "driveway"
address = "192.0.2.50"
username = "admin"
password = "$REOSTREAM_DRIVEWAY_PASSWORD"
streams = ["main", "sub"]
rtsp = ["main", "sub"]
```

- Address, username, and password values starting with `$` are  read from that environment variable.
- Unknown keys are an error at startup, so a typo fails at boot instead of serving 404s.
- Saving from the page validates first and reloads only the cameras that changed.

### Home Hub and NVR cameras

A camera paired to a Reolink Home Hub or NVR is reached through the hub, as a numbered
channel starting at 0. Add one `[[camera]]` block per camera, all with the hub's address
and login, each with its own `channel`:

```toml
[[camera]]
name = "porch"
address = "192.0.2.20"
username = "admin"
password = "$HUB_PASSWORD"
channel = 0
streams = ["main", "sub"]

[[camera]]
name = "yard"
address = "192.0.2.20"
username = "admin"
password = "$HUB_PASSWORD"
channel = 1
streams = ["main", "sub"]
```

Channels are added by hand in the config for now; the Setup page does not ask for one.
Video works for hub cameras, but their per-camera settings page is refused, because those
settings would reach channel 0 instead. This is untested on a real hub so far, so please
report how it goes.

## Control page

The control port (8562) is the only part with a password. The streaming port (8560) is
open, which is what a recorder needs, so you can firewall the two separately.

It shows each stream's state, plays it live, edits the config, tails the log, and has a
page per camera for settings, floodlight, NTP and timezone. Writes are labeled by how
sure we are they work (proven, unverified, known inert, unsafe), and a 200 from the
camera is shown as "accepted", not "confirmed", unless it was read back. See
`docs/control.md`.

`listen` without a password refuses to start unless `allow_no_password = true` is set.

## RTSP

On by default at port 8561 (not 8554, which go2rtc uses). A camera's `rtsp` list names
which of its streams are served; cameras added from the page serve all of them. Remove
the `[rtsp]` section to turn it off.

Serves `rtsp://host:8561/<cam>`, `<cam>_sub` and `<cam>_extern`. TCP interleaved is the
default; UDP is offered, but a lost packet is a corrupt frame.

### Two-way audio

On a camera with a speaker, each RTSP stream also offers an ONVIF back channel, so
Frigate's talk button works through go2rtc with no extra config. Only clients that ask
for a back channel see it; ffmpeg and other players get the same streams as before.

reostream checks the camera's own hardware report before offering it. A camera without
a speaker never gets a back channel, because one that is sent a talk session anyway
breaks talk until it reboots. Speech is sent to the camera over its own connection,
opened when audio starts and closed a few seconds after it stops.

## Frames feed

`/<cam>.frames` on port 8560 streams one record per access unit, for clients that feed a
hardware decoder directly. Each record is a 20 byte big-endian header, then the payload
(Annex B video with parameter sets inline, or AAC):

| offset | type | field |
|---|---|---|
| 0 | 2 bytes | magic `RF` |
| 2 | u8 | version, 1 |
| 3 | u8 | kind: 1 keyframe, 2 inter frame, 3 AAC |
| 4 | u8 | codec: 1 H.264, 2 HEVC, 3 AAC |
| 5 | u8 | reserved |
| 6 | u16 | width, 0 until known |
| 8 | u16 | height, 0 until known |
| 10 | u32 | camera time in microseconds (wraps every 71.6 minutes) |
| 14 | u32 | payload length |
| 18 | u16 | server send time, epoch milliseconds mod 65536 |

A subscriber starts at a keyframe and skips to the next one if it falls behind. Served
for the streams in the camera's `rtsp` list.
