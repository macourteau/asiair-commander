# ASIAIR hardware notes

**Scope.** Everything Commander knows about the ZWO ASIAIR and its SMB1 server that the rest of
the design relies on: the numbered hardware findings (H1–H13, with the gaps explained in §1.3),
the share layout and on-device folders, how empty and absent removable media look over the wire,
the `scan_air` discovery protocol and the :4700 control channel, the device clock, the FITS
headers the ASIAIR writes, the SMB1 client behaviours the device forces, link speeds and frame
sizes, and the FITS library facts that follow from them. These notes state device behaviour and
its direct consequence; the mechanisms built on it live in the docs linked from each section.
Every identifier, address and SSID below is synthetic;
[`test/FIXTURES.md`](../../test/FIXTURES.md) lists the canonical values.

## 1. About these notes

### 1.1 What the notes are for

The ASIAIR is a closed embedded device. Commander cannot ask it for a checksum, a list of
connected clients, or whether a frame is final, so every safety rule in the design rests on
something observed about how the device behaves. These notes are the single home of those
observations. Other docs cite a finding by its H-number (`H5`) or a section here
("asiair-hardware-notes §4") and state only the consequence for their own mechanism.

### 1.2 How much each fact is trusted

Each fact is one of three kinds, and the text says which:

| Kind | Meaning | Example |
|---|---|---|
| Measured | Observed on a real ASIAIR over SMB1 or UDP, usually more than once | H2, H5, the `scan_air` reply shape |
| Reproduced | Measured, and also reproduced by the integration fixture (`test/integration/samba`, a Samba NT1 server staged like an ASIAIR) so a regression fails a test | guest-session downgrade, TRANS2 FIND fragmentation, non-empty folder delete |
| Inferred | Deduced from logs and byte counts, not instrumented on hardware | H13 |

Live-hardware smoke tests sit behind the `live` build tag (`internal/device/live_test.go`;
architecture §14.5); `TestLiveProbeOpenSharesMount` exercises H1 and H2 on real hardware and
`TestLiveReadFirstFile` the streaming read path. Hermetic tests that pin a finding's
consequence are named beside each finding below.

### 1.3 Numbering

H-numbers are stable identifiers. H3, H6, H8 and H9 have no entry; the gaps are deliberate, and a
number is never reused. H always means a hardware finding: the Prometheus instrumentation hazards
are a separate series, P1–P15, in [architecture §17.6](2026-10-17-architecture.md).

### 1.4 The trust boundary the device imposes

The ASIAIR exposes its storage as guest SMB shares that accept any password (H11) and let a
guest delete anything (H7). `scan_air` and the control channel are unauthenticated JSON over
UDP and TCP. So any host on the same LAN segment can already read, write and delete a rig's
frames directly, and can forge a discovery or control reply. Commander's defences are aimed at
the benign failure modes this device produces (DHCP churn, a swapped or absent stick, a
listing that silently truncates, a frame revised after it was written), not at a same-L2
attacker. The threat model is
[device-identity-and-media §5](2026-10-17-device-identity-and-media.md); every delete guard is
Commander's own ([delete-safety §2.5](2026-10-17-delete-safety.md)).

## 2. Hardware findings

### H1 — the device clock arrives free at negotiate

`Session.ServerTime()` reports the device's clock from the SMB1 NEGOTIATE response, with no extra
round trip. Commander measures `offset = device − host` from it once per session. The offset is
applied only where a device timestamp meets the host's wall clock (`min_file_age`, the cadence
predictor); a device-to-device comparison (decide-to-pull, the pre-delete re-stat) never sees it.
Those compare the raw stored values within ±2 s (H4), because the measured offset jitters from one
session to the next: an offset-corrected compare would read an unchanged file as changed and re-pull
it every cycle ([frame-lifecycle §4](2026-10-17-frame-lifecycle.md)). `internal/clock` makes the two
domains distinct types, so a cross-domain comparison does not compile. Guarded by
`TestForSessionMeasuresOffset`, `TestYoungerThanUnderSkew` and `TestYoungerThanBothClocksWrong`
(`internal/clock`). §7 covers what the clock reads and why it can be years wrong.

### H2 — the share root lists as `""`, not `"."`

A mounted share's root is addressed, and lists, as the empty path; nothing in Commander uses
`"."` for it. Every path in the `device.Share` seam is relative to the share root, which is `""`.
Exercised against hardware by `TestLiveProbeOpenSharesMount` (`live` tag), and against the
fixture by `TestSMBDeviceAgainstRealContainer`.

### H4 — device storage is FAT-family, so mtimes are coarse and zone-sensitive

The eMMC root carries `FSCK*.REC` recovery files, the signature of a FAT-family filesystem.
Two consequences follow. Modification times have 2-second granularity, so every device-side
size-and-mtime comparison uses a ±2 s tolerance (inclusive). And FAT stores local wall-clock
time, so a device time-zone or DST change can shift every listed mtime at once, which can let
a still-growing file pass `min_file_age` alone. That is why quiescence also requires the
size-and-mtime to hold still across polls (`settle_cycles`, on by default at 1;
[frame-lifecycle §4.4](2026-10-17-frame-lifecycle.md)). `FSCK*.REC` files are junk and are
never classified ([frame-lifecycle §3.2](2026-10-17-frame-lifecycle.md)). Guarded by
`TestEqualWithinTolerance`, `TestNewerThanTolerance` (`internal/clock`) and `TestIsJunkFile`
(`internal/filter`).

