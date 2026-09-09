package baichuan

// Message IDs recovered from the dispatch table in the NVR's camera-facing
// Baichuan client (app/netclient, records of [id:4][handler:4][name:32]).
// The names are the firmware's own descriptions, verbatim.
//
// This table is authoritative for what it contains and is not exhaustive:
// the camera dispatches with a switch rather than a table, so ids it accepts
// but the NVR never sends do not appear here. Ping (93) is one such id.
//
// Generated; see docs/msgids.json.

const (
	MsgIDHeartbeat                   = 0   // heartbeat
	MsgIDLogout                      = 2   // logout
	MsgIDPreviewStop                 = 4   // preview stop
	MsgIDReplayStart                 = 5   // replay start
	MsgIDGetSysDataTime              = 6   // get sys data time
	MsgIDReplayStart2                = 7   // replay start
	MsgIDGetPresets                  = 8   // get presets
	MsgIDGetDns                      = 9   // get dns
	MsgIDTalkClose                   = 11  // talk close
	MsgIDLogin2                      = 12  // login
	MsgIDSetImagingSettings          = 13  // set imaging settings
	MsgIDFindFileInfo                = 14  // find file info
	MsgIDFindFileInfoNext            = 15  // find file info next
	MsgIDFindFileClose               = 16  // find file close
	MsgIDSetVideoEncoderCfg          = 17  // set video encoder cfg
	MsgIDPtzControl                  = 18  // ptz control
	MsgIDPtzPreset                   = 19  // ptz preset
	MsgIDPtzCruise                   = 20  // ptz cruise
	MsgIDGetEventProperties          = 21  // get event properties
	MsgIDCreatePullPointSubscription = 22  // create pull point subscription
	MsgIDReboot                      = 23  // reboot
	MsgIDUnsubscribe                 = 24  // unsubscribe
	MsgIDIspSet                      = 25  // isp set
	MsgIDIspGet                      = 26  // isp get
	MsgIDLogin3                      = 27  // login
	MsgIDLogout2                     = 28  // logout
	MsgIDGetOsd                      = 29  // get osd
	MsgIDSetOsd                      = 30  // set osd
	MsgIDGetSyscpuload               = 31  // get syscpuload
	MsgIDGetEncbitrate               = 32  // get encbitrate
	MsgIDAlarmReport                 = 33  // alarm report
	MsgIDGetSysversion               = 34  // get sysversion
	MsgIDEmailCfgGet                 = 42  // email cfg get
	MsgIDEmailCfgSet                 = 43  // email cfg set
	MsgIDOsdGet                      = 44  // osd get
	MsgIDOsdSet                      = 45  // osd set
	MsgIDMdGet                       = 46  // md get
	MsgIDMdSet                       = 47  // md set
	MsgIDShelterGet                  = 52  // shelter get
	MsgIDShelterSet                  = 53  // shelter set
	MsgIDRecCfgGet                   = 54  // rec cfg get
	MsgIDRecCfgSet                   = 55  // rec cfg set
	MsgIDGetEnc                      = 56  // get enc
	MsgIDSetEnc                      = 57  // set enc
	MsgIDGetUserCfg                  = 58  // get user cfg
	MsgIDSetUserCfg                  = 59  // Set user cfg
	MsgIDPtzCruise2                  = 64  // ptz cruise
	MsgIDUpdateDev                   = 67  // update_dev
	MsgIDFtpCfgGet                   = 68  // ftp cfg get
	MsgIDFtpCfgSet                   = 69  // ftp cfg set
	MsgIDFtpTaskGet                  = 70  // ftp task get
	MsgIDFtpTaskSet                  = 71  // ftp task set
	MsgIDGetLocalLink                = 76  // get local link
	MsgIDIspBase                     = 78  // isp base
	MsgIDPtzParam                    = 79  // ptz param
	MsgIDVersionInfo                 = 80  // version info
	MsgIDRecTaskGet                  = 81  // rec task get
	MsgIDRecTaskSet                  = 82  // rec task set
	MsgIDRestore                     = 99  // restore
	MsgIDAutoRebootSet               = 100 // auto reboot set
	MsgIDSdcardGet                   = 102 // sdcard get
	MsgIDSdcardFormat                = 103 // sdcard format
	MsgIDGetGeneral                  = 104 // get general
	MsgIDSetGeneral                  = 105 // set general
	MsgIDDstGet                      = 106 // dst get
	MsgIDDstSet                      = 107 // dst set
	MsgIDOsdDefGet                   = 110 // osd def get
	MsgIDShelterDef                  = 111 // shelter def
	MsgIDEncDefGet                   = 112 // enc def get
	MsgIDMdDefGet                    = 113 // md def get
	MsgIDGetUidCfg                   = 114 // get uid cfg
	MsgIDWifiInfoGet                 = 116 // wifi info get
	MsgIDWifiInfoSet                 = 117 // wifi info set
	MsgIDPerformanceInfo             = 122 // performance info
	MsgIDReplaySeek                  = 123 // replay seek
	MsgIDIspDef                      = 132 // isp def
	MsgIDDownloadCut                 = 143 // download cut
	MsgIDIframeRequest               = 189 // iframe request
	MsgIDPtzPreset2                  = 190 // ptz preset
	MsgIDGetAutoUpdate               = 195 // get auto update
	MsgIDSetAutoUpdate               = 196 // set auto update
	MsgIDWifiSignalScan              = 198 // wifi signal scan
	MsgIDWifiTest                    = 200 // wifi test
	MsgIDLedGet                      = 208 // led get
	MsgIDLedSet                      = 209 // led set
	MsgIDRfCfgGet                    = 212 // rf cfg get
	MsgIDRfCfgSet                    = 213 // rf cfg set
	MsgIDEmailTaskSet                = 216 // email task set
	MsgIDEmailTaskGet                = 217 // email task get
	MsgIDPushTaskSet                 = 218 // push task set
	MsgIDPushTaskGet                 = 219 // push task get
	MsgIDAfLearning                  = 222 // af learning
	MsgIDGetAutoFocusCfg             = 224 // get auto focus cfg
	MsgIDSetAutoFocusCfg             = 225 // set auto focus cfg
	MsgIDCropGet                     = 228 // crop get
	MsgIDCropSet                     = 229 // crop set
	MsgIDAudioTaskSet                = 231 // audio task set
	MsgIDAudioTaskGet                = 232 // audio task get
	MsgIDSetdeviceSleep              = 233 // setdevice sleep
	MsgIDBatteryInfoReport           = 252 // battery info report
	MsgIDBatteryInfoGet              = 253 // battery info get
	MsgIDGet4gModuleInfo             = 257 // get 4g module info
	MsgIDAudioFileInfoGet            = 260 // audio file info get
	MsgIDImportAudio                 = 261 // import audio
	MsgIDSaveAudio                   = 262 // save audio
	MsgIDPlayStopAudio               = 263 // play stop audio
	MsgIDAudioCfgGet                 = 264 // audio cfg get
	MsgIDAudioCfgSet                 = 265 // audio cfg set
	MsgIDAudioAlarmMute              = 266 // audio alarm mute
	MsgIDStartAlarmVideo             = 272 // start alarm video
	MsgIDFindAlarmVideo              = 273 // find alarm video
	MsgIDStopAlarmVideo              = 274 // stop alarm video
	MsgIDAgingTestDataGet            = 285 // aging test data get
	MsgIDAgingTestStart              = 286 // aging test start
	MsgIDFloodlightSet               = 288 // floodlight set
	MsgIDFloodlightTaskGet           = 289 // floodlight task get
	MsgIDFloodlightTaskSet           = 290 // floodlight task set
	MsgIDFloodlightReport            = 291 // floodlight report
	MsgIDZoomFocusGet                = 294 // zoom focus get
	MsgIDZoomFocusStart              = 295 // zoom focus start
	MsgIDThresholdGet                = 296 // threshold get
	MsgIDThresholdSet                = 297 // threshold set
	MsgIDCoverPreview                = 298 // cover preview
	MsgIDAiCfgGet                    = 299 // ai cfg get
	MsgIDAiCfgSet                    = 300 // ai cfg set
	MsgIDGetTimelapseCfg             = 319 // get timelapse cfg
	MsgIDSetTimelapseCfg             = 320 // set timelapse cfg
	MsgIDPtzGuard                    = 331 // ptz guard
	MsgIDPtzGuard2                   = 332 // ptz guard
	MsgIDPtzAutoTest                 = 340 // ptz auto test
	MsgIDPtzAutoTest2                = 341 // ptz auto test
	MsgIDAiDetectCfgGet              = 342 // ai detect cfg get
	MsgIDAiDetectCfgSet              = 343 // ai detect cfg set
	MsgIDAiDefaultDetectCfgGet       = 344 // ai default detect cfg get
	MsgIDGetAudioFileInfoList        = 347 // get audio file info list
	MsgIDDeleteAutoFile              = 348 // delete auto file
	MsgIDPlayAudioFile               = 349 // play audio file
	MsgIDStopAutoFile                = 350 // stop auto file
	MsgIDAfLearningResGet            = 355 // af learning res get
	MsgIDSdCardTest                  = 356 // sd card test
	MsgIDBinoAdjust                  = 359 // bino adjust
	MsgIDAdjustResultGet             = 360 // adjust result get
	MsgIDLinewidthGet                = 361 // linewidth get
	MsgIDZfBacklashGet               = 365 // zf backlash get
	MsgIDZfBacklashSet               = 366 // zf backlash set
	MsgIDSetAudioFileInfoList        = 368 // set audio file info list
	MsgIDFtyResultGet                = 369 // fty result get
	MsgIDFtyPeripheralStatSet        = 370 // fty peripheral stat set
	MsgIDFtyIrCutInfo                = 371 // fty ir_cut info
	MsgIDRelayHeader                 = 381 // relay header
	MsgIDRelayv2Stop                 = 382 // relayv2 stop
	MsgIDRelayv2Seek                 = 383 // relayv2 seek
	MsgIDFtyRangeGet                 = 384 // fty range get
	MsgIDFtyRangeGet2                = 385 // fty range get
	MsgIDOffsetAdjust                = 389 // offset adjust
	MsgIDOffsetAdjustResultGet       = 390 // offset adjust result get
	MsgIDSetIotAction                = 396 // set iot action
	MsgIDSmtCmdProc                  = 400 // smt cmd proc
	MsgIDExportAudio                 = 410 // export_audio
	MsgIDBandwidthTest               = 413 // bandwidth_test
	MsgIDGetBinoSttichCfg            = 417 // get bino sttich cfg
	MsgIDSetBinoSttichCfg            = 418 // set bino sttich cfg
	MsgIDFtyPeripheralStatGet        = 419 // fty peripheral stat get
	MsgIDImportImage                 = 420 // import image
	MsgIDExportImage                 = 421 // export image
	MsgIDOptocouplerCalib            = 426 // optocoupler calib
	MsgIDGetAutoReply                = 427 // get auto reply
	MsgIDSetAutoReply                = 428 // set auto reply
	MsgIDFtyDoorbellTest             = 432 // fty doorbell test
	MsgIDGetPtzCurPos                = 433 // get ptz cur_pos
	MsgIDGetAiTrackLimitCfg          = 434 // get ai track limit cfg
	MsgIDSetAiTrackLimitCfg          = 435 // set ai track limit cfg
	MsgIDGetAiTrackTaskCfg           = 436 // get ai track task cfg
	MsgIDSetAiTrackTaskCfg           = 437 // set ai track task cfg
	MsgIDGetAiDenoiseCfg             = 439 // get ai denoise cfg
	MsgIDSetAiDenoiseCfg             = 440 // set ai denoise cfg
	MsgIDGetFishEyeCfg               = 443 // get fish eye cfg
	MsgIDSetFishEyeCfg               = 444 // set fish eye cfg
	MsgIDSet3dLocation               = 445 // set 3d location
	MsgIDGetLargeBattery             = 449 // get large battery
	MsgIDSetLargeBattery             = 450 // set large battery
	MsgIDGetPowerMode                = 451 // get power mode
	MsgIDSetPowerMode                = 452 // set power mode
	MsgIDGetAfAlgCfg                 = 453 // get af alg cfg
	MsgIDSetAfAlgCfg                 = 454 // set af alg cfg
	MsgIDDingdongCtrl                = 483 // dingdong ctrl
	MsgIDDingdongPairedList          = 484 // dingdong paired list
	MsgIDDingdongDevOpt              = 485 // dingdong dev opt
	MsgIDGetDingdongCfg              = 486 // get dingdong cfg
	MsgIDSetDingdongCfg              = 487 // set dingdong cfg
	MsgIDGetHomebaseWakeupTask       = 488 // get homebase wakeup task
	MsgIDSetHomebaseWakeupTask       = 489 // set homebase wakeup task
	MsgIDDingdongScanlistReport      = 490 // dingdong scanlist report
	MsgIDGetAccessUsercfg            = 511 // get access usercfg
	MsgIDSetAccessUsercfg            = 512 // set access usercfg
	MsgIDFskRfPairReport             = 514 // fsk rf pair report
	MsgIDFskRfPairSet                = 515 // fsk rf pair set
	MsgIDGetCrosslineDetectCfg       = 527 // get crossline detect cfg
	MsgIDSetCrosslineDetectCfg       = 528 // set crossline detect cfg
	MsgIDGetIntrusionDetectCfg       = 529 // get intrusion detect cfg
	MsgIDSetIntrusionDetectCfg       = 530 // set intrusion detect cfg
	MsgIDGetLoiteringDetectCfg       = 531 // get loitering detect cfg
	MsgIDSetLoiteringDetectCfg       = 532 // set loitering detect cfg
	MsgIDWifiSdbInfoGet              = 537 // wifi sdb info get
	MsgIDWifiSdbInfoSet              = 538 // wifi sdb info set
	MsgIDFishEyeSubchnCtrl           = 541 // fish eye subchn ctrl
	MsgIDSirenStatusReport           = 547 // siren status report
	MsgIDSetPreviewStatSuccess       = 548 // set preview stat success
	MsgIDGetLegacyDetectCfg          = 549 // get legacy detect cfg
	MsgIDSetLegacyDetectCfg          = 550 // set legacy detect cfg
	MsgIDGetLossDetectCfg            = 551 // get loss detect cfg
	MsgIDSetLossDetectCfg            = 552 // set loss detect cfg
	MsgIDGetWifiRetcode              = 553 // get wifi retcode
	MsgIDRptStationListGet           = 563 // rpt station list get
	MsgIDGetSleepStateCfg            = 574 // get sleep state cfg
	MsgIDSetSleepStateCfg            = 575 // set sleep state cfg
	MsgIDSnapForSession              = 578 // snap for session
	MsgIDSnapTlps                    = 579 // snap tlps
	MsgIDCfgModifyReport             = 580 // cfg modify report
	MsgIDRestartMesh                 = 589 // restart mesh
	MsgIDDelayRecStatReport          = 590 // delay rec stat report
	MsgIDGetLongrunCfg               = 594 // get longrun cfg
	MsgIDSetLongrunCfg               = 595 // set longrun cfg
	MsgIDGetSilentMode               = 609 // get silent mode
	MsgIDSetSilentMode               = 610 // set silent mode
	MsgIDBatFlagReport               = 622 // bat flag report
	MsgIDSleepStatusReport           = 623 // sleep status report
	MsgIDGetBatteryMode              = 626 // get battery mode
	MsgIDSetBatteryMode              = 627 // set battery mode
	MsgIDAovInfoReport               = 687 // aov info report
	MsgIDGetPirMotionDetectCfg       = 694 // get pir motion detect cfg
	MsgIDSetPirMotionDetectCfg       = 695 // set pir motion detect cfg
	MsgIDForwardProxyMsg             = 705 // forward proxy msg
	MsgIDCoordinatePointReport       = 723 // coordinate point report
	MsgIDSetCoordinatePoint          = 724 // set coordinate point
	MsgIDGetPriSign                  = 729 // get pri sign
	MsgIDGetTamperAlarmCfg           = 763 // get tamper alarm cfg
	MsgIDSetTamperAlarmCfg           = 764 // set tamper alarm cfg
)

