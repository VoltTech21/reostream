# What these cameras can actually do

Surveyed 2026-09-09 across eight Reolink cameras on one network, all read only apart from
the fisheye mode changes noted below. The point of the survey was to find out what is
available before implementing anything, and the answer is that a great deal is exposed and
unused.

Everything here is the camera's own HTTP API on port 80, not Baichuan. Login returns a token
and the token goes in the query string; passing the password in the query string fails with
"password wrong", which looks like a credential problem and is not.

    POST /cgi-bin/api.cgi?cmd=Login
    [{"cmd":"Login","action":0,"param":{"User":{"userName":"admin","password":"..."}}}]

Ask `GetAbility` rather than probing commands. Each capability carries a permit and a
version; permit 0 means absent on that model. Unsupported commands return `ability error`
rather than failing obscurely.

## Present on all eight

| capability | what it is |
|---|---|
| `isp*` | brightness, contrast, saturation, sharpen, white balance, day and night, flip, mirror, anti flicker |
| `ledControl`, `IrLights`, `PowerLed` | IR is Auto or Off; the status LED can be switched off entirely |
| `ptzDirection` | direction control even on cameras with no motors, so digital rather than mechanical |
| `mask`, `snap` | privacy masking, and stills without opening a stream |
| `supportAiPeople`, `supportAiVehicle`, `supportAiDogCat` | **on camera AI detection** |
| `supportAiSensitivity`, `supportAiTargetSize`, `supportAiStayTime` | tuning for the above |

The AI is the surprising one. `GetAiCfg` returns per type detection and tracking flags, so
these cameras classify people, vehicles and animals themselves. A recorder doing its own
inference on a GPU is duplicating work the cameras already do, and camera side detection
keeps working when the recorder is busy or restarting.

## Model specific

| capability | cameras | notes |
|---|---|---|
| `supportFishEyeCfg` | fisheye only | view modes, see below |
| `supportBinoStitch` | pano only | dual lens alignment |
| floodlight: `supportFLswitch`, `FLBrightness`, `FLIntelligent`, `FLSchedule` | pano | a light that can be switched, dimmed and scheduled |
| `supportAudioPlay`, `supportSmartRec` | fisheye | |
| `alarmAudio` | fisheye, pano | |
| `WebHook` | fisheye, pano | up to four endpoints, camera pushes events out |
| `supportAIDenoise`, `supportAiAnimal`, `supportAiSnaptlps` | pano | |
| `videoClip` | the six others | |

`WebHook` is worth noting: those two cameras can POST events to a URL directly, so event
delivery does not have to be polled.

## Fisheye view modes

`GetFishEye` and `SetFishEye`, fields `imageType`, `installType`, `rotationAngle`.

| imageType | output |
|---|---|
| 0 | raw fisheye circle |
| 1 | panorama, the circle unwrapped into a flat band |
| 2 | quad, four dewarped sub views in a 2x2 grid |
| 3 | dual, two dewarped halves stacked |

All four produce 2560x2560. Modes 1 to 3 are genuinely dewarped by the camera, so a client
cropping regions out of the raw circle and correcting them itself is doing work the camera
will do properly.

**Changing `imageType` reboots the camera.** That is normal for this setting, not a fault,
and it has consequences worth planning for:

- the HTTP API returns 502 or nothing for around 15 to 20 seconds
- the auth token is invalidated, so anything after a set needs a fresh login
- a fixed sleep is the wrong tool, poll until the camera answers
- the recorder loses the stream and spawns replacement readers each time, which strand and
  need clearing afterwards

A restore path that reuses a token captured before the change will fail with "please login
first" and leave the camera on the wrong mode. Re-authenticate at the moment of restore.

**Changing the mode invalidates any motion mask or zone drawn against the previous
geometry.** Those coordinates are normalised to the frame, so a fisheye with door zones
drawn on the circular view will have them pointing at the wrong places after a switch to
mode 2 or 3.

## Aiming the sub views

The firmware has `fishEyeSubChnCtrl` carrying a `screenNumber` and a command of left, right,
up, down or reset, so the dewarped sub views are meant to be aimable. Over HTTP, `PtzCtrl`
accepts direction operations and returns success on a camera with no motors, which fits.
A test moving one direction repeatedly produced no visible change, most likely because the
call needs the screen number to say which sub view to move. Unresolved.

## The balanced stream

Alongside `mainStream` and `subStream` the cameras answer `externStream`, the "balanced"
stream the Reolink apps do not expose: roughly 896x512 H.264 at about 1 Mbps, with audio.
It is a useful middle option on the six single lens cameras.

The fisheye does not serve it. A request returns an empty video message and then the
camera's ordinary post login config push, with no media following, which reads exactly
like a held session and is not one.

## Pano stitching

`GetStitch` and `SetStitch`, fields `distance`, `stitchXMove`, `stitchYMove`. The response
carries the current value, the factory default and the valid range together, which is
everything needed to adjust it without guessing at bounds. Measured on the pano here:
distance 10.0 against a default of 8.0, and x and y moved to 3 and 4, so it has been
adjusted from factory at some point.

Unlike the fisheye mode change, this is a numeric nudge and does not reboot the camera.