### H5 — a missing USB stick mounts with an empty root

With no stick in the USB slot, the ASIAIR still exports the `Udisk Images` share, and its root
lists empty. By content alone that is indistinguishable from a stick whose frames have all been
transferred. A watched empty removable slot therefore suppresses "safe to power off" unless
presence is proven another way. The structural presence oracle and the slot rules are
[device-identity-and-media §7](2026-10-17-device-identity-and-media.md); §4 below gives the
exact wire shapes. Guarded by `TestPullRemovableAbsentMediumQuiet`,
`TestMediumPresenceAbsentStickIsAbsent`, `TestMediumPresenceDrainedStickIsPresent` and
`TestSafeToPowerOffDrainedRemovableIsSafe` (`internal/rig`).

### H7 — a guest can delete

The guest session has full delete rights on every storage share. The device enforces nothing,
so every guard on a device delete is Commander's own: the three-way content-hash check
([frame-lifecycle §9.2](2026-10-17-frame-lifecycle.md)), the identity probe and the media
marker. Exercised against the fixture by `TestSMBDeviceAgainstRealContainer`, which deletes as
guest.

### H10 — the removable share reports a synthetic volume serial

`VolumeInfo` on `Udisk Images` returns a constant serial, byte-identical with and without a
stick inserted, so the volume serial cannot detect a swapped or absent stick. `EMMC Images`
reports a real, stable serial, but eMMC is one fixed medium that cannot be swapped. The
removable-media marker file exists because of this finding, and it is scoped to the Udisk
share: nothing characterises the microSD (TF) serial, so TF device deletes are not gated on a
marker path never exercised for that slot. `VolumeInfo` is diagnostic only. The marker is
[device-identity-and-media §6](2026-10-17-device-identity-and-media.md). Guarded by
`TestMediumUsableSwapDetected` and `TestMediaConfirmedForDelete` (`internal/rig`).

### H11 — the guest share accepts any password

The device's Samba server (4.9.5) has no account matching the documented `guest` / `12345678`
login. It answers any NTLMv2 logon by granting a guest session instead of authenticating the
name, whatever password is sent. Two consequences: the SMB1 client must accept the guest
downgrade (smb1client's `AllowGuest`), or every session fails and the rig reads as offline;
and an authentication failure can never mean "the password changed", so no diagnostic is
built on one. Reproduced by the fixture's `map to guest = bad user` and guarded by
`TestServerGrantsGuestLogon` and `TestGuestLogonSessionOpens` (`test/integration`).

### H12 — a USB dropout mid-session fails over to eMMC

When the USB stick disconnects during a capture run, the ASIAIR silently writes the remaining frames
to the internal eMMC, with no clear error in its own log. The failover is partial and quiet: the
frames written before the dropout stay on the stick and the rest land on the eMMC. One session's
frames then split across `Udisk Images` and `EMMC Images`. A USB-primary rig must therefore watch
both shares, and config validation warns when a rig watches Udisk without EMMC. Because filing is by
FITS header, the two halves land in the same night folder, and a frame present on both shares is one
capture filed once (two rows sharing one `dest_path`). [frame-lifecycle
§3.4](2026-10-17-frame-lifecycle.md) carries the consequence. Guarded by
`TestValidateUdiskWithoutEMMCWarning` (`internal/config`) and `TestFramePredictionDedupesH12`
(`internal/rig`).

### H13 — the device revises a light frame in place after writing it

The ASIAIR rewrites a light frame on the device after the file is already complete and readable,
injecting its plate-solve WCS into the FITS header. On an ASI6200MM that overflows the primary
header from two 2880-byte blocks to three, so the file grows by exactly 2880 bytes (122,351,040 to
122,353,920) and every data byte shifts. On a camera whose base header leaves room in its last
block, the size need not change at all; only the bytes and the mtime do. The data unit is untouched,
which is what makes it the exposure's stable identity. The revision can become visible several
minutes after the frame's timestamp (observed from about 180 s to about 250 s, so a pull under a
shorter `min_file_age` can land before it and file such a frame twice), and the device's solve queue
has no bound Commander can observe; there is no control-channel call that says a frame is final.

This finding is **inferred**, not instrumented: the +2880 delta is exactly one header block,
and the affected frames are exactly those the device left without a WCS. It also explains why
some pulled lights carry no WCS (§8.3). The consequences (one surviving file per exposure, the
`data_sha256` discriminator, the 300 s `min_file_age` default) are
[frame-lifecycle §11](2026-10-17-frame-lifecycle.md). Guarded by
`TestDeviceRevisionSupersedesTheArchiveCopy`, `TestSameSizeRevisionIsRecognised`,
`TestShippedMinFileAgeHoldsAFrameForFiveMinutes` (`internal/rig`) and
`TestDeviceRevisionOverRealSMB` (`test/integration`).

## 3. SMB shares and on-device layout

### 3.1 Shares by medium

The ASIAIR exports one SMB1 share per storage medium:

