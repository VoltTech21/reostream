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
| `ledControl`, `IrLights` | IR is Auto or Off |
| `ptzDirection` | direction control even though `ptzCtrl` is absent everywhere, so digital rather than mechanical |
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

`GetStitch` and `SetStitch`, fields `distance`, `stitchXMove`, `stitchYMove`. Sending
`action: 1` returns the current value, the factory default and the valid range together,
which is everything needed to adjust it without guessing at bounds.

Measured on the pano here:

| field | type | range | default | current |
|---|---|---|---|---|
| `distance` | float | 2.0 to 20.0 | 8.0 | 8.0 |
| `stitchXMove` | int | -100 to 100 | 0 | 0 |
| `stitchYMove` | int | -100 to 100 | 0 | 0 |

An earlier note here recorded distance 10.0 with x and y at 3 and 4, and concluded the
camera had been adjusted from factory. Re-measured, everything reads factory default. The
discrepancy is unexplained and the earlier reading should not be trusted.

**`distance` is not a third offset.** Three separate pieces of evidence say so:

- The firmware has a dedicated type for it, `net_distance_t`, with its own parser,
  `get_distance_from_xmlnode(net_distance_t*, TiXmlNode*, const char*)`. The two moves have
  no such type; they are plain integers.
- The functions that handle this configuration are named for two things, not one:
  `nets_stitch_and_dc_x2s`, `nets_stitch_and_dc_v3_s2x`, and the stored blobs
  `StitchAndDc`, `StitchV2Dc`, `StitchV3Dc`. Stitching is consistently paired with a
  second concern abbreviated Dc.
- The ranges say it outright. `distance` is a float from 2 to 20 defaulting to 8, which is
  the shape of a physical measurement. The two moves are integers symmetric about a zero
  default, which is the shape of an offset.

The reason a stitched dual lens camera needs a distance at all is parallax. The two lenses
sit a few centimetres apart, so in the region where their fields of view overlap, an object
appears in a different place in each image, and by how much depends on how far away it is.
Near objects shift a lot between the two views, distant objects barely at all. That means no
single alignment is correct for the whole scene: a seam can only be made to disappear at one
depth. `distance` chooses that depth. Set it to roughly how far away the things you care
about at the seam are, and they line up; objects much nearer than that duplicate across the
seam, objects much further get clipped out of it.

`stitchXMove` and `stitchYMove` are a fixed nudge of one image against the other, applied on
top, and they correct something else entirely: mechanical tolerance in how the two lens
modules are mounted. That error is the same no matter what the camera is looking at, which
is exactly why it is a constant rather than a function of depth.

So the two controls fix two different errors. If the seam is misaligned the same way
everywhere in the frame regardless of subject, that is X and Y. If it lines up for things at
one depth and splits or doubles for things at another, that is `distance`.

Unlike the fisheye mode change, none of these reboot the camera.

**Confidence.** The ranges and the type are measured. Reading Dc as distance correction is
inference from naming, not proof; the strings never expand it. The parallax explanation
follows from the geometry of any two lens stitch and from `distance` being a physical
quantity in the first place, but it has not been confirmed by changing the value and
watching the image, which is the obvious next test and needs someone at the camera.

## What this fleet cannot exercise

Surveyed by asking all eight cameras for `GetAbility` and keeping the capabilities whose
permit is 0 everywhere. 190 capabilities are reported in total; 121 are on every camera,
17 on some, and these 52 on none. Each group is one purchase away from being testable, so
they are grouped by what would unlock them rather than listed flat.

**A PTZ camera** unlocks the largest group by far, 16 capabilities: `ptzCtrl`, `ptzPreset`,
`ptzPatrol`, `ptzTattern`, `ptzType`, `supportPt`, `supportZoom`, `supportFocus`,
`disableAutoFocus`, `supportDigitalZoom`, `supportPtzSpeed`, `supportPtzCalibration`,
`supportPtzCheck`, `supportPtzPresetImage`, `supportZoomAndFocusSliderCfg`,
`supportGuardPointImage`. Note that `ptzDirection` IS present on all eight and `ptzCtrl` is
absent on all eight, so whatever the former does here it is not motor control.

**A PTZ camera also unlocks auto-tracking**, which is a second group of five that depends
on the first: `aiTrack`, `aiTrackDogCat`, `supportAITrackLimit`, `supportAiTrackClassify`,
`supportAutoTrackStream`.

**A battery camera**: `battery`, `batAnalysis`, `mdWithPir`. This is the important one for
anyone else, since battery models have no RTSP at all and are the reason most people ran
the previous tool.

**A wireless camera**: `wifi`, `supportWiFiFreqPolicy`, `3g`.

**A microSD card**, not a camera: `disk`. These cameras have slots, they are just empty
because an NVR does the recording. `GetHddInfo` is the tell: it answers `code 0` with an
empty `HddInfo` list, which is a supported command reporting no media, not a refusal. An
unsupported command looks like `GetRec` does here, `"detail": "not support", "rspCode": -9`.
So on-camera storage is one card away from being testable, with no new hardware.

**A camera with alarm terminals**: `alarmIoIn`, `alarmIoOut`, `alarmRf`.

**A camera with a buzzer**: `supportBuzzer`, `supportBuzzerEnable`, `supportBuzzerTask`,
`supportBuzzerTaskEnable`.