// MsgName returns the firmware's description for a message id.
func MsgName(id uint32) string { return msgNames[id] }

var msgNames = map[uint32]string{
	0:   "heartbeat",
	1:   "login",
	2:   "logout",
	3:   "preview start",
	4:   "preview stop",
	5:   "replay start",
	6:   "get sys data time",
	7:   "replay start",
	8:   "get presets",
	9:   "get dns",
	10:  "talk ability",
	11:  "talk close",
	12:  "login",
	13:  "set imaging settings",
	14:  "find file info",
	15:  "find file info next",
	16:  "find file close",
	17:  "set video encoder cfg",
	18:  "ptz control",
	19:  "ptz preset",
	20:  "ptz cruise",
	21:  "get event properties",
	22:  "create pull point subscription",
	23:  "reboot",
	24:  "unsubscribe",
	25:  "isp set",
	26:  "isp get",
	27:  "login",
	28:  "logout",
	29:  "get osd",
	30:  "set osd",
	31:  "get syscpuload",
	32:  "get encbitrate",
	33:  "alarm report",
	34:  "get sysversion",
	42:  "email cfg get",
	43:  "email cfg set",
	44:  "osd get",
	45:  "osd set",
	46:  "md get",
	47:  "md set",
	52:  "shelter get",
	53:  "shelter set",
	54:  "rec cfg get",
	55:  "rec cfg set",
	56:  "get enc",
	57:  "set enc",
	58:  "get user cfg",
	59:  "Set user cfg",
	64:  "ptz cruise",
	67:  "update_dev",
	68:  "ftp cfg get",
	69:  "ftp cfg set",
	70:  "ftp task get",
	71:  "ftp task set",
	76:  "get local link",
	78:  "isp base",
	79:  "ptz param",
	80:  "version info",
	81:  "rec task get",
	82:  "rec task set",
	99:  "restore",
	100: "auto reboot set",
	102: "sdcard get",
	103: "sdcard format",
	104: "get general",
	105: "set general",
	106: "dst get",
	107: "dst set",
	109: "snap",
	110: "osd def get",
	111: "shelter def",
	112: "enc def get",
	113: "md def get",
	114: "get uid cfg",
	116: "wifi info get",
	117: "wifi info set",
	122: "performance info",
	123: "replay seek",
	132: "isp def",
	143: "download cut",
	151: "get device ability",
	189: "iframe request",
	190: "ptz preset",
	195: "get auto update",
	196: "set auto update",
	198: "wifi signal scan",
	199: "get support",
	200: "wifi test",
	201: "talk open",
	202: "talk fdx stream",
	208: "led get",
	209: "led set",
	212: "rf cfg get",
	213: "rf cfg set",
	216: "email task set",
	217: "email task get",
	218: "push task set",
	219: "push task get",
	222: "af learning",
	224: "get auto focus cfg",
	225: "set auto focus cfg",
	228: "crop get",
	229: "crop set",
	231: "audio task set",
	232: "audio task get",
	233: "setdevice sleep",
	252: "battery info report",
	253: "battery info get",
	257: "get 4g module info",
	260: "audio file info get",
	261: "import audio",
	262: "save audio",
	263: "play stop audio",
	264: "audio cfg get",
	265: "audio cfg set",
	266: "audio alarm mute",
	272: "start alarm video",
	273: "find alarm video",
	274: "stop alarm video",
	285: "aging test data get",
	286: "aging test start",
	288: "floodlight set",
	289: "floodlight task get",
	290: "floodlight task set",
	291: "floodlight report",
	294: "zoom focus get",
	295: "zoom focus start",
	296: "threshold get",
	297: "threshold set",
	298: "cover preview",
	299: "ai cfg get",
	300: "ai cfg set",
	319: "get timelapse cfg",
	320: "set timelapse cfg",
	331: "ptz guard",
	332: "ptz guard",
	340: "ptz auto test",
	341: "ptz auto test",
	342: "ai detect cfg get",
	343: "ai detect cfg set",
	344: "ai default detect cfg get",
	347: "get audio file info list",
	348: "delete auto file",
	349: "play audio file",
	350: "stop auto file",
	355: "af learning res get",
	356: "sd card test",
	359: "bino adjust",
	360: "adjust result get",
	361: "linewidth get",
	365: "zf backlash get",
	366: "zf backlash set",
	368: "set audio file info list",
	369: "fty result get",
	370: "fty peripheral stat set",
	371: "fty ir_cut info",
	381: "relay header",
	382: "relayv2 stop",
	383: "relayv2 seek",
	384: "fty range get",
	385: "fty range get",
	389: "offset adjust",
	390: "offset adjust result get",
	396: "set iot action",
	400: "smt cmd proc",
	410: "export_audio",
	413: "bandwidth_test",
	417: "get bino sttich cfg",
	418: "set bino sttich cfg",
	419: "fty peripheral stat get",
	420: "import image",
	421: "export image",
	426: "optocoupler calib",
	427: "get auto reply",
	428: "set auto reply",
	432: "fty doorbell test",
	433: "get ptz cur_pos",
	434: "get ai track limit cfg",
	435: "set ai track limit cfg",
	436: "get ai track task cfg",
	437: "set ai track task cfg",
	439: "get ai denoise cfg",
	440: "set ai denoise cfg",
	443: "get fish eye cfg",
	444: "set fish eye cfg",
	445: "set 3d location",
	449: "get large battery",
	450: "set large battery",
	451: "get power mode",
	452: "set power mode",
	453: "get af alg cfg",
	454: "set af alg cfg",
	483: "dingdong ctrl",
	484: "dingdong paired list",
	485: "dingdong dev opt",
	486: "get dingdong cfg",
	487: "set dingdong cfg",
	488: "get homebase wakeup task",
	489: "set homebase wakeup task",
	490: "dingdong scanlist report",
	511: "get access usercfg",
	512: "set access usercfg",
	514: "fsk rf pair report",
	515: "fsk rf pair set",
	527: "get crossline detect cfg",
	528: "set crossline detect cfg",
	529: "get intrusion detect cfg",
	530: "set intrusion detect cfg",
	531: "get loitering detect cfg",
	532: "set loitering detect cfg",
	537: "wifi sdb info get",
	538: "wifi sdb info set",
	541: "fish eye subchn ctrl",
	547: "siren status report",
	548: "set preview stat success",
	549: "get legacy detect cfg",
	550: "set legacy detect cfg",
	551: "get loss detect cfg",
	552: "set loss detect cfg",
	553: "get wifi retcode",
	563: "rpt station list get",
	574: "get sleep state cfg",
	575: "set sleep state cfg",
	578: "snap for session",
	579: "snap tlps",
	580: "cfg modify report",
	589: "restart mesh",
	590: "delay rec stat report",
	594: "get longrun cfg",
	595: "set longrun cfg",
	609: "get silent mode",
	610: "set silent mode",
	622: "bat flag report",
	623: "sleep status report",
	626: "get battery mode",
	627: "set battery mode",
	687: "aov info report",
	694: "get pir motion detect cfg",
	695: "set pir motion detect cfg",
	705: "forward proxy msg",
	723: "coordinate point report",
	724: "set coordinate point",
	729: "get pri sign",
	763: "get tamper alarm cfg",
	764: "set tamper alarm cfg",
}
