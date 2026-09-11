# Camera control

What a Reolink camera will tell you about itself, what it will let you change, and the
three things in this area that will mislead you.

Streaming does not need any of this. `cmd/reocam` is a separate binary.

## Start by asking

```
reocam -address 192.0.2.50 -password secret probe
```

No table can say what a given model implements. The ids here were recovered from an NVR
and a hub, and the set any one camera answers is its own. A message a camera does not
implement comes back 405 rather than failing the connection, so sending every known read
is safe, and it is the only honest way to find out.

Three models here, all on one fleet:

| | RLC-810A | FE-P | Duo 3 PoE |
|---|---|---|---|
| supported | 40 | 46 | 48 |
| wants parameters (400) | 11 | 12 | 12 |
| absent (405) | 52 | 45 | 43 |
| hung up | 0 | 0 | 0 |

Re-measured 2026-09-11. The classifier that produced the first version of this table
checked whether a reply had a body before checking its status code, so a message that
answers 400 with an error body was filed as supported, and it had a single catch-all
for everything else, printed as "absent (405)" regardless of what the camera actually
sent. Fixing that moved three messages, the same three on every model: `dns` and
`syscpuload` answer 200 with an empty body and were being counted as absent, and
`usercfg` answers 400 with a body and was being counted as supported. Each model's
count is therefore supported +1, wants-parameters +1, absent -2 from the original
table. The numbers above are from the fixed, status-only classifier; nothing here
implies any camera's actual behavior changed.

The useful number is the agreement: of 103 messages probed, **89 answer the same way on
all three**, and these are not similar cameras. Only 14 differ, and they differ where you
would expect:

| message | RLC-810A | FE-P | Duo 3 PoE |
|---|---|---|---|
| `fisheyecfg` | - | yes | - |
| `binosttichcfg` (dual lens stitch) | - | yes | yes |
| `ptzcurpos` | - | yes | - |
| `aitracklimitcfg`, `aitracktaskcfg` | - | yes | - |
| `aidenoisecfg`, `wifisdbinfo` | - | yes | yes |
| `crosslinedetectcfg`, `intrusiondetectcfg` | - | - | yes |
| `loiteringdetectcfg`, `legacydetectcfg`, `lossdetectcfg` | - | - | yes |
| `accessusercfg` | - | - | yes |
| `crop` | yes | wants args | wants args |

A fisheye has the fisheye and digital PTZ messages, the two lens models have stitching,
and the newest model has the smart detection suite. Nothing here is surprising, which is
the point: probe the camera and believe what it says.

## Where the message ids came from

Not from a packet capture. They are in a dispatch table compiled into the *client* side
of Reolink's own firmware:

| | |
|---|---|
| record | `[id:4][handler_ptr:4][name:32]`, stride 40 |
| anchor | the string `heartbeat\0` followed by a record named `login` |
| NVR `app/netclient` | 183 ids |
| HomeHub Pro `netclient` | 240 ids |
| merged | **246**, in `docs/msgids.json` |

The names are the firmware's own descriptions, verbatim: `get support`, `led set`,
`battery info get`.

Eight of them were already proven against live cameras before the table was found, and
the table agrees with all eight. That agreement is the whole reason to trust the other
238.

The camera side dispatches with a `switch`, so there is no table to read there. Ids a
camera accepts that no NVR or hub ever sends will not appear. `93`, the ping, is one such
id: cameras answer it, and it is not in the table.

### Two namespaces, and why one of them is a trap

The same binaries contain 724 `MSG_*` strings. **They are a different numbering.** That
enum is the device's internal IPC bus, not the wire protocol.

This matters because it produced a wrong answer that survived for a while. `HeartBeat = 5`
came from `rpc_msg_name(5)`, which resolves in the IPC enum to `MSG_APP_HB`. In the
Baichuan dispatch table, **5 is `replay start`**. Every camera model answered 421 to the
"heartbeat" because it was being asked to begin playback of recorded video. The Baichuan
heartbeat is id 0.

If you go looking for more ids, read them out of the dispatch table, not the `MSG_*` pool.

### Recovering more

`docs/msgids.json` is not exhaustive. More ids can come from:

1. Other Reolink client firmware, the same way. A hub or NVR that manages hardware yours
   does not will carry ids yours never sends. The HomeHub Pro contributed the battery and
   sleep messages precisely because it drives battery cameras.
2. A capture of the official client, which is the only method that also yields the *XML
   schemas*. No dispatch table contains those. See the `bcpcap` tool.

Extracting a HomeHub Pro image: `tools/pakextract.py` splits the `.pak`; the `app`
section is UBI whose volume is **squashfs**, so `ubireader_extract_images` then
`unsquashfs`. `ubireader_extract_files` fails on it with "Wrong node type".