| Share | Medium | Removable | Notes |
|---|---|---|---|
| `EMMC Images` | internal eMMC | no | always present; real volume serial |
| `Udisk Images` | USB stick | yes | exported with the slot empty (H5); synthetic serial (H10) |
| `TF Images` | microSD card | yes | exported with the slot empty, like Udisk (§4.3) |
| `Image` | single legacy share | — | some models and older firmware expose only this |

Share names are fixed by firmware, so name-based slot detection is sound: Udisk and TF are
removable slots, eMMC is fixed ([device-identity-and-media §7.1](2026-10-17-device-identity-and-media.md)).
The set of shares varies by model, and one model may expose all four at once, so Commander
never hardcodes it: setup and adoption
enumerate what the device actually offers and the operator picks which to watch. `IPC$`,
`print$` and every other `$`-suffixed administrative share are filtered out of the listing.

### 3.2 What a share root holds

Each storage share root holds the ASIAIR's own folders:

| Folder | Contents | Commander's handling |
|---|---|---|
| `Autorun/` | science frames from plans and sequences | the default watched root |
| `Live/`, `Plan/` | frames from live and plan modes | optional watched roots |
| `log/` | session logs (§3.5) | the log-capture path, never the frame path |
| `Preview/`, `Video/` | previews and recordings | not watched |
| `batch_stack_tmp/` | live-stack scratch that churns during a session | junk, never listed |
| `System Volume Information/` | filesystem metadata | junk, never listed |

Science frames live under `Autorun/<TYPE>/<TARGET>/`, where `<TYPE>` is `Bias`, `Dark`,
`Flat` or `Light`. Commander still classifies by header, never by folder
([routing-and-filing §1](2026-10-17-routing-and-filing.md)).

### 3.3 The nested layout on a removable stick

On a raw USB stick the ASIAIR nests all of its data under a top-level `ASIAIR/` folder, so the
watched root `Autorun` lives at `ASIAIR/Autorun/`. The eMMC uses the flat layout. Commander
probes each configured root at the share root and, only on a confirmed not-found, under
`ASIAIR/`; the first layout that resolves wins for the whole share for that cycle. Because the
flat layout wins as soon as any configured root lists at the share root, a folder named like a
configured root created at a stick's root would hide `ASIAIR/<root>/` from the harvester. The
device file browser refuses to create one ([device-file-browser §7](2026-10-17-device-file-browser.md)).
Guarded by `TestPullUdiskNestedUnderASIAIR` (`internal/rig`) and
`TestLayoutGuardRefusesFlatRoot` (`internal/devfiles`).

### 3.4 Previews and frame names

Each frame is accompanied by a JPEG preview named `<frame-stem>_thn.jpg`:
`Light_NGC 7000_300.0s_Bin1_20260715-004334_0001.fit` pairs with
`Light_NGC 7000_300.0s_Bin1_20260715-004334_0001_thn.jpg`. Pairing strips the trailing `_thn`
before matching stems. FAT commonly stores extensions in upper case (`.FIT`, `.JPG`), so every
extension match is case-insensitive. Previews are never transferred; they are deleted with
their parent frame or by the orphan-preview sweep
([frame-lifecycle §3.3](2026-10-17-frame-lifecycle.md)). A frame name embeds the actual sensor
temperature and a short sensor name; both are less reliable than the header (§8.2).

### 3.5 Session logs

The `log/` folder holds two logs per session that Commander can capture:
`PHD2_GuideLog_<YYYY-MM-DD>_<HHMMSS>.txt` (header `PHD2 version …, Log version 2.5`) and
`Autorun_Log_<YYYY-MM-DD>_<HHMMSS>.txt`. The Autorun log has a Chinese-language `_CHN` twin
that the default globs would also match, so it is excluded by name. Both are free-form
`YYYY/MM/DD HH:MM:SS <message>` lines with bracketed tags (`[Autorun|Begin]`, `[Guide]`,
`[AutoFocus|End]`) and no structured error field, which is why Commander files them and never
parses them ([architecture §11](2026-10-17-architecture.md)).

### 3.6 Which locations are guest-writable

