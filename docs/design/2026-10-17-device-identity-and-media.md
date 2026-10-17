# Device identity, discovery and removable media

**Scope.** How Commander knows which ASIAIR it is talking to, and which medium sits in a
removable slot. It covers scan_air discovery and the guid-keyed registry; binding a rig
profile to a device by guid and cpuId, with address resolution and the last-known endpoint;
the per-session identity probe that every pull, delete, reset and file-browser session
takes; the trusted-LAN threat model those mechanisms are judged against; the removable-media
marker that guards a USB stick; the classification of removable slots and the structural
presence oracle behind the Safe-to-power-off verdict; and the per-share disable control. The
scan_air wire format and the hardware behaviour these rest on are in
[asiair-hardware-notes](2026-10-17-asiair-hardware-notes.md) §5 and §4; the delete gates
these mechanisms feed are enumerated in [delete-safety §3](2026-10-17-delete-safety.md); the
verdict itself is defined in [frame-lifecycle §14](2026-10-17-frame-lifecycle.md).

## 1. Overview

ASIAIR units cannot be given stable network names. On a LAN with several of them the router
hands out `asiair`, `asiair-1`, `asiair-2`… in boot order, and those names and their DHCP
addresses reshuffle on every reboot. A rig profile bound to a hostname or IP can therefore
silently point at a different physical device after a reboot. Commander answers "which
device is this?" with two device-minted identifiers instead:

- **guid**, the stable device identity the ZWO app itself keys on, reported by the scan_air
  UDP discovery protocol; and
- **cpuId**, a 64-bit SoC serial reported by the same protocol, kept as a secondary key.

Identity is used three ways:

1. **Discovery** (§2) broadcasts scan_air and keeps a guid-keyed registry of what answered.
   A discovered device is only offered; it is never touched until the operator adopts it.
2. **Binding and resolution** (§3). An adopted rig stores the device's guid and cpuId. Each
   cycle resolves that identity to the device's current address.
3. **Confirmation** (§4). Every rig session Commander opens to a device (pull, delete, reset
   and file browser) re-proves, with a live unicast scan_air probe taken while the session
   is open, that the device at that address reports the bound guid (or cpuId). No reply
   means refuse; a different identity means refuse and raise a critical alert.

Identity is a property of the device, not of a share or a medium. Whether the USB stick in
the slot is the one Commander adopted is a separate question the probe cannot answer, so a
**media marker** (§6) answers it for the Udisk share, and a **structural presence oracle**
(§7) answers whether a removable slot holds a medium at all.

None of this is the final delete-safety guarantee. Every device delete is authorised only
by the three-way content-hash check ([delete-safety §4](2026-10-17-delete-safety.md),
[frame-lifecycle §9.2](2026-10-17-frame-lifecycle.md)): a wrong device reached by DHCP churn
does not hold a byte-identical file at the same path, so its frames are held, never
deleted. Identity confirmation and the media marker are defence in depth that keep
Commander from pulling a wrong device's frames into the wrong tree, from listing a wrong
device as drained, and from treating an empty slot as a finished drain.

## 2. Discovery

Package `internal/discovery`, type `Scanner`. Discovery runs outside the rig workers
([architecture §3.9](2026-10-17-architecture.md)): workers read the registry, never the
network, when they resolve an address.

### 2.1 scan_air broadcast and the registry

A discovery round sends the scan_air request (`netsite.ScanAirRequest`) as a UDP broadcast
to port 4720 and collects replies for a 2 s window. On the local site
(`netsite` local site, `Broadcast`) one unconnected IPv4 socket sends to
`255.255.255.255:4720` and to the directed-broadcast address of every up, non-loopback IPv4
interface, so a multi-homed NAS reaches every attached segment. The socket is unconnected on
purpose: replies arrive from `device:4720`, which a connected socket would filter out. A
send failure on one interface is logged and does not fail the round. A round runs on every
site that is up, concurrently; a remote site runs its own broadcast at its end
([remote-sites §4](2026-10-17-remote-sites.md)).

Each reply is a JSON-RPC envelope whose `result` carries `guid`, `cpuId`, `model`, `ip` and
`ssid` (`parseReply`). A reply is dropped when it does not parse, its `method` is not
`scan_air`, its `code` is non-zero, or its `guid` is empty. A blank `result.ip` falls back
to the datagram's source address. The device advertises no display name; the UI labels a
device by its SSID (the one field its owner sets and recognises, such as
`ASIAIR_0a1b2c3d`), falling back to model plus a short guid, because several devices of one
generation report a byte-identical model string. The raw datagram is kept so an
operator-triggered scan can log exactly what each device said.

The registry holds one entry per guid, with a cpuId → guid secondary index for non-empty
cpuIds only. An entry records the device's sighting on every site that has seen it, the
site its address resolves through, and when it was first seen. A device is
**online** while its current sighting is younger than the staleness window (135 s).

| Call | Network I/O | Purpose |
|---|---|---|
| `Scan` | broadcast, outside the lock | Background round; merges replies, then wakes rigs (below). |
| `ScanOnDemand` | broadcast, outside the lock | The Scan button and opening the Devices page; logs every reply verbatim and names each retained entry that did not answer. |
| `Discovered` | none | The UI list; `Managed`/`RigID` come from matching each entry's guid or cpuId against the live config (`matchManaged`). |
| `Resolve(guid, cpuId)` | none | A worker's current endpoint for its device (§3.2). |
| `ProbeGUID(endpoint)` | unicast scan_air | The identity primitive (§4). |
| `DeviceShares(guid)` | one guest SMB session | Adopt-time share listing (§2.3). |
| `Count` | none | The registry-size gauge, safe on the metrics scrape path. |

At startup the composition root runs one synchronous `Scan` to warm the registry before
the manager starts the workers, so a worker's first cycle can resolve; `RunBackground` then
refreshes every 45 s and does not scan eagerly, so startup does exactly one round. After a
round, discovery tells the manager, by rig id, which managed rigs' devices answered
(`RigWaker.WakeRig`), so a rig that went unreachable resumes when its device reappears
rather than sitting out its whole retry interval. The callback runs with the registry lock
released, because the manager takes its own lock.

### 2.2 Bounding reply floods

scan_air is unauthenticated UDP, so any host on the segment can answer. The registry bounds
what an answer can cost:

- one broadcast round processes at most 256 datagrams (`maxReplies`) and dedupes by guid
  within a site;
- the registry holds at most 256 entries (`maxEntries`), dropping the oldest-seen unmanaged
  entry first;
- an unmanaged device unseen for 10 minutes (`forgetWindow`) is evicted;
- a guid or cpuId that a managed profile binds is exempt from both the cap and eviction, so
  a flood cannot push a managed device out of the registry;
- `Discovered`, `Resolve` and `Count` do no network I/O, so listing the registry cannot be
  turned into an SMB amplifier; the only SMB discovery ever opens is `DeviceShares`, for the
  single device an operator is adopting, bounded by a short budget;
- the registry mutex never wraps network I/O: a round's transport call runs unlocked and its
  replies are merged under the lock afterwards.

A site whose replies must come from the address they name (`RepliesMatchSource`, set by a
remote site whose forwarding allowlist is built from reply sources) drops a reply whose
`result.ip` differs from its datagram source, since recording that address would point the
rig at a path the site refuses to carry.

