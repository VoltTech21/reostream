# Changelog

Each version of the add-on is the reostream release of the same version. The full notes
for every release are at <https://github.com/VoltTech21/reostream/releases>.

## 0.2.12

Live view in the page plays closer to live. A camera that allows only one connection can be
added from the page with its main stream.

Notes: <https://github.com/VoltTech21/reostream/releases/tag/v0.2.12>

## 0.2.11

reostream shows under Settings > Updates when a new release is out (with the cameras added
to Home Assistant). Saved PTZ views can be deleted from the Live tab. Fixes: an always-on
camera reached by UID has its own sound on RTSP again, controls with an undo no longer
disappear after a save, and the detection zone editor keeps working while a brush is picked.
Clips are no longer included in Home Assistant backups.

Notes: <https://github.com/VoltTech21/reostream/releases/tag/v0.2.11>

## 0.2.10

Lighter on CPU and memory. Battery cameras behind a Home Hub or NVR sleep until watched.
Talk offers G.711 first again. A fix for an NVR's sub stream serving the main one after a
reconnect, and a guard against a battery camera waking for motion over and over.

Notes: <https://github.com/VoltTech21/reostream/releases/tag/v0.2.10>