## Reading and writing

```
reocam -address ... get all           # every readable block
reocam -address ... get md            # one block, as XML
reocam -address ... set 45 < md.xml   # write it back
```

**Never write a document this tool, or you, invented.** The only correct body for a write
is what the matching read returned with one field changed.

That is not a style preference. It is what makes this work on hardware nobody has seen:
the camera supplies its own schema, including fields this code has never heard of, and
echoing its own document back preserves the exact byte formatting that some messages
insist on. A compact document is accepted for `TalkAbility` and refused for `TalkConfig`.

There are 55 read/write pairs in the table, paired on the firmware's exact wording.

`reocam verify -write` measures how far that goes on a given camera. It reads each
document and writes that same document straight back, which exercises the write path
without changing anything. On one 8 MP wired camera all 20 applicable pairs returned 200
with the document unchanged, and five more were held back because re-applying an encoder
or image configuration interrupts the stream.

Acceptance is not effect, and the two come apart per message. Changing a field and
reading it back on a fresh connection:

| message | result |
|---|---|
| `45 osd set` | takes effect |
| `209 led set` | takes effect |
| `43 email cfg set` | takes effect |
| `47 md set` | **200, and nothing changes** (both models) |
| `288 floodlight set` | **200, and nothing changes** |

`md set` was tried with one sensitivity window changed, all four changed, and `enable`
turned off. The camera answered 200 every time and re-read identical every time.

Repeated on a second model, a Duo 3 PoE, with `osd set` as a control:

| | `45 osd set` | `47 md set` |
|---|---|---|
| RLC-810A | effect | 200, inert |
| Duo 3 PoE | effect | 200, inert |

So this is not one model being odd. A message being accepted, on hardware that implements
the matching read and hands over a full document, still says nothing about whether the
write does anything. Assume nothing here works until its effect has been observed.

## The three things that will mislead you

### 421 means you built the message wrong

A configuration write is a **two section** message: the channel in an extension section,
the document in a second section, exactly as `TalkConfig` has always been sent. Sent as a
single section, a camera answers **421 and changes nothing**.

421 reads like "this model does not support that". It is not. The same 421 came back from
the mis-numbered heartbeat. Do not use it to conclude a feature is absent.

The shape is visible in the header: a two section message has `MsgLen` greater than
`PayloadOff`; a single section message has them equal.

### A reply status is not proof

Message 288 accepts a floodlight command, with the element name `FloodlightManual` taken
from the firmware rather than guessed, and answers **200**. Snapshots taken either side of
it show no change. A wrong element name on the same message answers 400, so the document
is being parsed and then ignored.

`47 md set` behaves the same way and is the more instructive case, because unlike the
floodlight there is no plausible hardware reason for it: the camera implements `md get`,
returns a full document, accepts that document back with any field changed, answers 200,
and re-reads unchanged.

Two way audio "worked" twice before it made any sound: once the camera had refused the
config and nothing checked the reply, once it accepted every packet and played silence.

Verify the effect, not the call. For a config write, read it back on a *fresh connection*.
For anything physical, measure it.

### Some settings are HTTP only

On this firmware the floodlight, the fisheye view modes and the dual lens stitch
parameters are reachable over the camera's CGI API and not over Baichuan, whatever the
dispatch table implies.

`GetWhiteLed` intermittently answers `please login first` (rspCode -6) on a connection
that logged in seconds earlier. The CGI client logs in again and retries once.

## The floodlight, as a worked example

`mode` and `state` are separate fields, and conflating them makes the light look like it
only has an on switch. `mode` is what the light does; `state` is whether it is lit now.

Mapped by setting each mode over CGI and reading the Baichuan `FloodlightTask` back:

| CGI mode | FloodlightTask | behaviour |
|---|---|---|
| 0 | `alarmMode 0, enable 0` | off |
| 1 | `alarmMode 1, enable 1` | lights on detection |
| 2 | `alarmMode 1, enable 1` | lights on detection |
| 3 | `alarmMode 3, enable 3` | runs the schedule |

So `on` is mode 1 with state 1, and `motion` is mode 1 with state 0, which is how these
cameras ship.

## Not covered

- **Battery cameras.** The ids are now known: `574/575` sleep state, `623` sleep status,
  `626/627` battery mode, `694/695` PIR motion, `687` AOV. None have been sent to a
  battery camera, because there is not one here. They sleep, wake on motion, and send
  state messages this client has never parsed. Assume this does not work.
- **Motion boxes.** There are none. `md` (46) is a grid mask, 120x67 on one camera here,
  and `AiCfg` (299) reports `detectType` as a list of types. The only coordinate carrying
  message in all 246 is `723/724 coordinate point`, which is a PTZ target, not per object
  rectangles. Object boxes have to come from your own detector.