### 2.3 Adoption is explicit

Commander never manages a device it merely discovered. The Devices page lists discovered devices
split into managed and available; an available device is adopted through the ordinary rig dialog
with every setting pre-filled (guest credentials, a name derived from the device label) and the guid
shown read-only. On open the dialog calls `POST /api/discovered/{guid}/shares`, which opens one
guest SMB session to the resolved address and lists the device's shares to prefill the watched-share
list. Saving posts a `RigProfile` carrying the device's guid and cpuId to `POST /api/rigs`, which
validates it like any other rig. First-run setup's first-rig step has its own, simpler picker
([architecture §8.2](2026-10-17-architecture.md)): it scans, lists the devices no rig manages,
labels each by model and a short guid rather than the SSID-first label of the Devices page, and
binds the chosen device by guid and cpuId.

Whether a device is already managed is answered by the registry match on guid or cpuId, never by
reading anything on the device; a device carrying a rig-identity marker file (§4.5) but an unbound
guid still lists as available (`TestStaleIdentityMarkerStillAdoptable`). A managed device does not
appear in the available list at all. Two profiles cannot bind one guid (§3.4).

## 3. Binding a rig to a device

### 3.1 guid and cpuId

A rig profile (`model.RigProfile`) carries `guid` and `cpuId` (YAML and JSON keys `guid`,
`cpuId`; the camelCase matches the wire protocol), set at adoption. The guid is the primary
key. The cpuId is a bonus: some device classes (the Raspberry Pi CM4-based units) report an
empty cpuId, so it is optional everywhere and never required to match. It earns its place in
one case: a factory reset can change a device's guid while the SoC serial survives, and the
cpuId fallback keeps such a device resolving and confirming with no config rewrite.

A profile may instead carry only the `host` field (a hostname or IP). Such a
**host-bound** rig is migrated to a guid binding automatically (§3.4); `host` exists only as
that migration's fallback. Validation (`validateBinding`) requires a managed rig to carry a
guid or a host and rejects two managed profiles sharing a non-empty guid (a clone or
double-adopt would race two workers onto one device); separately, `validateHosts` warns when
any two rigs with a non-empty host resolve to the same address.

### 3.2 Resolution order

At the start of each cycle the worker resolves the profile to an endpoint (a site plus an
IP; `rig.Worker.resolveAddress`):

1. **Registry by guid, then cpuId.** If the profile has a guid, `Resolve(guid, cpuId)`
   looks up the guid entry. If that entry is absent or offline and the cpuId is non-empty,
   it falls back to the entry the cpuId indexes. An online answer is used. The cpuId branch
   is skipped for an empty cpuId so two cpuId-less devices cannot collide.
2. **The last-known endpoint** (§3.3), if one is stored — resilient to a broadcast outage
   or a device that answers SMB before its next scan_air reply lands.
3. **The configured host**, for a host-bound rig: its first IPv4 address (the registry and the
   probe are IPv4-only), or the literal host if it does not yet resolve. A host-bound rig
   always resolves on the local site.
4. Otherwise the rig is offline for the cycle.

Every endpoint, however it was found, is only a best guess: the session opened to it must
still pass identity confirmation (§4) before anything is pulled or deleted. Resolution
decides where to connect, never whether to trust what answers.

### 3.3 The last-known endpoint

After a session confirms identity, the worker stores the endpoint under the store-meta key
`lastip:<rigID>` (`LastIPKey`), writing only on change. The stored form names the site:
a local endpoint is the bare address, any other is `<site>..<ip>` (`netsite.Endpoint.String`,
`netsite.Parse`). An address confirmed on a remote site's LAN is therefore only ever dialled
again through that remote, and a bare value only locally; the wrong-network hazard this
closes is described in [remote-sites §6](2026-10-17-remote-sites.md). A stored value
that does not parse is skipped and logged, never dialled. Removing a remote clears every
rig's last-known endpoint on that site (`ForgetLastEndpointsOnSite`); deleting a rig clears
its own (§6.6).

### 3.4 Host-bound rigs migrate to a guid binding

A host-bound rig connects through its resolved host address and takes the same identity
probe as any other session (§4.1). With no stored guid there is nothing to compare, so the
rig trusts the device its host address answers with, exactly as the host binding already
trusted that address, and records the probe's answer as a pending bind under
`pendingguid:<rigID>` = `guid,cpuId` (`PendingGUIDKey`, `recordPendingGUIDValues`). The bind
reuses that one probe — there is no second probe and no second SMB session — and a blank
guid is never recorded.

The manager's maintenance loop (every 30 s) applies pending binds (`applyPendingBinds`).
For each managed rig with a parseable pending value it:

- clears the key and stops if the rig is already guid-bound (a late worker write);
- otherwise, inside the config guard, loads the live config, refuses with `errGUIDTaken` if
  another managed profile already holds that guid, deep-copies the config, sets `guid` and
  `cpuId`, clears `host`, validates, saves, applies and publishes it, then clears the key.

A guid collision clears the key and raises one config alert reporting the duplicate; it affects
no other rig's bind. A validation or save failure leaves the key for a later tick. A garbled
value is cleared. The migration runs continuously, at startup and whenever a host-bound rig's
device first answers, so a NAS that never restarts still migrates within a cycle or two.

The residual is the first-bind window: a different device answering at the host address
during that first probe is bound durably. It is small (the host binding was already trusting
that address), it never authorises a delete on its own (the content hash still has to
match), and it is corrected by removing the rig and re-adopting the right device.

### 3.5 State ownership and concurrency

| State | Owner and storage | Writers | Readers |
|---|---|---|---|
| Live config (`*model.Config`) | `atomic.Pointer`, published immutable | every config writer, each inside `config.Guard.Do`: the config API save, logout's session-secret rotation, setup completion, the manager's bind applier, the remote supervisor's `commit` and its `Start` apply | workers (at the cycle boundary), web |
| Discovery registry | `discovery.Scanner`, guarded by `Scanner.mu` | `merge`, under the lock, with no network I/O held | `Resolve`, `Discovered`, `Count`: snapshot under the lock, then work unlocked |
| Last-known endpoint | store meta `lastip:<rigID>` | the worker, after a confirmed session | the worker (`resolveAddress`) |
| Pending guid bind | store meta `pendingguid:<rigID>` | the worker, from a host-bound session's probe | the manager's applier, which also clears it |
| Persisted media id | store meta `mediaid:<rigID>:<share>` | the worker's pull phase (§6.3), reset (§4.4), rig deletion (`handleDeleteRig`, §6.6), which clears it | the pull phase (§6.3) |