The firmware writes captures into `ASIAIR/` on a stick, and a guest `Create`, `Open` and
`Remove` there succeed, with either `/` or `\` as the separator. That makes `ASIAIR/` the
reliable home of the removable-media marker
([device-identity-and-media §6.2](2026-10-17-device-identity-and-media.md)). Writability of a
removable share's *root* varies by device and firmware: on some devices guest writes at the
root are refused with Access Denied, on others a guest `Mkdir` at the root succeeds. Commander
therefore never depends on the root being writable, and never assumes it is not.

## 4. Empty and absent removable media

### 4.1 An empty slot and an unmountable stick look the same

Measured on a live ASIAIR (firmware DBS114):

| Slot state | `Udisk Images` share root |
|---|---|
| No stick inserted | empty: only `.` and `..` |
| Stick whose partition table and filesystem headers are zeroed | byte-identical to the empty slot, with the same free-space figures |

The firmware exports the bare mountpoint in both cases, and the mountpoint carries no capture
folders. So the residue shape that would fool the structural presence oracle (an `ASIAIR/` or a
configured root with no medium behind it) does not occur: a share root that lists successfully
but holds neither an `ASIAIR/` folder nor a configured root means the slot is empty
([device-identity-and-media §7.2–§7.3](2026-10-17-device-identity-and-media.md)). The one
shape no oracle can catch is a medium that mounts and lists but whose directory entries are
corrupted away; that case has not been characterised on hardware and is an accepted residual
([delete-safety §8](2026-10-17-delete-safety.md)).

### 4.2 Free space on an absent share is the device's own

With no medium in the slot, `Statfs` on the removable share returns the ASIAIR's own root
filesystem, a small, nearly full partition (about 7 GB total, under 1 GB free) unrelated to any
medium. Published as-is, it would show a bogus free-space line and drag the tightest-share
figure under the disk reserve. An absent share is therefore excluded from capacity
([device-identity-and-media §7.4](2026-10-17-device-identity-and-media.md)). Guarded by
`TestAbsentRemovableShareExcludedFromCapacity` (`internal/rig`).

### 4.3 The microSD slot behaves like the USB slot

An empty TF slot is exported as an empty `TF Images` share whose configured root does not
resolve, exactly like an empty Udisk: the walk fails with a not-found
(`readdir Autorun: file does not exist`). It is a removable slot for the safe-to-power-off
verdict and the capacity math, but not a marker-bearing share (H10). Guarded by
`TestEmptyTFSlotTreatedAsAbsent` and `TestPullRemovableAbsentMediumQuiet` (`internal/rig`).

### 4.4 A stick that drops out mid-session

A stick that disconnects during capture does not leave a residue mountpoint with partial
captures in it: the device redirects capture to the eMMC (H12). Both observations cut against
the residue premise of §4.1.

## 5. The scan_air discovery protocol

### 5.1 The exchange

`scan_air` is the UDP discovery exchange the ZWO app uses. Commander sends one CRLF-terminated
JSON request to `255.255.255.255:4720` and to each up, non-loopback IPv4 interface's directed
broadcast address (so a multi-homed host reaches every attached broadcast domain):

```
{"id":"132","method":"scan_air","name":"asiair-commander"}\r\n
```

Each device answers with one JSON-RPC line. Device A's reply:

```
{"jsonrpc":"2.0","method":"scan_air","result":{"guid":"0a1b2c3d","cpuId":"0011223344556677","ip":"192.168.1.75","is_pi4":true,"model":"ZWO AirPlus-RK3568 (Linux)","ssid":"ASIAIR_0a1b2c3d","connect_lock":false},"code":0,"id":"132"}
```

The fields sit under `result`, not at the top level, and there is no advertised device name.
A reply is dropped when it does not parse, its `method` is not `scan_air`, its `code` is
non-zero, or `result.guid` is empty. Unknown fields are ignored. Replies are collected for a
2 s window, at most 256 datagrams per round. The exchange is IPv4 only.

Guarded by `TestParseScanAirReply` and `TestProbeGUID` (`internal/discovery`), which encode
the observed request and reply formats so a firmware change fails a test rather than a rig.

### 5.2 The reply fields

| Field | Meaning | Use |
|---|---|---|
| `guid` | stable device identity; the ZWO app keys on it too | the primary binding key |
| `cpuId` | 64-bit SoC serial; survives a `guid` change such as a factory reset | secondary binding key |
| `ip` | the device's address on the network the reply came from | the endpoint; the UDP source address is the fallback when it is blank |
| `model` | product string, e.g. `ZWO AirPlus-RK3568 (Linux)` | display only |
| `ssid` | the device's own Wi-Fi name, `ASIAIR_<guid>` by default and renamable (Device B: `MyRig`) | display; it is the one field an owner sets and recognises |
| `is_pi4`, `connect_lock` | ignored | kept in the raw reply for diagnosis |

A Raspberry Pi CM4-based ASIAIR (Device B, guid `5eed0001`, model `Raspberry Pi Compute Module 4 Rev
1.0`) reports an **empty `cpuId`**. So a cpuId match counts only when both sides are non-empty; two
empty cpuIds never match. Several devices of one generation report a byte-identical `model` string,
which is why a device's label leads with its SSID when it reports one rather than relying on the
model plus a short guid. `result.ip` has equalled the datagram's source address in every reply
observed on a LAN (44 of 44); replies through the ASIAIR's own hotspot have not been tested.

### 5.3 Unicast probe

The device answers a directed unicast `scan_air` to `<ip>:4720` with the same `guid` as its
broadcast reply, every time. That makes the unicast probe the live-identity primitive: it ties a
guid to the exact device at an address while an SMB session to that address is open
([device-identity-and-media §4](2026-10-17-device-identity-and-media.md)). The probe's budget is 3 s
on the local network, the first datagram re-sent after 1 s and each later wait doubled. The reply
was unchanged across a firmware update (25 of 25, same guid and cpuId), and it comes back unchanged
through a remote site's forwarded UDP flow ([remote-sites §6](2026-10-17-remote-sites.md)). A
powered-off device answers neither the probe nor SMB: a TCP dial to it made by a remote is refused
within about 5 s, and both recover once it boots.

### 5.4 Sockets, firewalls and containers

Replies come back from `device:4720` to the sender's ephemeral port, so the broadcast uses one
**unconnected** UDP socket (`ListenUDP` on `0.0.0.0:0` with `SO_BROADCAST`); a connected
socket would filter those replies out. On Windows the installer adds an inbound,
program-scoped firewall rule so the replies reach that socket. A broadcast cannot cross
Docker's bridge NAT, so discovery from a container needs host networking on a Linux host on the
rigs' L2 segment; SMB itself works under any network mode with a route to the rig.

## 6. The :4700 control channel

### 6.1 Ports

The device answers on several TCP ports, and they do not agree with each other:

| Port | Role |
|---|---|
| 4400 | guider; `get_app_state` here returns a bare string |
| 4500 | unused by Commander |
| 4700 | imager; serves `pi_get_info`, `get_app_state` (the full object), `pi_get_time`, `pi_set_time` |
| 4800, 4801, 4900 | unused by Commander |

Commander speaks only to 4700, and only to serve the device clock
([features §6.3](2026-10-17-features.md)).

### 6.2 Framing

The channel is line-delimited JSON-RPC over one TCP connection. Requests are CRLF-terminated;
Commander numbers them from 200 upwards. The device pushes unsolicited event lines
(`{"Event":"Version",...}` on connect, then `PiStatus` and `Station` periodically), so the
next line to arrive is often not the answer: a reply is matched by its `id`, and everything
else is discarded. Ids come back as numbers on this channel (the `scan_air` reply quotes
them), so a quoted number is accepted too.

The wire formats in §6.2–§6.4 are quoted from the device rather than from any vendor
documentation, and the `internal/control` tests encode the same formats so a firmware change
fails a test rather than a rig: `TestCallMatchesReplyByID` and
`TestRequestIDsStartAt200AndIncrement` (framing), `TestCallReportsDeviceErrorCode` (§6.3),
`TestAppStateVerdict`, `TestGetTime` and `TestSetTimeSendsTheDeviceParamShape` (§6.4).

### 6.3 Errors

An error is a non-zero top-level `code` plus a string `error` field, not JSON-RPC's standard
error object:

```
{"jsonrpc":"2.0","method":"get_controls","error":"device not connected","code":318,"id":230}
```

A zero `code` is success.

### 6.4 Methods Commander uses

`pi_get_info` returns the same identity the `scan_air` reply carries, so a write can be
guarded by an identity check on the connection that carries it:

```
{"jsonrpc":"2.0","method":"pi_get_info","result":{"model":"ZWO AirPlus-RK3568 (Linux)","guid":"0a1b2c3d","cpuId":"0011223344556677", ...},"code":0,"id":201}
```

`get_app_state` returns one object per subsystem:

```json
{"page":"preview",
 "capture":{"exposure_mode":"single","is_working":false,"state":"idle"},
 "stack":{"is_working":false, ...}, "solve":{"is_working":false, ...},
 "auto_focus":{"is_working":false, ...}, "pa":{"is_working":false},
 "auto_goto":{"is_working":false}, "merid_flip":{"is_working":false},
 "avi_record":{"is_working":false, ...}, "format_drive":{"is_working":false}, ...}
