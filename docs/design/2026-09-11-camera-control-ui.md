# The camera control surface

A web interface for changing settings on the cameras themselves, served by
`reocam`. It is the second half of a surface whose first half, the operator
page, ships inside the reostream daemon and is documented separately.

The split is by write target. The operator page reads status and edits
reostream's own config; it never writes to a camera. This writes to cameras,
it is much larger, and a fault in it must never be able to take video down.
That is why it is a separate process rather than more routes on a listener the
streaming daemon owns.

## What makes this hard, and why the design bends around it

A reply status is not proof. Message 47 `md set` and message 288 `floodlight
set` both accept a valid document, on hardware that implements the matching
read, answer 200, and change nothing. Two way audio reported success twice
before it made any sound. So the honest position is that a camera write is
unverified until its effect has been observed, and a UI that renders every 200
as a green tick is lying most of the time.

The second difficulty is that the write surface is mostly untested. There are
55 read/write pairs in the recovered message table. Writes are confirmed to
take effect on three: `45 osd set`, `209 led set`, `43 email cfg set`. Twenty
more accept their own document back unchanged, which exercises the write path
without proving any effect. The rest have never been sent.

This design does not pretend otherwise. It separates what is proven from what
is not, and says which is which on the page.

## Shape

`reocam serve -listen :8563 -config /etc/reostream/config.toml`

The existing binary grows a serve mode. Its own process, its own port, shipped
in the same image so there is still one thing to run. Streaming does not depend
on it and is unaffected by anything it does.

The fleet comes from reostream's config, read with the resolving loader, since
connecting to a camera needs a real password rather than a `$NAME` reference.
This is the opposite of the operator page's camera form, which must use
`LoadRaw` precisely because it writes that file back and resolving would bake
every secret into it. This program never writes that file. The distinction
belongs in a comment where the loader is called, because getting it backwards
is how a credential leak happens.

Authentication matches the operator page: one password, no usernames, constant
time comparison, session cookie, absolute expiry.

### One extraction

The operator page already has a session store, an `authed` wrapper, a template
render helper that clones per call, and a layout. Duplicating them is waste and
importing `internal/control` would drag `Supervisor`, `HubSource` and the rest
of the streaming side into `reocam`.

Move the generic parts to `internal/webui`, used by both. The second consumer
is what earns the extraction. Doing it while building the operator page, with
one consumer, would have been speculative generality.

### A wire fact that shapes the whole program

A configuration read or write opens a short lived Baichuan connection and does
**not** open a video stream. The one connection per main stream rule does not
apply to it, so this program can talk to a camera reostream is currently
streaming without contending for anything. `cmd/reocam` already does this
against a live eight camera fleet.

This is the opposite of the operator page's setup probe, which had to be
guarded against configured cameras precisely because it does open real streams.
Anything added here that opens a stream inherits that guard's problem and must
be treated the same way.

## The curated surface

Three groups, covering what actually gets changed.

**Picture and OSD.** Camera name and timestamp overlay, and the image settings
that are not an emitter: brightness, contrast, day/night switching. IR belongs
to the next group, since it is a light. OSD is one of the three proven writes. Image and encoder settings are reachable
but carry a warning, because `UnsafeToRewrite` already records why: re-applying
an encoder or image configuration makes the camera reconfigure its pipeline,
which interrupts the stream. The warning says that, rather than hiding the
control.

**Lights and IR.** Floodlight over CGI, because its Baichuan write is
known inert on this firmware. White LED over Baichuan, message 209, proven.
`mode` and `state` are presented separately, since conflating them makes the
light look as though it only has an on switch: mode is what the light does,
state is whether it is lit now, and "motion" is mode 1 with state 0, which is
how these cameras ship.

**Time and accounts.** NTP server and timezone, with fleet apply. Accounts are
read only; see the refusals below.

## Writing

The only document sent is the one the matching read returned, with one field
changed. Never a document this program composed.

That is not fastidiousness. It is what makes writes work on models nobody here
has seen: the camera supplies its own schema, including fields this code has
never heard of, and echoing its own document back preserves the exact byte
formatting some messages insist on. A compact document is accepted for
`TalkAbility` and refused for `TalkConfig`.