Every config write is serialised by one mutex. `config.Guard` is a plain lock wrapper: each
writer runs its own load, modify, validate, save, apply and publish sequence inside `Do`, so
the guard adds mutual exclusion and changes no writer's behaviour. Workers never write
config; they only record a pending bind in the store. The applier deep-copies before
mutating and never touches the published pointer in place. The lock order is the config
guard before the manager's own lock, and the applier never holds the manager lock when it
calls `Do`. The worker holds one SMB session per device at a time: the identity probe is
UDP on the worker's own goroutine while that session is open. Other subsystems may open
their own sessions alongside it (the device file browser up to two per rig,
[device-file-browser §5.3](2026-10-17-device-file-browser.md); a remote digest measurement
one); the bind needs none of them. Guarded by
`TestGuardSerializesReadModifyWrite`, `TestGuardPropagatesError`,
`TestApplyPendingBindsBindsAndClears`, `TestApplyPendingBindsSkipsGUIDCollision`,
`TestApplyPendingBindsClearsStaleKeyOnBoundRig` and `TestConcurrentScanListAndDrop`.

## 4. Confirming identity in every session

### 4.1 The confirmation truth table

`rig.Worker.confirmDeviceIdentity(profile, endpoint)` sends a unicast scan_air to the
endpoint through the endpoint's own site (`ProbeGUID`) and judges the reply:

| Probe result | Profile | Outcome |
|---|---|---|
| no well-formed reply within the budget | any | **unconfirmed** (`ok=false, foreign=false`) |
| reply | host-bound (no guid) | **confirmed**; the reply is recorded as a pending bind (§3.4) |
| reply whose guid equals the profile's guid | guid-bound | **confirmed** |
| reply whose cpuId equals the profile's cpuId, both non-empty | guid-bound | **confirmed** |
| any other reply, including both cpuIds empty and guids differing | guid-bound | **foreign** (`ok=false, foreign=true`) |

The comparison is `identityMatches`, exported as `rig.IdentityMatches` so the device file
browser re-proves its own sessions with exactly the worker's rule
([device-file-browser §5.3](2026-10-17-device-file-browser.md)). A remote site's digest
measurement probes inside its own session and is judged with the same `identityMatches`
comparison (`hashIdentity`, [remote-sites §7](2026-10-17-remote-sites.md)), with two
differences: an answer carrying neither a guid nor a cpuId is unconfirmed rather than
foreign, and a host-bound rig trusts the answer without recording a pending bind.

