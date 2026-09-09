# Groundwork for pano stitching and fisheye control

Notes from reading the NVR firmware, 2026-09-08. Nothing here is implemented yet, and
nothing here should be implemented from this document alone: these are names to look for on
the wire, not a specification. The wire is the specification. See CONTRIBUTING for why that
distinction is not negotiable in this project.

The NVR's camera-facing client speaks the same protocol we do, on the same port, so whatever
it does to stitch a panoramic camera or configure a fisheye it does over Baichuan. That makes
it a better reference than capturing the phone app, and it is where these names came from.

## What a camera says it can do

There is an ability table listing the capabilities a camera advertises. Two entries matter
here, sitting alongside familiar ones like `battery`, `motion` and `autoFocus`:

    fishEye
    binoCfg

That is worth using before anything else. Rather than guessing which models support what,
query the ability list and let the camera answer. A fisheye-only feature attempted against a
camera that does not advertise `fishEye` is a wasted capture session.

## Pano stitching

Messages:

    MSG_CFG_STITCH_GET / MSG_CFG_STITCH_SET
    GET_BINO_STITCH_CFG
    MSG_NETC_BINO_OFFSET_ADJUST
    MSG_NETC_BINO_OFFSET_GET
    MSG_SNAP_BINO_ADJUST_RESULT_GET
    MSG_SNAP_BINO_SCREEN_ADJUST

Parameter name: `BinoStitch`. There is also `BinoAdjust` and a `binoType` / `bino_type`
notion, with a log line about a channel's bino type changing from one value to another, so
the camera has modes rather than a single stitching state.

Four separate serialisers exist, which says the feature has four distinct payloads rather
than one config blob:

    net_bino_stitch_cfg_s2x / x2s      the stored configuration
    nets_bino_adjust_x2s               an adjustment request
    nets_bino_adjust_result_x2s        the result of an adjustment
    nets_bino_linewidth_x2s            a line width, presumably the seam

Storage keys the NVR reads and writes, which describe what the configuration consists of:

    CamCenterDiff
    CamWidthDiff
    CamHeightDiff
    CamStitchPara      (a blob, there is a length check against a maximum)
    CamEcsPara         (another blob)

Note these are keys in the camera's parameter store, reached over Baichuan, not XML element
names. The XML element names still have to come from a capture. But knowing the
configuration is centre, width and height differences plus two opaque blobs means a capture
can be read rather than guessed at.

The interesting part is `MSG_NETC_BINO_OFFSET_ADJUST` together with
`MSG_SNAP_BINO_ADJUST_RESULT_GET`. That is not a stored setting being written: it is a
request to re-align the lenses and a snapshot-based readback of how it went. A camera that
can be told to re-align itself and report the outcome is a different proposition from one
whose alignment you nudge by hand through a web UI.

## Fisheye

Messages:

    MSG_CFG_FISH_EYE_GET / MSG_CFG_FISH_EYE_SET
    GET_FISH_EYE_CFG / SET_FISH_EYE_CFG
    MSG_CFG_FISH_EYE_SUBCHN_CTRL
    FISH_EYE_SUBCHN_CTRL_V20

Parameters: `fishEyeCfg` and `fishEyeSubChnCtrl`. Serialisers exist for both, including
`nets_param_fish_eye_subchn_x2s`, so the sub-channel control is its own payload rather than a
field inside the main config.

**Sub-channel control is the part worth chasing.** A dedicated control message for
sub-channels strongly suggests the camera can emit dewarped views as additional channels,
rather than a client cropping regions out of the raw fisheye image afterwards. If that is
what it means, then any setup currently producing dewarped tiles with a crop filter and a GPU
transcode is doing work the camera will do itself.

The field names inside `fishEyeCfg` are still unknown. They are built by
`net_fish_eye_cfg_s2x` from string literals in that function's literal pool, and reading them
out needs an ARM disassembler, which is not installed here. The strings do not appear
standalone in the binary.

## Suggested order

1. Query the ability list on a fisheye camera and a panoramic camera. Confirm `fishEye` and
   `binoCfg` are advertised, and see what else is. Cheap, needs no capture, and it validates
   the whole approach before any effort goes into it.
2. Capture `GET_FISH_EYE_CFG` against the fisheye camera. Read the XML. That single capture
   settles the field names that firmware strings do not give up.
3. Capture `MSG_CFG_FISH_EYE_SUBCHN_CTRL` while changing a view in the Reolink app, which is
   what reveals whether sub-channels are what they appear to be.
4. Stitching after that. It is the more interesting capability but the less useful one: a
   pano that is already aligned does not need realigning, whereas dewarped sub-channels would
   remove real work from the pipeline every day.

## Tooling note

An ARM disassembler would make step 2 unnecessary for the field names, since the s2x
functions build the XML directly. `libnetpublic.so` is ARM 32-bit and unstripped, with all
these symbols present and addresses known. The objdump available here has no ARM support.
Even with one, the rule stands: use it to know what to look for, then confirm on the wire and
implement from the capture.