**Nothing here has these at all**, and no obvious single model would bring them:
`supportGop`, `mainEncType`, `supportEncoderSelect`, `supportAiFace`, `floodLight`,
`supportFLKeepOn`, `indicatorLight`, `powerLed`, `isp3Dnr`, `ispBackLight`,
`ispExposureMode`, `ispHue`, `mdTriggerAudio`, `mdTriggerRecord`, `ftpPic`,
`supportAoAdjust`, `supportImportExportImage`.

`supportGop` matters to this project specifically: no camera here reports it, so the GOP
configuration work has nothing to test against on this fleet.

`floodLight` reading 0 while the pano reports `supportFLswitch`, `supportFLBrightness`,
`supportFLIntelligent` and `supportFLSchedule` is a contradiction worth resolving before
trusting either flag.

## Two-way audio, and why `talk` is not the flag to trust

`talk` reports present on all eight cameras. Only two of them have a speaker.

Every other audio *output* capability lands on the same two and no others:

| capability | cameras |
|---|---|
| `talk` | all eight |
| `alarmAudio`, `customAudio`, `supportAudioAlarm` and its Enable/Schedule/TaskEnable | fisheye, pano |
| `supportAudioPlay` | fisheye |

`talk` is the outlier, and the fleet behaviour matches the cluster rather than the flag:
the fisheye and the pano open a talk session repeatedly and play audibly, while the other
six accept a TalkConfig exactly once, return 422 on every attempt afterwards, and never
make a sound. A camera with no speaker appears to accept the configuration and then wedge
the feature until it reboots.

So treat the cluster as the capability test and `talk` as meaningless on its own. Nothing
is harmed by trying: the six that refuse carry on streaming, recording and snapping
normally, and only their two-way audio is unavailable until a reboot.

## What a battery PTZ would and would not close

The 52 grouped again, this time against one hypothetical purchase, because a battery pan
and tilt camera covers four of the groups above at once. This is a mapping of ability flags
to physical hardware, not a measurement: no such camera has been queried here, and the
sweep should be re-run the moment one is.

**Certain**, because the hardware class guarantees them (10): `battery`, `batAnalysis`,
`mdWithPir`, `wifi`, `supportWiFiFreqPolicy`, `ptzCtrl`, `ptzPreset`, `ptzType`,
`supportPt`, `supportPtzSpeed`.

**Only on the right model** (11): the auto-tracking set `aiTrack`, `aiTrackDogCat`,
`supportAiTrackClassify`, `supportAutoTrackStream`, `supportAITrackLimit` needs a tracking
model; `floodLight` and `supportFLKeepOn` need a spotlight; `supportDigitalZoom`,
`supportPtzCheck`, `supportPtzPresetImage` and `supportGuardPointImage` are common on pan
and tilt models but not universal.

**Still absent afterwards**, best case, 30 of the 52:

| still missing | why | count |
|---|---|---|
| `supportZoom`, `supportFocus`, `disableAutoFocus`, `supportZoomAndFocusSliderCfg`, `supportPtzCalibration` | needs a motorised optical lens, which battery models do not carry | 5 |
| `ptzPatrol`, `ptzTattern` | patrol and pattern are wired PTZ features | 2 |
| `supportBuzzer`, `supportBuzzerEnable`, `supportBuzzerTask`, `supportBuzzerTaskEnable` | needs a model with a buzzer | 4 |
| `alarmIoIn`, `alarmIoOut`, `alarmRf` | needs physical alarm terminals | 3 |
| `isp3Dnr`, `ispBackLight`, `ispExposureMode`, `ispHue` | ISP controls absent from this firmware generation | 4 |
| `supportGop`, `mainEncType`, `supportEncoderSelect` | encoder controls, absent fleet wide | 3 |
| `mdTriggerAudio`, `mdTriggerRecord` | motion trigger actions | 2 |
| `indicatorLight`, `powerLed` | status LED control | 2 |
| `supportAiFace` | face detection, doorbell and a few other models | 1 |
| `ftpPic`, `supportAoAdjust`, `supportImportExportImage` | miscellaneous, no obvious single model | 3 |
| `3g` | cellular, needs an LTE model rather than a WiFi one | 1 |

`disk` is not in that table because it needs a microSD card rather than a camera.

The lens group is the only one worth a second purchase, and only if the zoom and focus
surface is ever implemented here. Most of the rest are settings this firmware generation
appears not to carry at all, so chasing them may be chasing nothing.

## Two ability lists, and what each is actually for

There are two, they are not the same question, and neither answers it fully.

**`GetAbility` over HTTP** returns around 190 entries with a permit per entry, and it does
vary by model: it is what says the fisheye has `supportFishEyeCfg` and the pano has
`supportBinoStitch`. This is the closest thing to a hardware capability list. It is also
the one that reports `talk` on six cameras with no speaker, so it is not to be trusted
entry by entry.

**`AbilityInfo`, Baichuan message 151**, returns a much shorter list grouped into system,
network, alarm, record, video and image, each entry suffixed `_rw` or `_ro`. It is a
**per-user permission list, not a hardware list**: all eight cameras here return an
identical 35 entries despite being three different models with visibly different hardware.
It answers "may this user change the LED state", not "does this camera have a speaker".

So `AbilityInfo` cannot be used to decide whether a feature exists. Its value is the
read/write split, which is real and which the HTTP list does not give.