A configuration write is a two section message: the channel in an extension
section, the document in a second section. Sent as one section a camera answers
421 and changes nothing, and 421 reads exactly like "this model does not
support that". It is not. The same 421 came back from the mis-numbered
heartbeat for a year.

After a write, the page reports one of:

- **Confirmed.** The block was read back on a fresh connection and the field
  holds the new value. Available for configuration writes, where the read
  already exists and the comparison is free.
- **Accepted.** The camera answered 200. No claim is made about effect. This is
  what actuators with no trustworthy read back get, the floodlight included.
- **Refused**, with the status and what it usually means. 421 says the message
  was built wrong rather than that the feature is missing. 405 says this model
  does not implement it.

Every write records the document that was there before, so a single action puts
it back byte identical. That is already how this hardware gets worked on by
hand and it should not require a person to remember to save a copy first.

## The raw view

Per camera: the `probe` result, which is the only honest answer to what a model
implements, and every readable block as XML.

The 55 writable pairs each carry a label: proven, unverified, known inert, or
unsafe to rewrite. The editor is seeded from the read, so the rule against
invented documents holds here too.

This view is where an unverified pair becomes a proven one. Somebody changes a
field, writes it, reads it back on a fresh connection, and observes. The label
is a fact about this codebase's knowledge, not about the camera, and it should
be updated when the knowledge changes.

## Apply to every camera

Limited to settings that should be identical across a fleet. NTP server and
timezone, and nothing else. Both go over CGI (`SetNtp`, `SetTime`), because the
message table contains no Baichuan set time.

This is not a page of its own. It is a checkbox, unticked, on the two forms of
the camera's own time page: the operator is already filling in the value for one
camera, and "also do this to the other seven" is a decision about that same
value rather than a separate screen with a second copy of the same two forms.
Unticked is the default because writing eight cameras has to be asked for. The
page this replaced had no such choice -- its only button was "apply to every
camera" -- which is the complaint it was removed over.

The result is a per camera table, never one summary line. Eight cameras are
eight independent outcomes, and partial success is the normal case rather than
an error: the fisheye and the dual lens model disagree with the other six about
most things. A single green tick over a partial apply is the same lie as
reporting a 200 as proof.

## What this refuses to do

- **Reboot, firmware update and factory reset are not sent.** Not behind a
  confirmation. Not present.
- **Battery camera messages are not sent.** The ids are known: 574/575 sleep,
  626/627 battery mode, 694/695 PIR, 687 AOV. None has ever been sent to a
  battery camera, because there is not one here. They sleep, wake on motion and
  send state messages this client has never parsed. Assume broken means do not
  ship it.
- **Accounts are read only.** Message 59 turned out to be a user config *write*
  sitting in the read sweep, which means `get all` was sending a user config set
  with an empty body against live cameras. Until somebody deliberately
  establishes what it does, a page that can rewrite camera accounts is a page
  that can lock an operator out of their own cameras.
- **Anything with a physical effect confirms every time.** LED, IR, floodlight.
  A light switching on in a shop at two in the morning should never be a stray
  click.
- **Nothing here opens a video stream.** If that changes, the contention
  problem the operator page's probe guard exists for arrives with it.

## Testing

`fakecam` covers the wire shapes, including a regression test for the two
section write form specifically, since the one section version fails as 421 and
421 is indistinguishable from an unsupported feature by inspection.

No test connects to a real camera and no test scans a network.

Live verification at the end, on the bay and lounge cameras, which are the safe
ones. Change a setting, read it back on a fresh connection, restore it, and
confirm the restore is byte identical.

## Not in this document

- Fisheye dewarp modes, per section virtual PTZ and dual lens stitch alignment.
  Those are the next piece and they need this one's shell to hang from.
- Motion detection and recording configuration. `md set` is confirmed inert on
  two models and the recorder does detection anyway.
- Snapshots. Not needed by anything here once actuator verification is out of
  scope.
- Camera discovery. Still unbuilt, still separate.