```

The device is busy when **any** top-level object carries `"is_working": true`, or when
`capture.state` is anything but `"idle"`. The rule is generic on purpose, so firmware that adds
a subsystem is respected without a code change; scalar entries such as `page` are skipped.

`pi_get_time` returns the wall clock in whole seconds plus the device's IANA zone, and
`pi_set_time` takes the same fields as a one-element array:

```
{"id":202,"method":"pi_set_time","params":[{"time_zone":"America/Toronto","year":2026,"mon":7,"day":27,"hour":16,"min":48,"sec":4}]}
{"jsonrpc":"2.0","method":"pi_set_time","result":0,"code":0,"id":202}
```

The fields carry no sub-second part, so a correction leaves about 1 s of residual drift, and
any sync threshold below about 2 s would write on every cycle.

### 6.5 What the channel does not offer

No method reports connected clients. `connect_lock` (also in the `scan_air` reply) is a stored
user preference read through `get_setting`, the pairing lock, not a live-connection indicator.
No method reports whether a frame is final (H13). Elapsed time since the last frame proves
nothing about idleness: sub-exposures run from seconds to an hour, with filter changes and
autofocus between them. So `get_app_state` is the only idleness signal there is.

## 7. Clock behaviour

### 7.1 No real-time clock

An ASIAIR has no real-time clock and no NTP client. It boots to the date its firmware image
shipped with (2019-02-14 on an AirPlus) and keeps it until the phone app connects. The app's
first write after connecting, ahead of any camera or mount query, is `pi_set_time` with the
phone's time and zone. Stepping the clock on connect is the vendor's own designed behaviour.

### 7.2 What that means during capture

Imaging requires the app, and the app sets the clock on connecting, so a clock years wrong
almost never coincides with a capture. A device more than a few minutes out is one no app has
talked to since it booted. The residual case is an app that connected carrying its own wrong
time. Commander measures the offset every session (H1) and, when a rig opts in, corrects it
over the control channel while the device is idle
([features §6](2026-10-17-features.md)). That correction is cheap insurance, not a load-bearing
mechanism: no safety rule depends on the device clock being right, because the offset applies
only where device time meets host time, and device-to-device comparisons never see it.

### 7.3 Timestamps that inherit the device clock

- **`DATE-OBS`** is written from the device clock, in UTC. Frames captured while the clock was
  wrong keep a wrong `DATE-OBS`; Commander does not repair them.
- **File mtimes** are FAT local time with 2 s granularity (H4). Correcting the clock does not
  rewrite them. A device that was behind leaves frames with old mtimes, which are instantly
  past `min_file_age`; a device that was ahead leaves future-dated mtimes, which delay
  `min_file_age` by at most the size of the correction and heal on their own.
- **Folder mtimes** that a Commander write touches (a created folder, a folder gaining an
  entry) take the device's date. Folder mtimes are only displayed. File mtimes a copy sets
  explicitly are written with `Chtimes` (§9.7).

### 7.4 The zone is the device's

`pi_get_time` names an IANA zone. A correction expresses the current time in that zone, so a rig
set for a remote site's zone is never dragged into the Commander host's. Resolving the name
needs a tz database on every platform, so the binary embeds one (`time/tzdata`); a zone the
embedded database does not know is never guessed at.

## 8. FITS headers the ASIAIR writes

### 8.1 Keywords the design depends on

Confirmed on real ASIAIR light, flat, dark and bias frames (ASIAIR Plus and ASIAIR Mini):

| Keyword | Example | Notes |
|---|---|---|
| `CREATOR` | `'ZWO ASIAIR Plus'`, `'ZWO ASIAIR Mini'` | identifies the producer |
| `IMAGETYP` | `'Light   '`, `'Flat    '` | title case and FITS-padded: `Light`, `Dark`, `Flat`, `Bias` |
| `OBJECT` | `'NGC 7000'`, `'C-2025 A6'` | the target as typed; may carry internal spaces |
| `EXPTIME`, `EXPOSURE` | `600.` | both present; Commander reads `EXPTIME` |
| `SET-TEMP` | `-10` | the cooler set-point, an integer |
| `CCD-TEMP` | `-9.89999961853027` | the actual sensor temperature, a float |
| `INSTRUME` | `'ZWO ASI2600MM Pro'` | the camera model |
| `FILTER` | `'HA      '` | absent on a one-shot-colour camera |
| `BAYERPAT` | `'RGGB    '` | present on a one-shot-colour camera |
| `DATE-OBS` | `'2025-10-11T05:25:41.531614'` | UTC, microseconds, no `Z` suffix |
| `GAIN`, `OFFSET` | `100`, `50` | integers |
| `XBINNING`, `YBINNING` | `1` | |
| `TELESCOP` | `'ZWO AM5N'`, `'EQMod Mount'` | the **mount**, not the telescope |
| `FOCALLEN`, `XPIXSZ` | `1052`, `3.75999999046326` | focal length (mm) and pixel size (µm, with binning) |
| `RA`, `DEC` | `45.96408`, `64.889922` | the pointing, in degrees |
| `NAXIS1`, `NAXIS2` | `6248`, `4176` | image dimensions; `BITPIX` 16 |

### 8.2 Gotchas

1. **`IMAGETYP` casing and padding vary by producer.** The ASIAIR writes `'Light   '`; other
   capture software writes `'LIGHT'` or `'Dark Frame'`. Values are trimmed and matched
   case-insensitively through the alias table, which ships seeded with the four ASIAIR values
   plus `dark frame` and `bias frame`
   ([routing-and-filing §1.1](2026-10-17-routing-and-filing.md)).
2. **Group temperature by `SET-TEMP`, never `CCD-TEMP`.** A cooler that undershoots its
   set-point on a warm night reports a `CCD-TEMP` several degrees off; grouping by it would
   scatter matching darks and lights across near-but-unequal values.
3. **`DATE-OBS` is UTC with no zone marker.** It is parsed as UTC (a trailing `Z` is stripped
   if present) and converted to the site's local zone before night bucketing.
4. **The filename is less reliable than the header.** It embeds the actual temperature and a
   short sensor name.
5. **`TELESCOP` holds the mount on ASIAIR frames**, so the OTA cannot be read from the frame
   and is a rig-profile setting. Other capture software may put the real OTA there.
6. **A one-shot-colour camera writes no `FILTER`**, so a template segment built on `<FILTER>`
   collapses rather than failing ([routing-and-filing §3.4](2026-10-17-routing-and-filing.md)).

### 8.3 The embedded WCS, and when it appears

The ASIAIR plate-solves only periodically on the mount. A light it solved carries a full TAN-SIP
WCS (`CTYPE1 = 'RA---TAN-SIP'`, `CRVAL1/2`, `CRPIX1/2`, the CD matrix); others carry none,
though they are ordinary sub-exposures. The WCS is injected after the frame is written (H13),
so whether a pulled light carries one depends both on whether the device solved it and on
whether the pull landed after the injection. The WCS cards can push the header into a further
2880-byte block: the solved light fixtures carry three-block headers, and an unsolved
ASI6200MM light grows from two blocks to three when solved (H13). Commander reads the embedded
WCS when it is there ([features §2](2026-10-17-features.md)) and plate-solves the rest itself,
seeded by the `RA` and `DEC` cards ([features §3](2026-10-17-features.md)).

### 8.4 Header layout

A FITS file is a primary header of whole 2880-byte blocks of 80-column cards, terminated by an
`END` card, followed by the data unit, padded to a whole number of 2880-byte blocks. The
header alone is a few KB, which is why classification reads only the leading blocks of the
local copy (§11.1). The ASIAIR's in-place revision edits only the header, so the data unit is
the exposure's identity (H13).

## 9. SMB1 client behaviours the device forces

Commander talks to the device through the public module `github.com/macourteau/smb1client`,
wrapped by the `device.Share` seam (`internal/device`). Its error vocabulary does not escape
the seam: callers ask `IsNotFound`, `IsNetwork`, `IsExist`, `IsInUse`, `IsDirNotEmpty` or
`IsPermission`, and a not-found is normalised onto `fs.ErrNotExist`. A dropped or closed
connection is classified through `IsNetwork` (smb1client's `IsNetworkError`), never by matching
the library's internal sentinel.

### 9.1 Sessions and the guest logon

Every session is TCP 445 plus an NTLMv2 logon with the guest credentials, downgraded by the server
to a guest session (H11). The reachability probe is a 3 s TCP dial to 445. Each post-connect
operation is bounded by a per-operation deadline, so a device that goes silent cannot hold a read, a
write or the teardown. Several concurrent sessions from one host work: three at once (two listing,
one writing) ran without error, and two sessions from one source IP, one holding a file open while
the other hashed it, ran with identical digests and no reset, in both orders and with one of the two
sessions tunnelled. smb1client sends `VcNumber` 0, so the server does not tear down a client's other
sessions on a further logon. A session the server drops anyway is a network error and fails closed.

### 9.2 TRANS2 FIND fragmentation

A server answers a TRANS2 FIND with only as many bytes as its own send buffer holds (Samba's
`max_xmit`, about 16.5 KiB) and continues in further messages under the **same MID**, while the
search still reports more to come. A client that reads one message per MID silently returns a
truncated prefix of every batch, in name order, with no error. Full-length ASIAIR capture names
encode to about 250 bytes per entry, so roughly 66 entries fill a message and every 100-entry batch
fragments. smb1client reads every fragment of a TRANS2 reply, guarded by
`TestLargeDirectoryListsCompletely` (400 entries) and `TestRemoteLargeDirectoryListsCompletely`
(`test/integration`). The safety argument and the exact-count rule are [delete-safety
§5](2026-10-17-delete-safety.md).

### 9.3 Sharing violations

A file another open holds (a download, a hash, a lingering search handle after `ReadDir`) fails
a delete with `STATUS_SHARING_VIOLATION` (`0xC0000043`). It is reported as `IsInUse`, which
callers test before `IsPermission` because `errors.Is(err, os.ErrPermission)` is also true for
it. smb1client retries a sharing violation on a **directory** removal up to 10 times with
exponential backoff capped at 1 s per attempt (about 6.5 s in all); a **file** delete is not
retried by the library. The harvester's delete pass retries a failed device delete on a later
pass; the device file browser reports it as `in_use` with a Retry action ([device-file-browser
§5.7, §5.8](2026-10-17-device-file-browser.md)). Guarded by `TestDevfilesDeleteHeldOpenInUse`
(`test/integration`).

### 9.4 Opens do not share delete

smb1client opens a file for reading with `FILE_SHARE_READ | FILE_SHARE_WRITE` and without
`FILE_SHARE_DELETE`. A read in progress therefore blocks a delete of the same file (§9.3),
which is benign: the delete fails and is retried.

### 9.5 Deleting a non-empty folder

A delete-on-close open of a non-empty directory is acknowledged at every step by the device's
Samba yet leaves the directory and its contents in place, so a client using it reports success
for a delete that did not happen. smb1client therefore removes a directory with
`SMB_COM_DELETE_DIRECTORY`, and a non-empty one comes back as `STATUS_DIRECTORY_NOT_EMPTY`
(`0xC0000101`), classified as `IsDirNotEmpty`. The fixture reproduces the false-success
behaviour, so `TestDevfilesDeleteNonEmptyRefused` and `TestDevfilesServiceDeleteNonEmptyRefused`
(`test/integration`) fail on a client that regresses.

### 9.6 Exclusive create, mkdir and rename never replace

| Operation | Onto a free name | Onto an existing name, or a case variant of one |
|---|---|---|
| `CreateNew` (`O_CREATE\|O_EXCL`, `FILE_CREATE`) | works | `STATUS_OBJECT_NAME_COLLISION` (`0xC0000035`), `IsExist`; existing file unchanged |
| `Mkdir` | works | `0xC0000035`, `IsExist` |
| `Rename` | works | `0xC0000035`, `IsExist`; target unchanged, source stays |

The server is case-insensitive, so `REF.FIT` collides with `ref.fit`. Rename never replaces,
which is what lets a copy commit by temp-and-rename without ever destroying an existing file.
Apart from the marker write (`WriteMarkerAt`), the seam has no truncating create. A case-only
rename of one file (`ref.fit` to `REF.FIT`) may be refused as a collision with itself or carried
out, depending on the server; either way exactly one entry survives with its bytes. Guarded by
`TestDevfilesCreateNewExclusive`, `TestDevfilesMkdir`, `TestDevfilesRenameNeverReplaces` and
`TestDevfilesRenameCaseOnly` (`test/integration`).

### 9.7 Setting times

`Chtimes` (a `SET_PATH_INFORMATION` basic-info write) works on the device, and a fresh
session's listing reads the set mtime back exactly. Guarded by `TestDevfilesChtimes`
(`test/integration`).

### 9.8 Read sizes

smb1client takes a pipelined read path for reads of 128 KiB or more and goes sequential below
that; small reads crater throughput silently. Commander streams every frame with
`OpenReader` + `io.CopyBuffer` through a 512 KiB buffer (`header.CopyBufSize`), hashing as
the bytes pass ([frame-lifecycle §5.2](2026-10-17-frame-lifecycle.md)). It never uses the library's
whole-file `ReadFile` for a frame, for two reasons. No read path is trusted to return a whole
file (a silent truncation is the same failure class as §9.2), so the streamed byte count is
always checked against the listing size. And only the streamed form lets the hash be computed over
the same bytes as they pass.

### 9.9 Free space and volume information

`Statfs` takes no path, because SMB1 scopes the query to the mounted tree: it reports the
share's own capacity, which for an absent removable medium is the device's own root
filesystem (§4.2). `VolumeInfo` reports the label and serial, synthetic on Udisk (H10), and is
diagnostic only.

## 10. Network links and frame sizes

### 10.1 Frame sizes

A 16-bit frame's data unit is `NAXIS1 × NAXIS2 × 2` bytes, padded to a whole 2880-byte block:

| Camera (binning 1) | Image | Approximate frame size |
|---|---|---|
| ASI6200MM | 9576 × 6388 | 122 MB |
| ASI294MM | 8288 × 5644 | 94 MB |
| ASI2600MM | 6248 × 4176 | 52 MB |
| ASI294MC | 4144 × 2822 | 23 MB |

Calibration runs are dense: a bias set is many frames in a few minutes. Session logs and
previews are small.

### 10.2 Link speeds

| Path | Typical throughput | Time per 120 MB frame |
|---|---|---|
| ASIAIR Wi-Fi → good access point → wired NAS | about 12 MB/s | about 10 s |
| SMB writes to the device (eMMC, Udisk) | about 5.7–7.4 MiB/s | — |
| Wi-Fi through walls or at range | about 1.2–1.5 MB/s | about 80–100 s |

Spread across a night, reading every frame twice (once to copy, once to re-hash before deleting) is
about 0.4 MB/s for one 120 MB frame roughly every ten minutes, against about 12 MB/s on a good link,
which is why verification has no off switch ([overview §7.2](2026-10-17-overview.md)). On a slow
link the pre-delete device re-hash costs as much link time as the copy, so a large delete backlog
would starve listing and pulling; the delete pass is sliced against a listing-anchored deadline for
that reason ([frame-lifecycle §12](2026-10-17-frame-lifecycle.md)). A partial transfer is never
resumed: at these speeds a re-pull costs seconds to a minute or two.

### 10.3 The device is busy while it captures

The ASIAIR is a small embedded computer running the camera, guiding and dithering at the same time.
Parallel reads from one rig have never been shown to help, and hammering a rig with them is a good
way to upset a capture in progress, so Commander keeps one connection and one file at a time per
rig, and parallelises only across rigs ([architecture §3.4](2026-10-17-architecture.md)).

## 11. FITS library notes

### 11.1 Header reads never use `fitsio.Open`

`github.com/astrogo/fitsio` v0.3.0 (pure Go, BSD-licensed, no cgo) decodes the full data unit
into memory when a file is opened through `fitsio.Open`; there is no lazy, header-only read. A
header-only classification through it would cost a full-frame allocation (about 120 MB) per
frame. `internal/header` therefore reads the leading 2880-byte blocks directly, stopping at
`END` and bounded to 100 blocks so a malformed file cannot walk into its data unit. The header
is read from the local copy after the pull, never from the SMB stream: every frame is
downloaded anyway, and nothing is skipped on header content (previews are excluded by
extension), so reading the header before the pull buys nothing. The digests of the data unit
are teed off bytes already being read, never a second pass
([frame-lifecycle §11.3](2026-10-17-frame-lifecycle.md)).

### 11.2 `fitsio` decodes previews

The preview renderer does need every sample, so it decodes the primary HDU with `fitsio`'s
decoder. `fitsio.Read` returns raw samples, with the `BZERO`/`BSCALE` rescale applied only by a
separate `Image()` method, so the renderer applies `BSCALE · raw + BZERO` itself, exactly once.
`fitsio` is not defensive against a malformed data unit, so the decode runs under a `recover`
and a bad file becomes a preview error rather than a crash
([features §1](2026-10-17-features.md)).

### 11.3 Header-only fixtures

The ASIAIR header fixtures in `internal/header/testdata` (`asiair_*.fit`) are header-only: their
file length equals their header length (2880, 5760 or 8640 bytes), so they carry no data unit and
can never authorise a revision supersede. Tests that need a data unit use synthesised frames, built
by `internal/header/fitstest` or committed as `internal/header/testdata/synth_light_60x48_16bit.fit`
([architecture §14.2](2026-10-17-architecture.md)).