A guid-bound rig that matches only on cpuId (its device's guid changed) keeps running on the
cpuId fallback with its stored guid unchanged; no path rewrites the guid of an already-bound
rig. A cpuId-less device whose guid changes cannot be matched at all and must be re-adopted.

On the local site the probe is sent on a fresh unconnected IPv4 socket (`netsite` local
site, `ProbeUDP`), never a connected `DialUDP` one, for the same reason as the broadcast
socket (§2.1, [asiair-hardware-notes §5.4](2026-10-17-asiair-hardware-notes.md)): a connected
socket filters out a reply whose source differs from the address it dialled.

The probe tolerates loss: an unanswered datagram is re-sent with a doubling wait (first wait
1 s plus one site round trip) until a budget of 3 s plus the site's round-trip and handshake
allowances runs out (`probeBudget`). scan_air is idempotent, so re-sending is safe. Nothing
outlives the budget; the next cycle probes afresh. Guarded by `TestConfirmDeviceIdentity`,
`TestProbeGUID`, `TestIdentityProbeRetriesWithinBudget` and
`TestIdentityProbeRoutesThroughSessionSite`.

### 4.2 Per session, not per cycle

The probe is taken while the SMB session it vouches for is open, against that session's
exact endpoint. One host owns an IP at a time and an established SMB connection fails if the
device behind it changes, so a reply from that endpoint during the session is the device the
session talks to. A registry entry cannot give that guarantee: it can name a stale device for
up to the staleness window.

The delete pass opens its own session after the pull session has closed
([frame-lifecycle §9](2026-10-17-frame-lifecycle.md)), and a DHCP handoff can land between
them, so confirmation is **per session**:

| Session | Where it confirms | On foreign | On unconfirmed |
|---|---|---|---|
| Pull (`pullPhase`) | after open, before any share is mounted | critical `identity_mismatch` alert; rig to Error with an "identity" last error; verdict Unknown (reason `identity`); cycle refused, so the delete pass does not run | rig stays Idle and online; verdict Unknown (reason `identity`); cycle refused |
| Delete (`deletePhase`) | after open, before the pass starts | the same foreign block (`refuseForeignDevice`); nothing deleted | nothing deleted; counted as a blocked delete (`identity_unconfirmed`) |
| Remote digest measurement | inside the remote's own session | foreign block for deletes only; the slice ends | the slice ends; its deletes defer |
| Reset identity (§4.4) | after open, before any marker is cleared | refused | refused |
| File browser | after open, before any operation | session refused | session refused |
| Clock sync | on the control channel itself | not set | not set ([features §6.4](2026-10-17-features.md)) |

The pull session's confirmation also gates everything that cycle does on the device:
listing, pulling, the orphan-preview sweep and session-log capture all run only after it.
Guarded by `TestIdentityForeignRefusesAndAlerts`, `TestIdentityConfirmedPulls`,
`TestDeletePassMismatchingProbeDeletesNothing`, `TestDeletePassNoReplyDeletesNothing`,
`TestDeletePassConfirmedDeletes`, `TestDeletePassCountsUnconfirmedIdentity` and
`TestIdentityForeignOnLaterSlice`.

### 4.3 Failing closed

- **No reply means refuse.** An unanswered probe is not evidence of a wrong device, so it
  raises no mismatch, but nothing is pulled or deleted on an unconfirmed session. The worker
  counts consecutive no-reply pull sessions (`noConfirmStreak`); at the third it raises one
  Warn `identity_unconfirmed` alert ("can't confirm this rig's device identity (no scan_air
  reply); pulls are paused"). A confirmed session resets the streak and auto-acknowledges the
  alert; a foreign reply also resets it, because a definite reply breaks a no-reply run.
  Guarded by `TestNoReplyStreakDegradesThenAutoAcks`.
- **A foreign reply raises the critical mismatch alert** (`identity_mismatch`), once per
  session with a value-free message so the alert store's `(rig_id, category, message)`
  dedup collapses repeats. The rig's last error always contains "identity", which the
  Devices page keys its banner on. The fix for a foreign device is to remove the rig and
  re-adopt the right device, not to reset: reset requires the live guid to equal the
  profile's (§4.4), so it always refuses a foreign device, and the banner offers Remove rig
  rather than Reset. Only a reset that clears every share acknowledges `identity_mismatch`
  (§4.4); a later confirmed session does not, so after a transient foreign reply the Critical
  alert stands until the operator acknowledges it. The DHCP-churn case is such a transient:
  resolution finds the right device again on a later cycle, so no re-adoption is needed and
  the standing alert only needs acknowledging.
- **Two profiles cannot bind one guid.** Validation rejects it at save, and the bind applier
  refuses it with `errGUIDTaken` (§3.4).
- **cpuId is optional.** It is compared only when both the profile and the reply carry one,
  so the CM4 class, which reports it empty, confirms on guid alone.

A device that answers SMB but not UDP 4720 is never pulled from. That is the deliberate cost
of a fail-closed probe: such a device has just answered a scan to be offered at all, and
a device answering a directed unicast with its broadcast guid is confirmed on hardware
([asiair-hardware-notes §5](2026-10-17-asiair-hardware-notes.md)), so the case is rare and
visible on the card.

### 4.4 Reset identity

Reset identity is the explicit operator path for a **deliberately swapped USB stick**. It is
an asynchronous rig command (`resetIdentity`), so a refusal (below) surfaces as a one-shot
config alert ("couldn't reset identity for rig …"), the channel other worker-side conditions
use. Reset clears markers, so it must positively target the right device first:

1. The rig must resolve to an endpoint, or reset is refused ("device not reachable right
   now").
2. A host-bound rig is refused ("still binding to its device"): there is no identity to
   confirm against, and the state lasts only until the bind applier runs.
3. Reset opens an SMB session and, while it is open, probes the endpoint; it proceeds only if
   the live guid equals the profile's guid. A mismatch or no reply refuses it. This couples
   reset, an SMB-only operation, to UDP 4720 reachability: a device reachable by SMB but not
   by the probe cannot be reset until the probe answers. That is the accepted trade for never
   clearing the wrong device's markers. The comparison is on the guid alone, so a rig running
   on the cpuId fallback (§4.1) cannot be reset at all.

Only the three refusals above raise the config alert. If reset's SMB session fails to open,
the failure is logged at Warn and nothing else surfaces; the operator retries.

For each enabled share it then clears the media marker at both locations (`ASIAIR/` and the share
root), clears the persisted media id to empty, and removes any rig-identity marker file best-effort.
On a removable share the `ASIAIR/` copy is authoritative, so a failed root clear (a read-only root)
does not fail the share; on a fixed share the root is the only location. When every share clears,
reset auto-acknowledges the rig's `identity_mismatch` and `media_changed` alerts; otherwise they
stand. The next pull cycle re-adopts the inserted stick (§6.3). Guarded by
`TestResetIdentityRefusesHostBound`, `TestResetIdentityRefusesOnProbeMismatch`,
`TestResetIdentityClearsOnMatchingProbe` and `TestResetIdentityClearsMediaIDAndReadopts`.

### 4.5 Why there is no rig-identity marker

An obvious design writes a per-rig marker file (`<profile id> <uuid>`) at the root of each
watched share on first bind and refuses any share carrying a different profile's marker, with
static IPs or unique hostnames as the operator's precondition. Commander deliberately has no
such marker:

| How a per-share rig marker would fail | Cause |
|---|---|
| Repeated "Access Denied" write retries on `Udisk` or `TF` | On some devices the removable share's root is not guest-writable. |
| A whole share silently never pulls | A share the marker cannot be written to is skipped every cycle. |
| Safe to power off never reads yes | The skipped share counts as a failed listing. |
| Unmanaged hardware reads as "already managed" | The marker travels with a USB stick and duplicates with cloned storage, and it keys on a profile id derived from the rig name, so two rigs with one name collapse to one identity. |
| A wrong unmarked device is adopted, not refused | An absent marker is trust-on-first-use, so a guid resolving to an unmarked box would mark it. |

The live probe answers the same question from a device-minted identity, needs no write
access, cannot ride a medium between devices, and holds in both the pull and the delete
session. A `.asiair-commander-rig-id` file found on a share (`model.IdentityMarkerName`)
is excluded from listings (`internal/filter`), hidden by the file browser, and removed
best-effort whenever a pull session or a reset holds the share; Commander never writes one; a
read-only root may keep it, harmlessly. The listing exclusion is covered by `TestIsJunkFile`
(`internal/filter`).

## 5. Threat model

Commander's trust boundary is a trusted LAN. The ASIAIR's SMB shares grant guest access
whatever password is sent, and the guest can delete (H7, H11), so any host on the
segment can already read, write and delete the device's files directly. Discovery and the
identity probe are judged against that boundary: they must get the benign failure modes
right and must not claim protection the system does not have.

| Threat | Status | Why |
|---|---|---|
| DHCP churn hands a rig's old address to a different ASIAIR | **Closed** (caught and refused, not prevented) | Resolution makes the encounter rare, not impossible: inside the staleness window a cycle can still reach the wrong device. Every session probes the endpoint it is talking to; an honest device answers with its own guid, which is foreign, so the session is refused. The content-hash check backstops any delete. |
| A DHCP handoff between the pull and delete sessions | **Closed** | Confirmation is per session (§4.2). |
| An address handoff inside one open session | Out of scope | The established SMB connection would fail; a sub-second handoff between open and probe is theoretical, and the hash still guards deletes. |
| A same-segment host forges scan_air replies, including a probe reply | **Not defeated** | No unauthenticated LAN protocol can stop it, and the attacker already has guest SMB. A spoofer still cannot make Commander delete a different device's frames: a delete needs the device's bytes to hash to the recorded anchor ([delete-safety §4](2026-10-17-delete-safety.md)). |
| A reply flood exhausting memory or amplifying into SMB | **Bounded** | §2.2. |
| Two devices with identical identity (a true clone) | **Refused at bind** | Two profiles cannot hold one guid (§3.4); detecting a clone beyond that is a non-goal. |
| Host-bound first-bind to a wrong device | Accepted residual | §3.4. |
| A device whose guid changed, matched by cpuId | Accepted | It runs on the cpuId fallback indefinitely with a stale stored guid (§4.1). |
| A remote site lies about identity or digests | Bounded | The tunnel is authenticated and encrypted; a remote measures but never compares, and Commander judges its probed identity by the same rule ([remote-sites §5](2026-10-17-remote-sites.md), [remote-sites §7](2026-10-17-remote-sites.md)). |

The web layer's own boundary (Host, Origin and CSRF checks) and the attacker it defends
against are independent of this and specified in [architecture §8.3](2026-10-17-architecture.md).

## 6. The removable-media marker

### 6.1 What it guards

A rig's Udisk share is backed by a USB stick that can be pulled, swapped or fail. The
ASIAIR keeps exporting the Udisk share with no stick inserted, its root lists successfully
and empty, and it reports a synthetic, constant volume serial with or without a stick (H5,
H10). A listing alone therefore cannot tell "every frame was pulled" from "the stick is
gone", nor one stick from another. The media marker, `.asiair-commander-media-id`
(`model.MediaMarkerName`, package `internal/mediaid`), breaks that tie: a one-line JSON
record carrying a random 128-bit id, written on the first managed cleanup of a medium,
persisted per rig and share under `mediaid:<rigID>:<share>`, and judged on every later pass
(`mediaid.Verify`):

| Verdict | Meaning |
|---|---|
| `Match` | The marker's id equals the persisted id: the adopted medium. |
| `AbsentMarker` | No marker. |
| `Foreign` | A marker whose id differs from the persisted one, or a garbled marker: a swapped medium. |
| `Unreadable` | The read failed with a real error: transient. |
| `SelfRecognized` | Reserved for a per-instance key fingerprint (§6.5); unreachable, since markers carry an empty fingerprint. |

The marker exists only on the Udisk share (`isRemovableShare`). eMMC is a single fixed
medium with a real, stable serial; the microSD (TF) share carries no marker either (§7.1).
The marker never travels outside the medium it describes, and it is junk-class by name, so
it is never pulled and never deleted by cleanup.

### 6.2 Where it lives

On a removable share the marker's canonical location is `ASIAIR/.asiair-commander-media-id`, inside
the folder the firmware writes captures into. That folder is guest-writable even on devices whose
share root is not ([asiair-hardware-notes §3.6](2026-10-17-asiair-hardware-notes.md)), so a marker
can always be written where captures exist. The share root is read as a fallback location.

`readMediaMarker` reads `ASIAIR/` first and falls back to the root only when `ASIAIR/` holds
no marker, gating the fallback on `errors.Is(err, fs.ErrNotExist)` so a transient error at
`ASIAIR/` stays a deferral and never falls through to a stale root copy. It is read-only.
Marker access goes only through the marker calls of `device.Share` (`ReadMarker`,
`WriteMarkerAt`, `Delete`), never through `Stat`, `ListDir` or `OpenReader`, so the real share
and the test fake key a relocated marker identically. Writes and verification go through
`mediaid.PrefixedOrigin`; `readMediaMarker` calls `ReadMarker` directly, and the root-copy
clear and the rig-marker removal call `Delete` directly.

The location is fixed rather than derived from where the data was found
(`resolveDataPrefix`): the prefix probe returns empty for an empty or absent stick, which is
exactly the state the marker must disambiguate.

### 6.3 Judge first, migrate second

The pull phase's `mediumUsable` judges a removable share's medium and returns two results,
`usable` and `present` (§7.2). `usable` gates marker writes, migration and adoption, and the
orphan-preview sweep's device deletes; it does not gate the delete pass or the restore pass,
which check only that a marker is readable (§6.4) and rely on the content hash. It locates
the marker, judges it, and only then writes anything:

1. **Read** the marker, `ASIAIR/` first, then the root. A store read error or a non-not-found
   marker read error makes the medium unusable this cycle.
2. **No marker anywhere** → `adoptMedium`: with frames on the medium, mint an id, write it
   into `ASIAIR/` and prove the write by reading it back (`Media.Adopt`), then persist it.
   On the nested layout the firmware writes captures into `ASIAIR/`, so a medium with frames
   has that folder, and the share seam needs no directory-create primitive (it has none). A
   flat-layout stick with no `ASIAIR/` folder cannot be adopted: the write fails, is logged
   (below), and the stick never drains. With no frames there is nothing to adopt; presence is
   settled structurally (§7.2).
3. **No persisted id** (a reset, or local state loss such as a reinstall or host move):
   there is no prior adoption to protect, so the marker is never judged Foreign. Where it
   sits decides which case this is:
   - at the **share root** → mint a fresh id: this is the post-reset shape, where reset
     cleared `ASIAIR/` but could not delete a root copy on a read-only root, and reusing that
     id would resurrect what the operator just cleared;
   - at **`ASIAIR/`** → adopt the marker's existing id in place: this is state loss, and
     minting would re-label the stick, so two installs sharing one medium would take turns
     overwriting each other's marker into permanent media-changed alerts;
   - an unparseable marker carries no identity to keep, so it is minted over like an absent
     one.
4. **Otherwise judge** with `Verify` at the location found:
   - `Match` → usable; a root-found marker is migrated into `ASIAIR/`, reusing the same id
     (a relocation, not an adoption), and the root copy is deleted only after the `ASIAIR/`
     write read back. A failed write or root delete leaves an authoritative copy in place and
     cleanup proceeds.
   - `Foreign` → unusable; one critical `media_changed` alert per share; no marker is
     written or migrated and the orphan-preview sweep deletes nothing on a medium Commander
     did not adopt. The delete pass still runs, gated by the presence check and the content
     hash (§6.4).
   - `AbsentMarker` (vanished mid-check) or `Unreadable` → unusable this cycle.

Mutating a medium before judging it would relocate a foreign marker and delete the root copy
before the swap guard had refused it, so judging comes first. A garbled `ASIAIR/` marker
beside a good root copy is judged Foreign; the root copy is deleted only after a verified
migration, so this is rare, and reset recovers it. Guarded by
`TestMediumUsableMatch`, `TestMediumUsableSwapDetected`,
`TestMediumUsableAbsentMarkerPopulatedAdopts`, `TestMediumUsableAbsentMarkerEmptyStickUnusable`,
`TestMediumUsableMigratesLegacyRootMarker`, `TestMediumUsableForeignRootMarkerNotMigrated`,
`TestMediumUsableMigrationRootDeleteFailureProceeds`,
`TestMediumUsablePostResetRootMarkerReadopts`, `TestMediumUsableNoPersistedIDReadopts` and
`TestMediumUsableAdoptFailureDegradesGracefully`.

A failed marker write is never fatal: it logs "could not write media marker; skipping
removable cleanup this cycle", pulls continue, and the stick simply does not drain until a
write succeeds.

### 6.4 The delete pass reads the marker and never writes it

Before any delete from a removable share, the delete pass requires the share's media marker
to be **readable** (`mediaConfirmedForDelete`), through the same read-only resolver. A fixed
share always passes. It never adopts, migrates or writes a marker; a share-root marker is honoured
there and relocated only by the pull phase. The restore pass applies the same gate to a sharing
row's device copy.

This is a **presence** check, not an identity check: it stops an absent or unmarked stick's
empty listing from being read as mass deletions, and it does not compare the marker against
the persisted id. A cycle whose pull phase judged the medium Foreign still runs its delete
pass, because a cycle is refused only by device-identity and session failures. That is sound
because the swap backstop is the per-frame three-way content hash plus the ±2 s source
re-stat ([frame-lifecycle §9.2](2026-10-17-frame-lifecycle.md)). Against a swapped stick:

- a recorded path missing on the swapped stick takes the vanished-source branch, which
  re-proves the NAS copy against the archive anchor before marking the row deleted — wrong
  bookkeeping that self-heals when the original stick returns, not lost data
  ([frame-lifecycle §9.4](2026-10-17-frame-lifecycle.md));
- differing content fails the re-stat or the hash and is held with a critical alert;
- a delete fires only for byte-identical content at the identical path with a preserved
  mtime, which means the bytes are proven on the NAS.

A stricter guard that runs `Verify` and accepts only `Match` or `SelfRecognized` is viable and keeps
"the delete pass never writes a marker" (Verify is a pure read, and by delete time the pull phase
has persisted the id). It would align the delete pass with the orphan-preview sweep, which already
requires a usable medium for its device deletes. It closes no known loss path, and adopting it
requires a regression test of a foreign-marked stick holding a byte-identical file at the recorded
path. The presence guard is guarded by `TestMediaConfirmedForDelete`; that an unadopted medium
authorises no delete is guarded by `TestUnadoptedMediumNeverAuthorizesDelete` (delete-safety §6
invariant 17).

### 6.5 Recovery without a persisted id

The persisted media id can be lost while the stick is fine: a reinstall, a move to another
host, a container recreated without its data volume. The rule in §6.3 step 3 recovers it
with no extra state: with no persisted id there is nothing a swap guard protects, so an
`ASIAIR/` marker is adopted in place and a root marker is minted over. Reset relies on the
same rule to re-adopt the next cycle.

`mediaid` also defines a `SelfRecognized` verdict for a marker carrying the instance's own
key fingerprint. Commander mints markers with an empty fingerprint and verifies with an
empty one, so that branch is unreachable; it is kept, with its re-persist, as the
destination for a real per-instance key. Wiring such a key needs key generation, storage and
a rotation story of its own, and the empty-persisted-id rule already gives the same recovery.
Relying on Reset for state loss instead was rejected: reset exists for deliberate swaps, and
on a drained stick it is a one-way trap, since re-adoption needs frames on the medium.
Guarded by `TestMediumUsableNoPersistedIDReadopts` and
`TestMediumUsablePostResetRootMarkerReadopts` (delete-safety §6 invariant 18).

### 6.6 Deleting a rig clears its media identity

Rig deletion (`handleDeleteRig`) clears the deleted rig's `lastip:` and `pendingguid:` keys
and one `mediaid:` key per configured source share, each logging a warning on failure. A
media key that outlived its rig would make a different stick verify Foreign the moment a rig
reused the id. The store has no prefix scan, so the keys are derived from the deleted
profile's sources. Guarded by `TestDeleteRigClearsMediaID` (delete-safety §6 invariant 19).

## 7. Removable slots and media presence

### 7.1 Removable slot versus marker-bearing share

A rig watches up to three SMB shares: `EMMC Images` (internal, always present),
`Udisk Images` (USB) and `TF Images` (microSD). Two of those are removable slots, but they
differ in what Commander knows about them, so two predicates in `internal/rig` answer two
different questions:

| Predicate | Matches | Governs |
|---|---|---|
| `isRemovableSlot` | `Udisk` and the word-anchored `tf` (`TF Images`, `tf`; not `catfish` or `outfit`) | empty-slot handling for the Safe-to-power-off verdict and the capacity math |
| `isRemovableShare` | `Udisk` only | the media marker: adoption, the delete-pass presence gate, reset's marker clear, the orphan sweep, the file browser's nested-layout guard |

An empty microSD slot behaves exactly like an empty USB slot on the wire: the share is
exported, its configured root resolves nowhere, and its free space reports the ASIAIR's own
root filesystem. So TF is a removable slot. But the marker is scoped to Udisk because the
hardware findings characterise only the Udisk serial (H10) and say nothing about the
microSD's; widening the marker predicate to TF would start gating TF device deletions on a
marker path never exercised for that slot, a change to the delete path with no evidence
behind it. A TF delete is guarded by device identity and the content hash, like eMMC.

Names are matched rather than behaviour because ASIAIR share names are fixed by firmware;
detecting a slot by its shape (mounts, no `ASIAIR/`, no configured root) would silently
swallow a genuinely misconfigured fixed share as an empty slot. The known limit is that a
share exported under a non-default name is not classified as a removable slot; a
config-level `removable` flag would lift it and is left to broader share-configuration work.

### 7.2 The structural presence oracle

`removableMediumAbsent(share, roots)` lists the share root and returns `(absent, known)`:

- the root does not list → `known=false`: presence cannot be told, and every caller treats
  that as **not present**, never as present;
- the root holds an `ASIAIR/` directory (the nested layout) or any configured capture root
  (the flat layout) → present;
- otherwise → absent.

It is name-independent and consulted at two points in the pull phase:

1. **The walk not-founds.** For a removable slot whose configured root does not resolve, an
   empty and readable root makes the share absent: it is logged at Debug once per cycle (not
   a per-cycle Warn), added to the cycle's absent shares, and added to the structurally
   empty set the disable guard trusts (§8.2). Anything else is a hard listing failure.
2. **The walk succeeds** on a Udisk share. `mediumUsable` returns two results, `usable`
   (may Commander write a marker on this medium and sweep its orphan previews?) and `present`
   (was this medium proven to physically exist this cycle?). Only `present` feeds the
   verdict: it is read only by the cycle's absent set, which the listing status and the
   verdict consume, while the delete gate
   (`mediaConfirmedForDelete`, §6.4) is a separate pure read of the marker that this result
   never reaches, so presence cannot widen delete permission. A drained stick is unusable
   (nothing is adopted) yet plainly present, and conflating the two would pin a drained rig
   at Unknown forever.

| Path in `mediumUsable` | `usable` | `present` |
|---|---|---|
| a fixed share | yes | yes |
| store or marker read error | no | no |
| `Match`, or adoption of an existing id after state loss (persisted) | yes | yes |
| `Foreign` | no | **yes** — a foreign marker proves a stick is inserted |
| marker vanished mid-check, `Unreadable` | no | no |
| no marker, no frames, root says absent | no | no (H5 preserved) |
| no marker, no frames, root unreadable | no | no |
| no marker, no frames, root says present | no | **yes** — drained but inserted |
| adoption failed (generate, write or persist), or persisting an existing id after state loss failed | no | yes — the medium is proven present |
| adoption succeeded | yes | yes |

`present=false` from a read error and from a structurally absent stick are the same value,
so the disable set never keys on it: a share joins `EmptySlots` there only if an independent
`removableMediumAbsent` probe also proves it empty (invariant D3). The marker-less drained
case is the reason the oracle exists: a stick that never held frames while managed carries
no marker and cannot obtain one, since adoption needs frames.

Writing the marker on first sight rather than on first cleanup, so every stick is marked,
was rejected: it widens the write surface onto media Commander may never clean. Treating any
successfully listed removable share as present was rejected because it reintroduces H5.
Guarded by `TestMediumPresenceDrainedStickIsPresent`, `TestMediumPresenceAbsentStickIsAbsent`,
`TestMediumPresenceMatchIsPresent`, `TestMediumPresenceForeignIsPresent`,
`TestMediumPresenceUnreadableRootIsNotPresent`, `TestSafeToPowerOffDrainedRemovableIsSafe`
and `TestPullRemovableAbsentMediumQuiet` (delete-safety §6 invariants 16, 17 and 21).

### 7.3 Residual risk, settled on hardware

Because the oracle arbitrates presence, a wrong answer points toward a false *safe* in
principle. Its premise holds — a present medium carries `ASIAIR/`
or its capture roots — but the converse, "an `ASIAIR/` folder implies a present, readable
medium", would fail if a stick that fails to mount left the device exporting a mountpoint
with leftover capture folders.

Reachability is narrow, because a safe verdict also needs a successful recursive walk of
every configured capture root:

| Exported shape | Outcome |
|---|---|
| a bare `ASIAIR/` with no configured root inside | the walk not-founds, the oracle says present, so it is a hard listing failure → Unknown |
| `ASIAIR/<configured root>/`, empty, with no medium behind it | the walk succeeds with zero frames → safe is reachable; the only false-safe shape |
| a medium that mounts and lists but whose frame entries are corrupted away | safe is reachable, and no oracle can prevent it: the medium genuinely mounted and read |

On real hardware, a slot with no stick and a slot holding a stick with its partition table
and filesystem headers zeroed export **byte-identical** share roots — only `.` and `..`, with
identical free-space figures ([asiair-hardware-notes §4](2026-10-17-asiair-hardware-notes.md)).
The firmware exports the bare mountpoint and no capture folders, so the false-safe residue
shape does not occur and the oracle reads absent in both cases. The mid-session stick-loss
case also leaves no residue: the device redirects capture to the eMMC (H12). The oracle
stands with no stat or liveness probe.

Two hardenings were rejected. A stat or liveness probe adds nothing: by the time the oracle
is consulted the cycle has listed the root, walked every configured root and read the marker
at two paths, a stronger liveness proof than a stat. Requiring a positive marker read for a
safe verdict on removable media brings back the drained-stick trap of §7.2. Both "a
non-programmer will pull the plug on this word" (against a false safe) and "by morning it is
done" (against a permanent false Unknown) are held deliberately; delete-safety §6 invariant
16 pins them as a pair.

### 7.4 An absent share does not count toward capacity

With no medium in the slot, `statfs` on the removable share reports the ASIAIR's own small,
nearly full root filesystem, not a stick. Counting it would publish a phantom free-space
figure for the share and drag the tightest-share figure under the device disk reserve,
raising a false `device_disk_low` alert for a slot with nothing in it. So the listing status
(`publishListingStatus`) takes the cycle's set of absent shares and skips them for both the
published per-share figures and the tightest-share calculation. Any absent share still
suppresses the safe verdict. `device_disk_low` auto-acknowledges on the resolving
transition, when the tightest share is roomy again, so a reinserted stick leaves no stale
warning. A `deviceLowRaised` latch gates that acknowledgement so a healthy rig does not
rescan alerts every cycle, as the backlog alert does; the latch starts "possibly raised"
(delete-safety §6 invariant 25). Guarded by
`TestAbsentRemovableShareExcludedFromCapacity` and `TestDeviceDiskLowAutoAcksOnRecovery`
(delete-safety §6 invariants 23 and 24).

### 7.5 A watched empty slot reads Unknown, by design

An absent removable slot that the rig watches suppresses Safe to power off: a stick pulled
mid-run looks exactly like a drained one from the share alone (H5), so an empty slot might be
hiding frames that were never transferred. This is deliberate. The card explains
it with the power-off reason `removable_absent` ("A removable share (USB/microSD) has no
medium inserted. Insert it, or turn that share off in the rig's settings, to read safe."),
defined with the verdict in [frame-lifecycle §14.3](2026-10-17-frame-lifecycle.md) (B1).

The operator's remedy for a slot they genuinely do not use is to stop watching it (§8), not a
weaker verdict. Relaxing the verdict for a watched empty slot, or a global "hide empty
shares" switch, were both rejected because each would also hide a just-pulled stick
carrying un-transferred frames. The verdict, its reasons and how several shares combine are
owned by [frame-lifecycle §14](2026-10-17-frame-lifecycle.md) (§14.2 for multiple shares and
removable slots).

### 7.6 Invariants A1–A5

| ID | Invariant | Test |
|---|---|---|
| A1 | An empty microSD (TF) slot is treated as an absent removable slot — excluded from capacity, raising no `device_disk_low`, logged at Debug not Warn — through the real not-found listing path the Udisk slot takes. | `TestEmptyTFSlotTreatedAsAbsent` |
| A2 | An empty removable slot (TF or Udisk) still suppresses safe — H5 holds; only a drained-but-inserted medium reads safe. | `TestPullRemovableAbsentMediumQuiet`, `TestSafeToPowerOffDrainedRemovableIsSafe` |
| A3 | A populated TF share lists, pulls and counts exactly as any fixed share does. | `TestPopulatedTFShareUnchanged` |
| A4 | The delete-pass media gate does not apply to TF: a TF device deletion is not gated on a media marker (`isRemovableShare` is Udisk-only). | `TestTFDeleteGateUnchanged` |
| A5 | `isRemovableSlot` matches `TF Images` and `Udisk Images` and rejects `EMMC Images` and incidental substrings (`catfish`, `outfit`); `isRemovableShare` matches Udisk only. | `TestRemovableSlotClassification` |

All are in `internal/rig`. A1 and A2 are the load-bearing pair: A1 pins that an empty TF
slot raises no false alert or phantom capacity line, and A2 that the absent-slot verdict
still suppresses safe. A4 pins that the marker gate covers Udisk only. The coverage map is
`test/ADVERSARIAL_COVERAGE.md`.

## 8. Disabling a share

### 8.1 What disabled means

A watched source (`model.Source`) carries `disabled` (YAML and JSON `disabled`, omitted when
false). The field is `Disabled` rather than `Enabled` so its zero value means enabled: a
config without the field loads every share enabled, with no nil handling.

A disabled share is ignored wherever the rig acts. The pull phase, reset and the session
build their share sets from `RigProfile.EnabledSources()`, so a disabled share is never
mounted, listed or pulled and never enters the absent set, the capacity math or the verdict.
The delete and restore passes iterate rows from the store rather than the watch set, so they
consult `RigProfile.DisabledShares()` and skip a disabled share's rows: its device originals
are not cleaned, its frames are not restored, and its possibly unreachable share is not
mounted per frame. A share carrying both a disabled and an enabled source of the same name
counts as enabled. A share removed from config entirely is not "disabled", so its recorded
frames still drain. Store-level aggregates that are not share-scoped (the resident count
behind the drain label, the never-deletable backlog count) still count a disabled share's
leftover rows; that is harmless to the verdict, and is why the claim is "ignored where the
rig acts", not "erased".

The two mechanisms cover different cases. Treating an enabled empty TF slot as a removable slot (§7)
keeps a slot the operator uses but has momentarily emptied harmless: no false alert, correct
capacity, and an honest Unknown verdict. Disabling drops a slot the operator never uses from the
verdict entirely. A rig whose only non-drained shares are disabled reads safe. In the rig editor the
toggle sits beside the share's remove button, with the helper text "Disable a slot you don't use —
only while it's empty", and is offered only for a share in the rig's published empty-slot set.

### 8.2 The disable guard

Disabling a share that still holds un-pulled frames would hide them behind a false "safe".
The guard therefore uses one rule with no count: **a share may be newly disabled only when
the rig's most recent fresh listing proved its slot structurally empty.** An empty slot
holds no medium and no frames to strand.

- **The empty-slot set.** `RigStatus.EmptySlots` (`empty_slots`) lists the shares whose slot
  was structurally confirmed empty this cycle (§7.2). It is per-cycle like the verdict:
  cleared at the start of every pull phase, so a refusing or offline cycle publishes an empty
  set, never a stale membership. It is deliberately narrower than the cycle's absent set,
  which also holds a frame-bearing share hit by a transient marker or store read error —
  fine for suppressing the verdict, fatal for an authorisation that permanently removes the
  share from it.
- **One funnel.** The guard (`disableGuardProblems`) runs in the web layer's single config
  save path, which every rig create, rig update and config PUT passes through, so it cannot
  be bypassed through the API or duplicated inconsistently. It fires for each source
  disabled in the candidate config that is not already disabled in the stored one, which
  covers editing a share, creating a rig with a pre-disabled share, removing and re-adding
  a share disabled in one update, and inserting a rig through `PUT /api/config`.
- **The conditions** (`confirmDisableAllowed`): the rig is online; it is not paused (a
  paused worker runs no cycles, so its set and listing time are frozen); its last successful
  listing is within one poll interval; and the share is in `EmptySlots`. Otherwise the save
  is refused with a per-field problem on `rigs[<id>].sources` ("Can't disable '…' while a
  medium is inserted — remove it (or delete the share) first", "Bring the rig online so we
  can confirm '…' is empty before disabling it", and the paused and stale variants), which
  renders inline through the shared field-error mapper
  ([architecture §8.8](2026-10-17-architecture.md), C1–C4).

A rig being created has never listed, so creating a share pre-disabled is always refused: the
operator adds it enabled, lets one listing confirm the slot empty, then disables it. A
medium that is present but drained cannot be disabled either; the operator removes the
medium first or deletes the share.

**Residual.** An operator who physically inserts a card carrying un-pulled frames and
disables its share before the next listing sees the medium is permitted: the guard reads the
still-empty set, and the verdict then ignores the card. This is a deliberate, self-inflicted
sequence, bounded by the one-poll freshness rule, in the same class as hand-editing
`disabled: true` into `config.yaml` (no listing exists at load time to check against). The
design does not claim protection against an operator hiding media they just inserted. A
synchronous re-probe at save time would narrow the window further and is not required.

Rejected alternatives:

| Alternative | Why rejected |
|---|---|
| A UI-only confirm over the last published pending count | The count reads 0 exactly when it matters (slot empty at the last listing, frames land, rig goes offline), and a hand-edited config bypasses the UI. |
| A server-side guard refusing a share with pending frames | Every count-based predicate leaks: a listing-failed or just-emptied share reads zero, and the published count is stale between listings. |
| Allowing a drained-but-inserted share to be disabled | A frame can land on the present medium between the confirming listing and the save. |
| Letting the delete pass keep cleaning a disabled share | "Disabled" would not mean ignored: the share would still be mounted per frame, failing noisily when unreachable, and its originals deleted. |

### 8.3 Invariants D1–D4

| ID | Invariant | Test |
|---|---|---|
| D1 | A disabled share is ignored where the rig acts: never mounted, listed, pulled, counted toward capacity or the verdict, or cleaned or restored by the delete and restore passes (aggregate store counts still include its rows, harmlessly). | `TestDisabledShareFullyIgnored`, `TestDeleteAndRestoreSkipDisabledShare` (`internal/rig`) |
| D2 | A config with no `disabled` field loads every share enabled. | `TestSourceDisabledDefaultsEnabled` (`internal/model`) |
| D3 | `RigStatus.EmptySlots` holds only structurally confirmed empty slots, not the broader absent set: a frame-bearing share hit by a transient read error is absent (verdict suppressed) but not in `EmptySlots`, so the guard cannot strand its frame. | `TestStructurallyEmptySlotSetExcludesTransient` (`internal/rig`) |
| D4 | The save path refuses to introduce a disabled share (edit, create, re-add or config PUT) unless the rig is online, not paused, its listing is within one poll interval and the share is in `EmptySlots`, returning a problem on `rigs[<id>].sources`; an already-disabled source passes untouched. | `TestDisableGuardStructurallyEmptyOnly`, `TestDisableGuardCreateAndReAdd` (`internal/web`) |

D3 and D4 are the load-bearing pair: D3 pins what the published set may contain, and D4 that
the guard authorises a disable only from it. The coverage map is
`test/ADVERSARIAL_COVERAGE.md`.

## 9. Identity across sites

A rig's device may be reached directly on Commander's LAN or through a paired remote at
another site. The guid binding, the cpuId fallback, confirmation and the frame keys are the
same on every path; moving a rig between paths changes only the endpoint its next cycle
resolves to. How identity holds across paths (the site-qualified last-known endpoint of §3.3,
the probe sent inside the open session, path precedence for a rig visible on two paths,
remote-found adoption and local-only host-bound rigs) is specified in
[remote-sites §6](2026-10-17-remote-sites.md); how the delete path's reads run through a
remote is in [remote-sites §7](2026-10-17-remote-sites.md).

The registry side of path precedence is guarded by `TestScannerFansOutAndTagsSites` and
`TestPathPrecedenceHysteresis`; removing a site drops its sightings (`DropSite`), guarded by
`TestDropSiteRemovesSightings`.

## 10. Rejected alternatives

Alternatives that reject one specific mechanism sit with it: the rig-identity marker (§4.5),
marker placement and ordering (§6.2, §6.3), state-loss recovery (§6.5), presence hardenings
(§7.2, §7.3), the verdict for watched empty slots (§7.5) and the disable guard (§8.2). A
stricter delete-pass media guard (§6.4) is viable and not rejected. The rest:

| Alternative | Why rejected |
|---|---|
| Keep hostname-based binding, guessing `asiair`, `asiair-1`… and SMB-probing them | Unreliable under DHCP churn and yields no durable identity. |
| A rig-marker "router": on a mismatch, probe the configured hosts, read their markers and rebind | A workaround for lacking a device id; scan_air's guid is that id. |
| A MAC-address fingerprint | SMB never exposes the peer MAC; ARP is OS-specific, same-segment only and invisible behind the device and test seams. guid and cpuId are in-band and testable. |
| A token file Commander writes to the device's internal storage | Device-minted guid and cpuId already exist, and a written token inherits every marker failure of §4.5. |
| Workers write the guid bind into config directly | N concurrent config writers racing the web layer. Workers record a pending bind; one applier writes through the guard. |
| A separate migrator session (at startup or on a schedule) | A second worker-side SMB session to a device whose worker is live breaks the one-worker-session rule and needs a fleet pause. In-session correlation needs neither. |
| Correlate a host-bound rig to a guid by the registry's address | Binds whatever the registry last saw at that address, which a stale entry gets wrong during a reboot storm. The live probe ties the guid to the connected device. |
| Rewrite a bound rig's guid when its device's guid changes | A runtime config write per change, for nothing: resolution and confirmation already match on cpuId. |
| Require cpuId as a hard second key | The CM4 device class reports it empty. |
| Confirm identity once per cycle, in the pull session only | The delete pass runs in its own session (§4.2). |
| A durable soft anchor for the host-bound window | It would share the manager-consumed pending key, races it, and protects only the window, not the first bind; the content hash and re-adoption cover the residual. |
| Drop the media marker and rely on the content hash alone | The marker answers the one question the probe cannot (device is not medium): it raises `media_changed` on a swapped stick, keeps Commander from writing markers on or sweeping orphan previews from a stick it did not adopt, and its presence check stops an absent stick's empty listing from reading as mass deletions. Device deletes themselves still rest on the content hash (§6.4). |
| The ASIAIR control channel (port 4700) for identity | A second, stateful protocol for no gain over scan_air. Clock sync does use it, and confirms identity on it ([features §6.4](2026-10-17-features.md)). |
| An active subnet sweep or mDNS | One broadcast reaches the whole broadcast domain; mDNS is unverified on these devices. |
| A user-facing manual address field | Resilience is automatic (the cpuId fallback and the last-known endpoint). |
| A rig-identity marker whose failed write is non-fatal after a successful probe | Treats a symptom: it keeps the fragile two-signal model, false blocks from a roaming USB stick, and the repeated Access Denied write retries of §4.5. |

Non-goals: routed or cross-subnet discovery on the local site (a broadcast does not cross
subnets or broadcast-isolated access points; a remote site is the supported way to reach
another network), IPv6 discovery, and detecting cloned devices beyond refusing a second
profile on one guid. A rig whose device moves to a routed-away subnet, so that its last-known
address no longer reaches it, becomes unreachable and is recovered at the network layer.
