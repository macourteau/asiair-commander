# Remote sites

**Scope.** How Commander manages ASIAIRs at a remote location, such as a remote observatory,
through a small always-on computer there (a Raspberry Pi or a mini PC) running Commander's
`remote` role behind a pinned tailcat tunnel. This doc covers the tunnel and why it has this
shape; the Endpoint, Site and tunnel seams, with the local site as a null object; what runs on
the remote and what stays in Commander; pairing and trust; device identity across network paths;
the delete path, where the remote measures and Commander compares; behaviour on an unreliable
link; transfer admission and bandwidth; what the UI shows; packaging and version skew. It hosts
the remote invariants 1–12 (§12) and the remote acceptance criteria AC1–AC27 (§13). Both are
cited doc-qualified: "design remote-sites §12 invariant 4", "design remote-sites AC27".

## 1. Problem and scope

Commander manages the devices it reaches on its own LAN. A rig at a remote site, with a small
internet-connected computer next to it, is outside that reach unless something carries
Commander's device traffic there: its frames would not be pulled, verified, filed or cleaned up,
and it would have no card.

Commander touches a device over five channels, each a TCP dial or a UDP exchange:

| Touch | Protocol |
| --- | --- |
| Discovery (`scan_air` broadcast) | UDP 4720, broadcast |
| Identity probe, once per SMB session | UDP 4720, unicast |
| Reachability probe | TCP 445 |
| SMB1 session: listing, pull, hash, delete | TCP 445 |
| Clock-sync control channel | TCP 4700 |

The wire facts of each live in [hardware notes §5, §6 and §9](2026-10-17-asiair-hardware-notes.md).

A device address alone says nothing about the network it belongs to. A remote-LAN address such
as `192.168.1.50` means nothing on Commander's LAN and may belong to a different device there.
Rig identity, by contrast, is already independent of the network path: a rig is bound by its
`scan_air` guid, with cpuId as a second key, every SMB session that can pull or delete re-proves
it with a live probe, and frame rows and media-id keys are keyed by rig
([device-identity-and-media §3–§4](2026-10-17-device-identity-and-media.md)). The same rig
reached over another path is therefore the same rig, provided its address carries the path.

The link to a remote site is assumed unreliable: consumer uplinks, cellular or satellite,
sometimes behind carrier-grade NAT, with outages from seconds to hours. The remote PC's own LAN
link to the ASIAIR is assumed as reliable as a home LAN. Two delete-path properties must survive
the remote link unchanged: the content-hash three-way check before every delete
([frame-lifecycle §9.2](2026-10-17-frame-lifecycle.md)) and listing completeness
([delete-safety §5](2026-10-17-delete-safety.md)).

Product requirements the design meets:

- The remote runs in a Docker container with an auto-updater, the main deployment for a machine
  nobody wants to think about updating. The Windows executable can also run the role by hand.
- The Devices page has "Add a remote" and a "Your remotes" section listing remotes and the rigs
  reached through each.
- A device a remote finds goes through the same adopt flow as a local one ("Found on your
  network", then "Your rigs"), and a remote-reached rig carries a "remote" pill.
- A known rig reached directly or through a remote is the same rig, so a rig can move to a
  remote site and back, temporarily or permanently.
- One remote serves one Commander. Every ASIAIR on a remote's LAN belongs to the operator; a
  foreign device on that LAN is a problem with the remote's network, not one this design works
  around.
- A remote is a convenience. The operator keeps access to the remote PC (shell, remote desktop
  or a site visit), and every remedy for a remote's pairing (a Re-pair after a suspected leak,
  a retired relay region, a lost env value, an accidental Re-pair, a reinstalled Commander) is an
  edit of the `.env` there.
- An installation that never adds a remote has the same behaviour, configuration and on-disk
  state as one built without remote support.

## 2. The tunnel

### 2.1 What tailcat gives and what it forces

tailcat (`github.com/tailscale/tailcat`) is a userspace WireGuard tunnel that rendezvouses
through Tailscale's DERP relays and needs no account. Both roles use one pinned released tag
(see `go.mod`); nothing depends on tailcat's unreleased `main`. These facts decide the shape that
follows.

| Fact | Consequence |
| --- | --- |
| Only a tailcat `Client` opens connections. `Server` has no dial method, and the client's netstack refuses every inbound flow. | The side that opens SMB and control sessions to the ASIAIR must be the client, and that side is Commander. |
| An address is `tc` + base64url(CBOR) of the server's public keys, a pre-shared key (PSK) and the DERP region. | The address is a secret bearer capability, not a public identifier. It is stable across restarts only if the key, the PSK and a fixed region are all persisted. |
| A tailcat "saved key" is a server identity: node private key, PSK and region. | What goes in the remote's env is the remote's server secret, not an address Commander serves. |
| `AllowedClients` pins clients: an empty list admits everyone, a non-empty list admits only those keys, and keys can be added but never removed. The pinned release has no trust-on-first-use hook; an upstream `AllowClient` hook would replace `AllowedClients`. | Pinning a key known before the first packet works on the pinned release. Trust-on-first-use (TOFU) cannot be done safely on it. |
| A client key the server does not admit is dropped silently: the client sees only a timeout, as from a remote that is down. | Commander cannot tell a mismatched `.env` from a down remote, so the card's wording names both causes. |
| The library, CLI and protocol have no key rotation. | Rotation would be a protocol of Commander's own. |
| A `Client` does not recover by itself after its server restarts (still disconnected after 300 s). A freshly built `Client` connects in about 0.1–0.3 s over public DERP. After first use, `Ping` returns immediately and proves nothing. With both processes alive, the same client recovers within seconds of a link returning. | Commander runs its own heartbeat and client-rebuild loop per remote, and a rebuild is needed only after a server restart. |
| After a client is built or rebuilt with a key the server has seen before, its first packets are lost until WireGuard's handshake retry about 5 s later, because the server answers the previous process's path. A TCP dial rides this out by retransmitting; a lone UDP datagram does not. Once the tunnel has carried traffic, no probe is lost. | The identity probe is sent only inside an SMB session already open on that site (§6), and a timeout that may be a fresh client's first exchange allows about 5 s for the handshake (§8.1). |
| `Client.DialTCP` and `Client.DialUDP` reach any `ip:port` the server's per-flow handlers (`OnTCPForward`, `OnUDPForward`) agree to forward; `AllowProxy` is declared but never read. Returned conns are gVisor `net.Conn`s whose errors are `*net.OpError` and which honour `SetDeadline`. A forwarded UDP socket stays open on the server for `UDPIdleTimeout` (2 minutes by default) after its flow ends. | The unicast identity probe and every TCP session run through the tunnel unchanged. `smb1.IsNetworkError` and the SMB idle deadline keep working. The forwarding policy lives in the per-flow handlers. |
| `OnTCPForward` runs before the client's TCP handshake completes: the client's dial stays blocked while it runs, a nil handler is answered with a RST, and a returned handler runs on the established conn. | The remote dials the device before returning a handler and returns nil when that dial fails, so a powered-off ASIAIR behind a live remote fails Commander's dial and its rig takes the ordinary offline path and alert, as on the LAN. A dial to a powered-off ASIAIR fails as a refused network error within about 5 s while *hello* keeps succeeding, and both recover once the device boots ([asiair-hardware-notes §5.3](2026-10-17-asiair-hardware-notes.md)). |
| Both ends rendezvous through Tailscale's public DERP servers (single-node regions, rate-limited, no SLA, revocable) and relay through them when hole-punching fails. One relayed transfer measured 1.09 MB/s against about 7.2 MB/s direct; whether the relay's rate limit or its latency set that was not isolated. | On a relayed path every FITS byte rides a third-party relay: tens of GB per rig-night (23–70 GB) at about 1 MB/s. The remote's card shows direct or relayed and the latency, and the operator docs name the dependency and its scale. |
| A stalled TCP flow through the tunnel resumes only at TCP's next retransmit after the link returns (about 43 s after a 60 s outage). A dial made during an outage fails with a bare context deadline. | A remote site's stall window is longer than the LAN's (§8.1), and the tunnel wrapper wraps the bare deadline as a network error. |
| tailcat links neither captive-portal detection nor the port mapper; netcheck contacts only the address's embedded region, and probes every public region only when no region is fixed. | A fixed region at pairing keeps the remote's network footprint to one relay region. |
| tailcat brings about 317 packages and 8–11 MB to a stripped binary. The Windows cross-build passes. | Commander pays this as a client, whatever the remote's packaging. |
| tailcat makes no API, CLI or wire-format stability promise, and its address format is not stable. | Both roles ship from one release with one tailcat pin, and a tailcat bump auto-merges only when the tests that catch a material change pass (§11). |

### 2.2 The decision

A **remote** is a tailcat *Server* on the remote PC, pinned at pairing to exactly one Commander
client key. It forwards to the ASIAIRs on its LAN under a least-privilege forwarding policy (§5),
and over the tunnel it serves the three-call **remote protocol**:

- *hello*: its remote-protocol versions, its whole-file and data-unit digest-algorithm versions,
  its build version, commit and date, its uptime, a per-process instance id, its direct or
  relayed path and relay region, and its recent warnings, each with a sequence number and an
  age. Its round trip is the latency sample.
- *broadcast*: send one `scan_air` round on its LAN and return the raw replies, each with its
  UDP source address.
- *hash*: measure one file's pre-delete digests in its own SMB session, never compare (§7).

Every request carries a Commander-generated call id, which the remote echoes in its log lines and
in any warning the call produces. The remote keeps **no durable state**.

Commander is a tailcat *Client* per remote and dials the ASIAIR through it as if the ASIAIR were
on Commander's LAN. Every decision (identity, pull, the three-way check, delete, clock) stays in
Commander, under a **site** layer: every device address is a typed endpoint naming the network it
is reached through, and the local site is a null object, so no code branches on local versus
remote.

```
 Commander host                                          Remote site (RPi / mini PC)
 +------------------------------------------------+      +-------------------------------------+
 | rig.Worker (holds its site), discovery.Scanner |      | asiair-commander, `remote` role     |
 |   addresses are typed: Endpoint{Site, IP}      |      |   stateless; the .env is the pairing|
 |              |                                 |      |                                     |
 |              v                                 |      | tailcat Server                      |
 | site registry: Dial, Probe, Broadcast,         |      |   key, PSK, fixed region from .env  |
 |   MeasureDigests, State, Costs; admission      |      |   AllowedClients = [Commander key]  |
 |   |-- local site (null object): net.Dialer ----+-LAN> |   refuses to start without it       |
 |   '-- remote site R: tailcat Client R =========+=WG==>|   forwards TCP 445, TCP 4700,       |
 |                                                | via  |   UDP 4720 to IPs that answered ----+--> ASIAIR(s)
 | remote supervisor: heartbeat, latency,         | DERP |   its own scan_air broadcast        |
 |   rebuild; one Site per remote                 | or   | remote protocol on the tunnel:      |
 | tunnel wrapper: the only tailcat importer      |direct|   hello, broadcast, hash            |
 | web: /api/remotes; hub: Snapshot.Remotes       |      |   (hash measures, never compares)   |
 +------------------------------------------------+      +-------------------------------------+
   dials out only; accepts nothing                         admits only the pinned key
```

## 3. Abstraction seams

Three layers carry the feature, and the rig logic's decisions above them do not depend on the site.

| Layer | What it hides | Who sees it |
| --- | --- | --- |
| **Endpoint** (`netsite.Endpoint`) | Which network an address belongs to. An endpoint is a typed `Endpoint{Site, IP}` at every Go boundary that carries an address: the resolver, the device factory, the identity probe and the control dialer. Its stored form, kept in `lastip:`, is the bare address for the local site and `id..ip` for any other. A legacy bare IP or hostname parses as the local site, so every stored `lastip:` value, every `Host`, every test fixture and the demo resolver keep their meaning. An empty DNS label (`..`) is the one thing no resolver puts on the wire, so an older binary reading `id..ip` as a bare host fails closed: `net.ParseIP`, Go's resolver, libc's and Windows' resolvers all reject it before sending a query. | Every boundary that carries an address, by type, so the compiler lists every consumer. |
| **Site** (`netsite.Site`) | How bytes reach a device on one network, and what that network costs: dial a TCP port, send one unicast `scan_air` probe, run one broadcast round, measure a file's pre-delete digests, report its state and its `Costs` (latency, minimum recent latency, stall window, handshake allowance, whether broadcast replies must match their source, read cap, and whether it is a remote with its display name). The **local site** is a null object: plain `net.Dialer` and UDP sockets, latency 0, always up, the global transfer cap, no rate limit, and a pre-delete hash that is the streaming read in Commander's own session. A **remote site**, provided by the remote supervisor, has a measured latency, a real state, its own transfer slot, a rate limiter and the remote *hash*. The **site registry** maps site id to site; it is distinct from discovery's device registry, which is keyed by guid. An unknown id, or a site that is not up, yields a site whose every dial and probe fails with a `net.Error` without opening a socket. Results only a remote produces, such as the identity the remote *hash* probes in its own session, are part of the shared result type. | The worker, which holds a handle to its site; the device factory, the control dialer, discovery and admission. Display may read a site's identity (§12 invariant 12). |
| **Tunnel wrapper** (`internal/remote/tunnel`) | tailcat itself: the server role (start from the pairing secret, the pinned key, the forwarding policy and a remote-protocol listener), the client role (TCP and UDP dials, remote-protocol calls), the pairing mint, and an in-memory fake for tests. | The remote role and the remote supervisor. It is the only package that imports tailcat, so an API break such as `AllowedClients` giving way to `AllowClient` lands in one place. |

The network-path package is `internal/netsite` rather than "site", because "site" already names
the observing location whose timezone frames are dated in.

The site layer is delete-path and identity code in its own right, not plumbing: site-qualified
addresses re-route the identity probe and the `lastip:` fallback, and the remote *hash* moves where
the pre-delete device digest is measured. That is why a change to it gets the delete-path review
(AC14).

Every session opener dials through the rig's site, the reset-identity path included. The
composition root builds the site registry and the remote supervisor and hands the registry to
discovery, the demo installer and the manager, which passes it to each worker. `web.Supervisor`,
the store schema and every decision of the cycle are independent of sites.

## 4. What runs where, and the data flow

| Concern | Remote | Commander |
| --- | --- | --- |
| Pairing secret | Its server identity and Commander's client public key, from the env file | The remote's address and its own per-remote client key, in `config.yaml` |
| Discovery | Sends its own fixed `scan_air` request on its LAN when asked, returns the raw replies with each one's source address, and updates its forwarding allowlist (§5) | Parses them with the shared parser, tags each with the site, merges them into discovery's device registry under the path rule (§6) |
| Identity probe | Forwards one UDP 4720 flow | Sends the probe through the session's site, once the SMB session is open |
| Listing, pull, delete | Forwards TCP 445 bytes | smb1client and every check, exactly as on the LAN |
| Pre-delete hash | Measures the file's digests and the device's identity in its own SMB session (§7) | Asks the session's site for them; makes the identity comparison and the three-way compare |
| Clock sync | Forwards TCP 4700 bytes | The control client; the tunnel's added latency (hundreds of milliseconds) is negligible against clock sync's 5-minute threshold ([features §6](2026-10-17-features.md)) |
| Liveness | Answers *hello* | Heartbeat, latency estimate, client rebuild, published state |
| Admission and bandwidth | Nothing | The admission policy and the optional rate cap (§9) |
| Durable state | None | `config.yaml` and `state.db` |
| Decisions | None | All of them |

The data flow:

1. **Discovery.** On each scan the scanner runs a broadcast round on every site concurrently. A
   remote site asks its remote's *broadcast* call to send the remote's own `scan_air` request on
   the remote LAN and return the raw replies, each with its source address, for the collect
   window. Commander parses them with `parseReply`, which falls back to the source address when a
   reply omits `result.ip`, and tags each address with the site. For a remote site
   (`Costs.RepliesMatchSource`) it drops a reply whose `result.ip` is set and differs from its
   source address, so Commander and the remote agree on the device's address. On the tested
   firmware `result.ip` equalled the source in every reply observed on a LAN, so the rule drops
   nothing there; the ASIAIR's own hotspot is untested
   ([hardware notes §5.2](2026-10-17-asiair-hardware-notes.md)). Replies merge into
   discovery's device registry under the path rule (§6), and online, offline and forget behave as
   for local devices. A remote that is down contributes nothing, and its devices age out like
   powered-off ones.
2. **Cycle.** The worker resolves an endpoint (the device registry, then `lastip:`, then `Host`)
   and takes that endpoint's site for the cycle. The device factory builds an SMB device that
   dials through the site. The identity probe goes through the same site once the SMB session is
   open. Pull, restore and clock sync run as they do on the LAN; before a delete, the device re-hash asks the
   site for the digests; the last-known endpoint is stored in its string form.
3. **Liveness.** The supervisor heartbeats each remote with *hello* every 10 s, and each round
   trip updates the remote's latency estimate. It rebuilds the tailcat client under the rebuild
   rule (§8.1) and publishes the remote's state. A rig whose ASIAIR is powered off behind a live
   remote fails its dial with a `net.Error` and takes the ordinary offline path; a rig whose
   remote's tunnel is not up shows its remote's state instead (§10).

## 5. Pairing and trust

The remote is the tailcat server and Commander a tailcat client per remote. "Add a remote" mints
both identities, so both pins exist before the first packet, and Re-pair is the only key
rotation. Two-way messaging needs no dual roles: Commander keeps a long-lived remote-protocol
connection to each remote, and what the remote reports, its warnings included, rides its replies
to Commander's *hello*, so the remote reports without Commander serving anything.

**Add** is one write request through the guarded write path
([architecture §8.3](2026-10-17-architecture.md)). The first identity is the remote's **server
identity**: node private key, PSK, and a **fixed** DERP region chosen at pairing (§10), because an
"auto" region is re-picked on every start and changes the address. A retired relay region is therefore
remedied by a Re-pair. The second is **Commander's
client key for this remote**, one per remote, so re-pairing one remote never touches another.
Commander keeps the derived address (which contains the PSK) and its client private key in
`config.yaml`, and discards the remote's private key once the response is sent. The response
shows, **once**, a single opaque env value (`ASIAIR_REMOTE_PAIRING`) holding the remote's server
identity plus Commander's client public key. The value carries a format version; every version
from the first stays readable, because a remote's `.env` is never rewritten by an update, so a
later field (such as a self-hosted DERP override) needs no Re-pair.

Every remotes write (Add, Re-pair, Rename, Remove, the bandwidth cap) goes through
`/api/remotes` into the remote supervisor, which saves the config itself under the config guard.
`PUT /api/config` keeps the stored `remotes:` value whatever the request carries, so a Settings
save can neither drop nor overwrite a remote's keys; only `/api/remotes` writes them.

A config that holds a remote is saved at a bumped schema version (`model.RemotesSchemaVersion`),
so a binary that predates that schema version refuses it rather than silently dropping the keys; a config with no remote
keeps the unbumped version ([architecture §7.3](2026-10-17-architecture.md)). `Save` picks the
version from the content rather than stamping `CurrentSchemaVersion`, and `Load` reads either
version as it is, without migrating it or refreshing `.bak`. So running a binary that predates
the remotes schema version means removing every remote first, which the operator docs state.

The remote starts with `AllowedClients = [Commander's key]` and **refuses to start when the pin is
missing or malformed**, since an empty list admits everyone and a missing variable must fail
closed rather than open. Both directions are pinned from the first packet: Commander pins the
remote through the address (WireGuard's IK handshake authenticates the server's static key, and
the PSK binds the session), and the remote pins Commander through its allowlist. There is **no
first-connection key exchange and no TOFU window**.

| Action | Commander | Remote | Effect |
| --- | --- | --- | --- |
| Add | Mints both identities, keeps the address and its client key, shows the env value once | Started by the operator with that env | The card reads *Waiting for first connection* until the first *hello* succeeds |
| Re-pair ("Make new setup files" in the UI) | Mints fresh keys on both sides, replaces its stored address and client key, shows a fresh env value once | Keeps running on the old env and is no longer dialled | The old env stops working the moment the config applies; the card reads *Waiting for first connection* until the operator replaces the `.env` and restarts the container. Frame history is untouched (`TestRepairKeepsHistory`). |
| Rename | Changes the display name only | Nothing | None on the tunnel |
| Remove | Stops dialling at once, clears every `lastip:` key that names this site (like rig deletion's cleanup), and drops that site's discovery sightings. Site ids are never reused, so a late last-endpoint write for the removed site names an unknown site and fails closed. | Keeps running and is never dialled again | In-flight operations on its rigs fail as network errors and take the ordinary retry or deferral paths; the rigs then show generic Offline; the operator stops the container |

**Forwarding policy (least privilege).** The remote forwards TCP 445, TCP 4700 and UDP 4720 only,
and only to IPv4 addresses that answered its own `scan_air` broadcast. It holds that allowlist in
memory and writes nothing to disk, so after a restart it forwards nothing until its first
broadcast round. Commander runs that round on the first successful *hello* after every
(re)connect, while the site is still connecting: a remote site is **up** once its tunnel is
connected and that reconnect round has completed, so the empty allowlist raises no per-rig
offline alert. An address stays on the allowlist until the remote restarts, with no expiry, which
keeps `lastip:` resilient to broadcast outages, so a remote device behaves like a local one (the
local site allows everything).

An address joins the allowlist only when its reply's `result.ip` is empty or equals the reply's
UDP source address. The remote protocol obeys the same policy: *hash* refuses an address off the
allowlist, and *broadcast* sends the remote's own fixed `scan_air` request, never bytes Commander
supplies. The policy is enforced inside `OnTCPForward` and `OnUDPForward`, and the remote sets a
short `UDPIdleTimeout`. Even a stolen Commander client key cannot turn the remote into a general
exit node.

**Secrets.** Commander keeps its pairing secrets in `config.yaml` (and its `.bak`), wrapped like
the session secret and redacted on every GET ([architecture §7.4](2026-10-17-architecture.md)).
The env value appears only in the Add or Re-pair response, which is served `no-store`; like the
one-time first-run setup token, no later GET returns it. A rig's SMB credentials reach the remote
only inside a *hash* call (§7). The tunnel wrapper never prints the address, a key or the env
value, and the remote API routes log no request bodies (§12 invariant 9). The setup zip is a copy
of the pairing secret, so the operator docs say not to keep the downloaded zip in a chat, a
ticket or a synced folder.

#### Threat model and residual risks

The trusted-LAN premise ([overview §7](2026-10-17-overview.md); threat model in
[device-identity-and-media §5](2026-10-17-device-identity-and-media.md))
extends to the remote LAN: every device there is the operator's. The tunnel adds an authenticated,
encrypted transport and no trust of its own.

**A stolen remote env file.** Whoever holds it can stand up an impostor remote that Commander will
dial.

- The impostor can fake `scan_air` replies and SMB content, and fabricated frames would be
  archived like real ones, so path sanitisation is the guard: every device-supplied name reaching
  a destination path goes through `internal/sanitize`
  ([routing-and-filing §4.1](2026-10-17-routing-and-filing.md)).
- It **cannot** reach Commander's host or UI, because the client netstack accepts nothing.
- It **cannot** make Commander delete a frame that has neither a verified NAS copy nor the
  operator's cull decision. An impostor on the remote LAN gains nothing beyond the direct SMB
  access anyone on that LAN already has.
- It **can**, knowing a rig's guid (sent in clear in `scan_air` replies on the remote LAN), pass
  the identity probe and serve a listing that omits unpulled frames, so *Safe to power off* reads
  Yes while they remain on the real device ([delete-safety §5](2026-10-17-delete-safety.md)).
- It also collects the plaintext SMB password of every remote rig a *hash* call names, where
  Commander's own tunnelled sessions expose only an NTLMv2 challenge-response. ASIAIR SMB
  passwords are low-value device credentials, and the guest share needs none.

The remedy is Remove or Re-pair. The signals of a leak are the alert on another remote using this
pairing and the alert on a rejected foreign client key (§10); the card's last-contact time and
direct/relayed state come from the peer itself and prove nothing.

**A stolen Commander data directory.** `config.yaml` is plaintext off Windows, so whoever holds
it, or a backup of `/data`, reaches every remote's allowlisted ASIAIRs on TCP 445, TCP 4700 and
UDP 4720 from anywhere. The remedy is Re-pair of every remote, and the operator docs treat `/data`
backups as secret.

**Two Commanders on one pairing.** A remote pins exactly one Commander client key. Two Commanders
holding it (a copied config, a migration in progress, a test instance) both pass the pin, the
tunnel flaps between them, and both act on the same ASIAIR, which is the hazard two Commanders on
one LAN already pose. The operator docs say to Re-pair a remote before running a second instance
from a copy.

## 6. Device identity across paths

The guid binding, the cpuId fallback, the per-session identity confirmation and the frame keys
are path-independent ([device-identity-and-media §3–§4 and §9](2026-10-17-device-identity-and-media.md)).
Moving a rig between a direct and a remote path changes only the endpoint its next cycle resolves
to.

- **The last-known-endpoint hazard closes by construction.** The last-known endpoint
  ([device-identity-and-media §3.3](2026-10-17-device-identity-and-media.md)) is stored in its
  string form, which parses back to the same site and IP, so a remote-LAN IP cached for a rig is
  only ever dialled through its remote, and a bare legacy value only ever locally. Without that, a
  rig that moved could be dialled at its old IP on the wrong network, a different device could
  answer the probe, and the rig would raise a false Critical identity mismatch.
- **The identity probe uses the session's own path, inside the open session.** For a remote
  endpoint the unicast `scan_air` probe is a forwarded UDP flow through the same site as the open
  SMB session, so the guarantee of re-proving the guid at the exact address the session talks to
  holds unchanged. Over a tunnel the order is load-bearing: the SMB dial carries a freshly built
  client through its first handshake, which a lone datagram would not survive (§2.1). The probe is
  re-sent within its budget; one unanswered within that budget leaves the session unconfirmed,
  which fails closed.
- **A rig visible on two paths** (a remote tested at home, or two remotes seeing one rig) keeps
  its current path while that path is fresh. It switches only when another fresh path's minimum
  recent latency is less than half the current one's (`pathSwitchRatio` 2), or when the current
  path goes stale. Minimum rather than smoothed latency is compared so a rig's own transfer cannot
  inflate its path's figure. Paths of different kinds (local against tunnelled, direct against
  relayed) differ by multiples, while two of the same kind differ by tens of percent, which is not
  worth a move that restarts the rig's sessions. A ratio keeps the local site (latency 0) winning
  over any remote, so a rig brought home goes direct at its next local sighting. Safety does not
  depend on the rule, because every session re-probes identity on its own path.
- **Remote-found, unadopted devices** appear in "Found on your network" with the same pill, adopt
  through the same guid-binding flow, and have their shares listed through their site, because the
  device factory is site-aware. A discovered device carries the site it was seen through.
- **`Host`-bound legacy rigs** resolve locally only; such a rig is adopted by guid once a remote
  sees it.
- **The first-run setup wizard** lists only local devices: a remote can be added only from the
  Devices page, after setup has set the password.

## 7. The delete path and listing completeness

A frame's bytes cross the remote link once, for the pull. The pre-delete re-read of the device
original (about 100 MB per frame) is done by the remote over its own LAN, so only the digests cross
the link, unless the whole-file digest algorithms of the two sides differ. The remote reading the
file a second time from the ASIAIR is the accepted cost: it saves link bytes, not ASIAIR-link time.

Every other read that informs a delete, and the delete itself, run in Commander's smb1client
session, carried by the tunnel as a TCP byte stream:

| Check | Runs in | Over a remote |
| --- | --- | --- |
| Identity probe in the delete session | Commander | A forwarded UDP flow on the session's site |
| Removable-medium marker, `Stat`, size and ±2 s mtime compare | Commander's SMB session | TCP 445 through the tunnel |
| The device re-hash: a full fresh read of the device original | The session's site: Commander's SMB session on the local site; the remote's own SMB session on a remote site, unless the whole-file digest algorithms differ | Only the digests cross the link |
| NAS at-rest verify | Commander, local disk | Unchanged |
| The three-way compare (device digest against the device anchor, NAS digest against the archive anchor) | Commander | Unchanged |
| The culled branch's mandatory post-hash re-evaluation ([library-reconciliation §9.1](2026-10-17-library-reconciliation.md)) | Commander | Unchanged |
| Final re-`Stat` and delete | Commander's SMB session | TCP 445 through the tunnel |
| A delete whose reply was lost ([frame-lifecycle §9.4](2026-10-17-frame-lifecycle.md)) | Commander, next pass | Unchanged |

**How the three-way check stays intact.** The three-way compare, its anchors and every decision
stay in `deleteOne`, in Commander. Only where the device digest is measured sits behind the site
(`Site.MeasureDigests`): the local site runs the streaming read in Commander's session, and a remote
site calls its remote's *hash*, or streams the file through the tunnel in Commander's session when
the whole-file digest algorithms differ. The tunnel adds an authenticated, encrypted transport
under SMB and no logic of its own.

**The remote *hash* contract.**

- The remote exposes `hash(ip, share, path, user, password)` and refuses an `ip` not on its
  forwarding allowlist. It opens its **own** SMB session to that address, takes a unicast
  `scan_air` probe inside that session, re-stats, and streams the file fresh with no cache through
  `internal/header`'s digest code, which the remote role compiles. It returns the probed guid and
  cpuId, for Commander's identity comparison, with the whole-file digest, the data-unit digest,
  the size and the mtime.
- Commander sends the rig's SMB user and password with each *hash* call over the tunnel; the
  remote holds them only for that call and never logs or writes them.
- **The remote never receives the expected hash and never compares.** A remote that compares can
  turn a bug into a false *match*, the only direction that loses data. A remote that only measures
  can produce a false match only by reading bytes that really hash to the recorded digest.
- A *hash* reporting a different identity takes the foreign-identity handling: the rig goes to
  Error, refuses its deletes, and raises a Critical `identity_mismatch` alert. One reporting no
  identity is handled like the delete session's unconfirmed probe: it ends the slice and defers
  its deletes (fail closed), quietly. Both end the delete slice and are counted under their own
  delete-blocked reasons, `hash_identity_mismatch` and `hash_identity_unconfirmed`
  (`docs/metrics.md`). A transient error defers the frame.
- *hello* carries the whole-file and data-unit digest-algorithm versions. The three-way compare
  uses only the whole-file digest; the data-unit digest is only recorded, as a culled frame's
  exposure identity ([frame-lifecycle §11.3](2026-10-17-frame-lifecycle.md)). So on a data-unit
  version mismatch alone Commander still uses the remote's whole-file digest and skips that
  record, which a digest from another algorithm would corrupt. Only when the whole-file algorithm
  differs does Commander stream the file itself through the tunnel, as the local site does, rather
  than defer, losing the bandwidth saving.
- Each *hash* runs on its own tunnel connection and streams progress (bytes read) every few
  seconds, so *hello* never queues behind it. Commander applies the SMB stall rule to that
  progress: it gives up only when no progress arrives within the site's SMB budget (§8.1), with no
  whole-file deadline. The device re-hash passes the site the same stop check it uses on the LAN
  (Pause as well as the context), and cancelling the call stops the remote's read.

**How listing completeness stays intact.** smb1client (which reads every message
of a fragmented TRANS2 FIND; [hardware notes §9.2](2026-10-17-asiair-hardware-notes.md)) runs in
Commander. The tunnel is a reliable byte stream: if it drops mid-listing, smb1client sees a
connection error and the listing is marked incomplete. It never sees a shorter listing. SMB
not-found statuses come from the ASIAIR through Commander's own client, so the tunnel cannot turn
an outage into "source vanished" or "slot empty". Because a transport change under enumeration
needs an exact-count test ([delete-safety §5](2026-10-17-delete-safety.md)), the
`TestLargeDirectoryListsCompletely` equivalent runs through a real tailcat tunnel, with a
mid-listing drop that must surface as an error (AC6). Against a real ASIAIR, listings were
identical in exact count and names direct, tunnelled and relayed, and file digests matched.

Two concurrent SMB1 sessions from one source address on the ASIAIR (Commander's tunnelled session
holding the file open while the remote's session hashes it) run with no reset and identical
digests on the tested firmware ([hardware notes §9.1](2026-10-17-asiair-hardware-notes.md)). On a
firmware whose server drops one of them, the dropped session is a network error and fails closed.

**Accepted timing change.** The gap between the final re-`Stat` and the delete grows from a LAN
round trip to a WAN round trip (0.1–2 s). The window between the hash and the re-`Stat` (tens of
seconds) is already the larger exposure and is unchanged.

**Lost delete replies.** A delete whose reply is lost self-heals safely: the next pass's `Stat`
finds nothing and the vanished-source path re-proves the NAS copy
([frame-lifecycle §9.4](2026-10-17-frame-lifecycle.md)). Its reporting is wrong: it raises a Warn
alert, "could not delete source (still safe on NAS)", one per frame, never auto-acknowledged and
false when the delete landed; it records `source_vanished` instead of `device_deleted`; and for a
culled frame it raises a false "doubly-vanished" warning and skips the culled-frame resolution.
LAN Wi-Fi rigs have the same window; a remote link makes it more frequent. A Commander-side durable
"delete issued" record, written before the delete and consulted by the vanished-source path, would
fix the reporting for both; it is a reconciliation change and is not built. This residual is
listed in [delete-safety §8](2026-10-17-delete-safety.md).

## 8. Behaviour on an unreliable link

**What is durable:** nothing on the remote; its env file is the operator's. On Commander: the
remote profiles and their secrets in `config.yaml`; each remote's last successful *hello* time in
the store, refreshed at most once a minute while it keeps answering, and with it whether the
remote ever connected under any pairing (Re-pair keeps it); and everything already in `state.db`.
A Commander restart therefore neither resets *Unreachable since T* nor turns a down remote into
*Waiting*. Frames are keyed by rig, so an outage loses no history.

**What is idempotent:** the frame pipeline already handles every interruption a flaky link
produces, provided tunnel failures surface as network errors.

| Interruption | Behaviour |
| --- | --- |
| During a pull stream | Any stream error discards the staged temp file and retries next cycle; the next frame opened on the dropped session fails as a network error, never marked failed. gVisor's conns return `*net.OpError`, and the dial path wraps tailcat's own startup errors and bare deadlines the same way. |
| While opening a session | The cycle is refused and the rig goes Offline. |
| During the pre-delete re-hash | The delete is deferred. |
| A delete was sent and its reply lost | It self-heals next pass (§7, *Lost delete replies*). |
| No identity probe reply arrives within its budget | The session is unconfirmed and fails closed. |
| The remote restarts (watchtower, power), mid-delete included | Any of the above; the supervisor then rebuilds the client (§8.1). |

**Retries stay inside the call.** The idempotent remote calls (*hello*, *broadcast*, *hash*) and
the unicast identity probe retry with short backoff within their own deadline; for *hash*, that
deadline is the stall window since its last progress. Nothing is queued beyond the cycle that
asked, because a result acts on facts that go stale. There is **no pull resume**: an interrupted
frame is retried next cycle.

**Stalled progress.** A remote rig with three consecutive passes (`stallPasses`; about two minutes
at the shipped 45 s poll) in which its remote's tunnel was up and a pull or delete attempt failed
raises one Warn `remote_stalled` alert naming its remote, auto-acknowledged on the next success,
and its blocked deletes are counted. A delete waiting for an ordinary reason (the settle window, a
supersede in flight, unclassified, `source_changed_deferred`) never counts, so the alert never
fires while the remote is Reconnecting, Unreachable or Waiting. It covers a pre-delete *hash* that
keeps failing, which the transient-error branch alone would only log, and a link that flaps faster
than one frame's transfer.

### 8.1 Heartbeats, reconnects and rebuilds

**The rebuild rule.** A client never recovers from a server restart by itself (§2.1), so the
supervisor rebuilds it. Rebuilding tears down the netstack and every connection through it, so the
supervisor rebuilds a remote's client only when its heartbeat has failed **and** nothing has moved
through the tunnel (a transfer's bytes, a remote *hash*'s progress, a *hello*) for longer than the
site's SMB budget. A transfer still making progress is never killed by its own supervisor. A
rebuild restarts that quiet window, and while the remote is Unreachable the window doubles per
rebuild up to 10 minutes. Recovery after a remote restart takes about one SMB budget. During an
outage longer than the budget the rule also fires; that costs only idle connections, which the next
cycle reopens anyway.

**Timeouts follow the measured latency.** Each site reports a latency, and every timeout adds
margins from it instead of assuming LAN or WAN constants: each timeout states how many round trips
it covers and adds two latency margins per round trip (`netsite.RoundTrips`). The local site's
latency is 0, so local timeouts equal the plain LAN values. A remote's latency is smoothed with its
variance rather than taken from the last sample, and its minimum recent latency is kept separately
for path selection (§6). A timeout that may be a freshly built client's first exchange, the
identity probe's among them, adds the site's handshake allowance (5 s on a remote site, 0 locally).
Under a bandwidth cap, transfer timeouts also allow for a read's expected duration at the capped
rate, so a low cap does not trip the SMB budget.

**A remote's stall window is longer.** Latency margins do not cover an outage, during which no
bytes move at all, so a remote site's stall window is 60 s against the local site's 30 s
(`netsite.RemoteStallWindow`, `netsite.LocalStallWindow`); the SMB budget is the stall window plus
the latency margins (`netsite.SMBBudget`). A stalled flow resumes only at TCP's next retransmit
after the link returns, so 60 s rides out outages of roughly 30–40 s; longer ones cut the frame at
its read deadline as a `*net.OpError`, and it is retried next cycle. The same budget governs the
remote *hash*'s progress stall and the rebuild rule. It is a property the site reports, not a
local-versus-remote branch.

**The measurements behind these numbers.** A client stays disconnected after a remote restart
(still so after 300 s), and a rebuilt client reconnects about 3.5 s after the remote is back; the
address is stable across restarts. With both processes alive and the link paused for 10 s, 60 s
and 3 min, the same client, never rebuilt, answers again 0.01–4.9 s after the link returns. A
relayed path between two separate networks runs at a 60–133 ms round trip, with broadcast, the
identity probe, an SMB listing and a remote hash all working over it. A killed remote is restarted
by its restart policy and accepts a new client. A real network drop, a direct-path recovery and
outages beyond 3 minutes are unmeasured; they bound the evidence for the 60 s stall window and the
rebuild rule.

### 8.2 Logging

A remote's state transitions and a rig's path switches log once, at Info or Warn (Warn on a move
away from connected or waiting). Per-attempt events (heartbeat misses, rebuild attempts, broadcast
failures) log at Debug and are counted. Commander sends no broadcast while a remote's tunnel is
down (the reconnect round runs before the site is up, §5), and while a remote is Unreachable its
client rebuilds back off to the capped interval. Commander copies each remote warning it has not
seen before into its own log at Warn, with the call id that produced it. No log line carries a
pairing secret, an SMB credential or a *hash* request's arguments (§12 invariant 9).

Metrics are labelled by remote id, never by name, and a rig's remote is the `remote_rig_info` info
series rather than a site label on the per-rig series. The per-remote series are listed in
`docs/metrics.md`.

## 9. Transfer admission and bandwidth

- Each remote site has one transfer slot of its own, in place of the global concurrency cap
  (`max_concurrent_transfers`, [architecture §3.4](2026-10-17-architecture.md)), and a remote
  rig's deletes and restores take that slot as well as its pulls.
- NAS-only work (retarget) is admitted as local work whatever the rig's site.
- The disk-reserve check and its accounting of bytes in flight stay one gate shared by every rig
  on every site.
- `Acquire` receives the requester (the rig and its site), and a pluggable admission policy picks
  the slot; the shipped policy (`siteSlots`) is the global cap for local rigs, one slot per remote,
  and the shared disk reserve. Fairness-based balancing is not built; the policy seam leaves room
  for it.

An optional per-remote bandwidth cap, in MB/s (10^6 bytes), is set on the remote's card and is off
by default. Commander enforces it by rate-limiting its reads on every connection the site registry
dials through that remote site, so TCP backpressure caps the remote's uplink on average, with about
one window of burst per transfer. The local site is never throttled, and the remote is unaffected.
Schedules are not built.

## 10. What the UI shows

**The Devices page.** "Add a remote" sits beside "Add a rig", and "Your remotes" is a third section
beside "Your rigs" and "Found on your network".

**The Add dialog** says in one sentence what a remote is and that the setup guide walks through
installing it. The prerequisites (shell access to the remote PC, which every repair needs, and a
64-bit Linux PC with Docker) sit behind a "Requirements" disclosure beside Advanced. It asks for a
name, then says the setup files are a secret shown only this once and offers them as one zip: an
`asiair-remote` folder holding `.env` (stored readable by its owner only) and `docker-compose.yml`
under their real names, since a browser drops a downloaded file's leading dot. It shows the
pairing fingerprint and numbered steps: unzip the folder into the remote computer's home folder;
inside it run `docker compose up -d`; wait for *Connected*. The raw `.env` and compose file, each
with Copy, sit behind a collapsed "Show the files" disclosure. The dialog then hands over to the
remote's card. Under Advanced, it shows the relay region chosen by netcheck (tailcat's measurement
of the nearest DERP region) and lets the operator pick another.

**A remote's card** shows the name, its state (below), the remote's version, its uptime and
restart count, a pairing fingerprint (of the two public keys only, never the PSK; the remote logs
the same fingerprint at start), its latency, whether its path is direct or relayed, its optional
bandwidth cap, its recent warnings and errors, the rigs reached through it and, marked as such, the
rigs last reached through it, and the unadopted devices seen there, which lead to adopting them.
Its actions are Rename, Re-pair (named "Make new setup files" everywhere, confirmed under the title
"Make new setup files for *name*?": the setup files made before stop working right away, and the
remote stays offline until the fresh ones are installed) and Remove (confirmed, telling the operator
to stop the container).

The warnings list (broadcast failures, refused forwards with the target address and reason, pin
rejections) is a short in-memory list carried by *hello*, so "Connected but no rigs found" can be
diagnosed from home while the remote writes nothing to disk. A refused forward also shows on the
card of the rig at that address; a failed dial to an allowlisted ASIAIR that is powered off is that
rig's own offline state, not a remote warning.

The page shows Commander's own reachability of the relay network apart from any remote's state, so
Commander's internet being down does not read as a remote down: *Commander is online* or *Commander
can't reach the internet*. Commander reads its own path as down when no tunnel client has held a
relay connection for 30 s.

| Remote state | Derived from | Card reads |
| --- | --- | --- |
| Waiting for first connection | No successful *hello* since Add or Re-pair | *Waiting for first connection*, saying it changes to *Connected* about a minute after the remote starts, with the setup steps and a "Lost the setup files? Make new setup files" action (Re-pair). From about five minutes after the Add or Re-pair it adds "Still waiting? Check that the remote computer is online and that its setup files are the latest ones", the plain form of the mismatched-`.env` cause. |
| Connected, direct | Latest *hello* succeeded, the site is up, and the remote reports a direct path | *Connected (direct)*, with the latency |
| Connected, relayed | Latest *hello* succeeded, the site is up, and the remote reports a relayed path | *Connected (relayed, slower)*, with the latency |
| Reconnecting | Connected under this pairing before; site not up for less than the unreachable threshold | *Reconnecting (last seen N ago)* |
| Unreachable | As Reconnecting, past the threshold | *Unreachable since T*, naming a stopped remote, a down link, a mismatched `.env` or a recent update of either side as possible causes |
| Needs update | Connected, but *hello* reports a digest-algorithm version different from Commander's; nothing else triggers it | *Needs update (remote)* or *Needs update (Commander)*, naming the older side; dials continue, and deletes skip the data-unit record, or stream the hash through Commander when the whole-file algorithm differs (§7) |

Whatever its state, once the build date a remote's *hello* reports trails Commander's by more than
two weeks (watchtower checks daily, so that is two weeks of missed updates, far past a late or
skipped run), its card asks the operator to check its updater, showing both versions, and one Warn
`remote_build_stale` alert is raised.

How each remote state shows on the rigs reached through it:

| Remote state | Tunnel up? | Rig card | Alert |
| --- | --- | --- | --- |
| Connected (direct or relayed) | Yes | Normal; the rig's own states | The rig's own |
| Needs update | Yes (dials continue) | Normal; the remote's card names the side to update | None extra |
| Reconnecting | No | "Remote *X* reconnecting"; Safe to power off reads Unknown. A rig with a live session shows the worker's own state instead | None |
| Unreachable (past the threshold) | No | "Remote *X* unreachable since *T*"; Safe to power off reads Unknown. A rig with a live session shows the worker's own state instead | One remote-level `remote_unreachable` alert, raised only once no rig through it has a live session, auto-acknowledged on the next successful *hello*; no per-rig alerts |
| Waiting for first connection | No | "Waiting for remote *X*"; Safe to power off reads Unknown | None for a remote that never connected; a remote that has connected before, under any pairing (as after a Re-pair), raises the remote-level alert past the same threshold; the card hints at a mismatched `.env` |

The unreachable threshold is `unreachable_alert_after` (15 minutes by default), as for a LAN rig,
and a rig's own unreachable timer starts when its remote's tunnel comes back up. Where Safe to power
off reads Unknown because of its remote, the rig card also shows the last known state with its time
("Last known state as of 03:12: …, 14 frames not copied"), and a rig with a live session while its
remote reconnects keeps a "via *X* (reconnecting)" line. The verdict itself is defined in
[frame-lifecycle §14](2026-10-17-frame-lifecycle.md).

A rig's own offline state and alert apply only while its remote's tunnel is up. The rig's
association with its remote comes from its last resolved endpoint (the site-qualified `lastip:`),
so it survives a daytime power-off; after Remove, those rigs show generic Offline. The worker stays
the one writer of rig state and publishes the site and the bare IP separately on `RigStatus`. When
a remote changes state, the supervisor wakes the workers of the rigs reached through it, so each
writes its remote-down state at once rather than at its next cycle.

**Remote alerts.** The alert catalogue is [architecture §10](2026-10-17-architecture.md). The
remote categories are `remote_unreachable`, `remote_stalled` (§8), `remote_pairing` (an instance id
that returns after another was seen, without a restart: another remote is using this pairing),
`remote_foreign_key` and `remote_build_stale`. A remote-level alert carries no rig id and no remote
id, since the alert store keeps one schema: it is found again by its message prefix, which names
the remote's id, among the newest 500 unacknowledged alerts, so one buried deeper is not
auto-acknowledged and stays until the operator acknowledges it.

Direct versus relayed and the relay region come from the remote's per-peer `Server.Status()`,
carried in *hello*, because the client cannot name its own region. tailcat reports a rejected client
key only as one debug log line, so the warnings list shows rejections of *other* keys, parsed from
that line under the wrapper behaviour tests that guard tailcat bumps (§11). Each raises a Warn
`remote_foreign_key` alert as evidence the address leaked; it stays until that remote is re-paired
or the operator acknowledges it, never clearing on its own. Commander's own rejected key gets no
*hello* through, and its remote reads *Waiting for first connection* or *Unreachable* with the
mismatched-`.env` hint.

**Rig cards.** The rig's pill is driven by the snapshot's site field, because the dashboard card
reads only the live snapshot. For a remote rig the Address fact reads "*ip* via *remote name*". The
Windows `\\ip` shortcut is hidden for a remote rig and for a device found through a remote, because
the browser cannot reach that address, and the "Found on your network" heading reads correctly for a
device seen through a remote. The endpoint's string form never reaches the snapshot or the API. The
rig editor has **no** route field: the route is runtime state, not configuration.

## 11. Packaging and version skew

**One binary, two roles.** `asiair-commander remote` is the remote role, shipped in the same
multi-arch image (`linux/amd64`, `linux/arm64`, [architecture §19.2](2026-10-17-architecture.md))
and started with `command: ["remote"]`. The supported remote is a Raspberry Pi or a mini PC running
Docker on a 64-bit Linux OS. On Windows, the executable runs in the remote role as a documented
manual option, with no service and no updater. `linux/arm/v7` is not built.

The remote role reads the env, starts the tunnel server and serves the remote protocol. Its logic
lives in its own package (`internal/remote/role`), which the `remote` verb only calls; it has no
store, no web UI and no config file beyond the env, and it logs to stdout. It exits non-zero only
after 10 minutes with no DERP home-region connection, so `restart: unless-stopped` recovers a wedged
tunnel or a boot without network; a missing *hello* never makes it exit, so it does not restart
while Commander is off, removed or re-paired, and its in-memory warnings survive. The role serves a
loopback healthcheck (`asiair-commander remote healthcheck`): healthy means the tunnel server is up,
the pin is loaded and the DERP home region is connected, and never depends on Commander or an
ASIAIR. There is no separate binary, Dockerfile, image repository, Makefile target or release step,
so the remote role adds nothing that could disturb the build-then-tag order `release.yml` depends
on.
The costs: the image carries ASTAP and the SPA unused, and the binary carries tailcat's 8–11 MB,
which Commander pays anyway as a client.

**The compose file** (`deploy/remote/docker-compose.yml`, with `.env.example`) runs the remote with
host networking (needed for the LAN broadcast, and it helps hole-punching), `env_file: .env`,
`restart: unless-stopped` and **no named volume**, since the remote has no state (the image's
`VOLUME /data` still gives the container an anonymous volume, which the remote never writes). The
remote's container logs rotate. Watchtower runs upstream's moving tag, scoped to this compose
project, updates itself, removes superseded images, and updates daily at 14:00 in the remote's `TZ`,
so a restart for an update lands in daytime. The image tag is `${REMOTE_TAG:-latest}`, so a
rollback is one edit. The image is public on `ghcr.io`, so the remote PC pulls it with no sign-in.
The compose file and the env variable names (`ASIAIR_REMOTE_PAIRING`, `REMOTE_TAG`, `TZ`) are a
forward-compatible contract: every later image runs under every earlier copy of the compose file,
because the copy on a remote PC is never refreshed. Watchtower is not version-pinned, because a pin
in a compose file on a remote PC could never move.

**Version skew.** Watchtower keeps each remote on the latest release. The remote protocol is
versioned and additive: both sides support every remote-protocol version since the first, and a side
uses a later feature only when the peer's *hello* reports it, so a Commander updated by hand much
later never strands a remote. A whole-file digest-algorithm mismatch costs at most the remote hash's
bandwidth saving (§7). A data-unit-only mismatch also skips the data-unit digest record for culled
frames, whose device copy is the only source for that backfill
([frame-lifecycle §11.8](2026-10-17-frame-lifecycle.md)), so that record is lost for frames drained
during the mismatch. A remote that stops updating is flagged on its card (§10).

**Compatibility tests.** `build-test` runs golden-transcript compatibility tests on every PR
(`TestRemoteProtocolTranscripts`):

- For each protocol version the repo keeps recorded *hello*, *broadcast* and *hash* requests and
  replies (`internal/remote/proto/testdata/transcripts/`).
- Every build must accept every recorded version in both roles.
- Recorded transcripts and recorded pairing env values are immutable: `build-test` fails when
  `git diff --diff-filter=MD` against the PR's merge base, over the transcript directory and
  `internal/remote/tunnel/testdata/env/`, is non-empty. The env fixtures are what keep every
  pairing-format version since the first readable (§5).
- A protocol change adds a transcript.
- The broadcast reply's JSON form is shared with `netsite.Datagram`, whose tags are a frozen wire
  contract (`TestNetsiteWireForm`).

The real-binary **cross-version test** (`TestRemoteCrossVersion`), the current build against the
previous release tag's in both directions, runs only in the tailcat gate below. It builds the
previous tag from source in CI with no secret. A missing release tag or a failed build of the
previous release fails the gate rather than skipping; a previous tag that predates the remote
role's test hooks skips the test by name.

**tailcat wire-format breaks are the exception.** One binary cannot hold two tailcat or
`tailscale.com` versions, and the link never comes up to negotiate, so the tailcat gate leaves such
a bump for a human to decide when it ships. Under watchtower the usual order is remote first, so
Commander sees only *Unreachable* until it is updated. That release's notes and the remote section
of `DOCKER_README.md` carry a short ops guide:

> 1. Update Commander.
> 2. Remotes update themselves within a day; to update one at once, run
>    `docker compose pull && docker compose up -d` next to its compose file.
>
> Until both run this release the remote reads *Unreachable*; nothing is deleted while the link is
> down.

**The tailcat gate.** For `github.com/tailscale/tailcat` and `tailscale.com` bumps only,
`.github/workflows/dependabot-automerge.yml` first runs the tunnel package's tests (the real tunnel
through a local DERP relay, never public DERP; the wrapper's behaviour tests; and the cross-version
test) and enables auto-merge only if they pass. Otherwise it disarms auto-merge, labels the PR
`tailcat-gate-failed` and comments, and the PR waits for a human; a later push that passes clears
the label. Other PRs are unaffected. tailcat is 0.x, so 0.7 → 0.8 is a non-major bump there. Each
material change fails one leg:

| Material change | Fails |
| --- | --- |
| An API break | Compilation, since only the tunnel wrapper imports tailcat. |
| A behaviour change | The wrapper's behaviour tests, which pin what the design relies on (an empty allowlist admits everyone, the forwarding policy holds in the per-flow handlers, tunnel errors classify as network errors, and the rejected-key and DERP home-region log lines keep the forms the remote parses, since the library exposes neither through its API), and the real-tunnel tests. |
| A wire break | The cross-version test. |

The gate does not run the Docker integration suite in `test/integration`. The local test relay is
admitted only by the `remotetest` build tag; a production build refuses it
(`TestProductionBuildRefusesTestRelay`).

The remote role's broadcast goes through its local site, an injectable seam, and the integration
harness runs a `scan_air` responder per mapped device, so the remote's allowlist is populated in
CI without a real ASIAIR.

## 12. Invariants

Each invariant names the tests that guard it; `test/COVERAGE.md` maps them in full.

1. The content-hash three-way check runs in Commander, comparing a device digest computed from a
   fresh full read (in the session of whichever side measures it: Commander's on the local site;
   on a remote site the remote's own, or Commander's tunnelled session on a whole-file
   digest-algorithm mismatch) with the device anchor and a cache-bypassing NAS digest with the
   archive anchor, exactly as `deleteOne` does on the local site; no delete happens without it. —
   `TestRemoteHashContract`, `TestLocalSiteMeasuresWithTheLocalRead`, `TestRemotePullDeleteCycle`,
   `TestRemoteHashMatchesLocalRead`.
2. Device identity is re-proven by a live probe in every SMB session that can pull, delete or
   measure a pre-delete digest, sent through the same site as that session and only once the
   session is open. — `TestIdentityProbeRoutesThroughSessionSite`,
   `TestIdentityProbeRetriesWithinBudget`, `TestRemoteHashContract`.
3. Every listing that informs a verdict or a delete is produced by Commander's own smb1client; a
   transport failure surfaces as an error, never as a shorter listing. —
   `TestRemoteLargeDirectoryListsCompletely`, `TestRemoteListingDropIsError`,
   `TestTunnelErrorsAreNetworkErrors`.
4. An endpoint is dialled only on the site it names; a legacy bare address names the local site;
   an unknown or removed site, or one whose tunnel is not up, fails closed and is never dialled on
   the LAN. — `TestEndpointLegacyAddressIsLocal`, `TestSitesUnknownSiteNeverDialsLAN`,
   `TestLastKnownEndpointRoundTrip`, `TestRemoveRemoteClearsLastIP`, `TestForgetLastEndpointsOnSite`,
   `TestNewRequiresForgetEndpoints`, `TestRemoteDownRigState`.
5. The remote holds no delete authority and no durable state. It may measure a digest and probe
   identity; it never receives an expected digest or identity, and never compares either. —
   `TestRemoteRoleIsStateless`, `TestRemoteHashContract`, `TestServerRejectsMalformedRequests`.
6. Frames, media-id keys and every durable binding stay keyed by rig, never by site or address, so
   a rig keeps its history across paths. — `TestRigMovesBetweenSites`, `TestRepairKeepsHistory`.
7. A remote admits no client other than its pinned Commander key, and refuses to start without a
   pin. — `TestRemoteRefusesWithoutPin`, `TestRemoteVerbRefusesWithoutPin`,
   `TestRemoteRejectsUnpinnedClient`, `TestRejectedKeyLogLineForm`.
8. A remote forwards only TCP 445, TCP 4700 and UDP 4720, only to IPv4 addresses on its allowlist,
   which holds only addresses that answered its own `scan_air` broadcast; its *hash* call reaches
   only those addresses, and its *broadcast* call sends only its own fixed `scan_air` request. —
   `TestForwardPolicy`, `TestRemoteProtocolPolicy`, `TestForwardRefusesDeadTarget`.
9. Pairing secrets live only in the remote's env file and in Commander's permission-restricted
   configuration files (`config.yaml` and its `.bak`), wrapped like the session secret; no GET
   returns them after creation. A rig's SMB credentials reach the remote only inside a *hash* call
   and live there only for that call. No log line and no `Error()` text of a tunnel-wrapper error
   carries either, no *hash* request's arguments are logged, and the remote writes neither. —
   `TestConfigRedactRemotes`, `TestRemoteSecretsStayInRestrictedFiles`,
   `TestConfigRedactRemotesOverAPI`, `TestRemotesAPIRedactsSecrets`,
   `TestRemotesCreateAndRepairAreNoStore`, `TestRemotesRoutesLogNoBodies`,
   `TestTunnelLogsNoSecrets`, `TestRedactorCoversEncodings`,
   `TestHashRequestNeverFormatsCredentials`, `TestRemoteHashContract`,
   `TestRemoteRoleIsStateless`.
10. Commander accepts no inbound connection from a remote. — `TestClientAcceptsNoInbound`.
11. Grace, settle, defer counts and every other time-based gate run on Commander's clock and store.
    The remote's wall clock is untrusted: it reports ages and uptime, never timestamps (its build
    date excepted, which only the stale-build notice reads). — `TestRemoteHello`,
    `TestRemoteStateMachine`, `TestRemoteClockIsNotTrusted`, `TestVerdictsIgnoreSiteKind`.
12. Display may read a site's identity (the pill, "*ip* via *remote*"), and admission, latency and
    rate limiting read its costs; no identity, listing, pull or delete verdict branches on whether
    a site is local or remote. — `TestVerdictsIgnoreSiteKind`, which proves the paths its scenario
    drives; `TestTimeoutsScaleWithSiteLatency`, `TestAdmissionPolicyPerRemoteSlot` and
    `TestSiteRateLimit` pin the cost-side branches, and review of any change to verdict code (AC14)
    covers the rest.

## 13. Acceptance criteria

Each criterion names the test that proves it and the invariant or section it checks. Tests marked
*integration* run under `//go:build integration` (the real-tunnel ones also under `remotetest`)
against the Samba NT1 container through a real tailcat tunnel with a local DERP relay.

1. A legacy bare IP or hostname, from `lastip:`, `Host`, a fixture or the demo resolver, parses as
   the local site and dials as a bare address always has; an endpoint's stored string form parses
   back to the same site and IP; and the local site's stored form is the bare address —
   `TestEndpointLegacyAddressIsLocal` (§12 invariant 4).
2. An endpoint naming an unknown or removed site, or a site whose tunnel is not up, fails its dial
   with a `net.Error` and opens no socket on the LAN — `TestSitesUnknownSiteNeverDialsLAN` (§12
   invariant 4).
3. A remote's broadcast replies are tagged with its site and merge into discovery's device
   registry, and one whose `result.ip` is set and differs from its source address is dropped; a
   fresh current path is kept, and another fresh path takes over only when clearly lower in latency
   or when the current one goes stale — `TestScannerFansOutAndTagsSites`,
   `TestPathPrecedenceHysteresis` (§4, §6).
4. The identity probe for a remote endpoint travels through the same site as the open SMB session,
   is sent only once that session is open, is re-sent within its budget, and a probe unanswered
   within that budget leaves the session unconfirmed — `TestIdentityProbeRoutesThroughSessionSite`,
   `TestIdentityProbeRetriesWithinBudget` (§12 invariant 2).
5. Tunnel dial and stream failures classify as network errors (`net.Error`,
   `smb1.IsNetworkError`), and `SetDeadline` aborts a stalled read —
   `TestTunnelErrorsAreNetworkErrors` (§2.1, §8).
6. Through a real tailcat tunnel against the Samba NT1 container, a directory large enough to
   fragment its TRANS2 FIND lists with the exact count, and a link drop mid-listing surfaces as an
   error with the listing marked incomplete — `TestRemoteLargeDirectoryListsCompletely` and
   `TestRemoteListingDropIsError` (integration) (§12 invariant 3).
7. Through the same tunnel, a full pull → verify → delete cycle, with the pre-delete digests
   measured by the remote, removes every eligible original exactly once with every NAS copy
   re-hashing to its record; a drop mid-pull leaves no failed row, for the frame in flight or the
   next one opened on that session; a remote restart is recovered by the supervisor; and a failing
   heartbeat does not rebuild a client whose connections are still moving bytes or whose remote
   *hash* still reports progress — `TestRemotePullDeleteCycle`, `TestRemoteMidPullDrop`,
   `TestRemoteRestartRecovers`, `TestRebuildSparesLiveTransfer` (integration) (§12 invariant 1;
   §8).
8. The remote refuses to start without a well-formed pinned client key, and a client presenting
   any other key is never admitted — `TestRemoteRefusesWithoutPin`,
   `TestRemoteRejectsUnpinnedClient` (§12 invariant 7).
9. The remote forwards only TCP 445, TCP 4700 and UDP 4720, only to addresses on its allowlist,
   nothing before its first broadcast round after a start, and refuses every other target in its
   per-flow handlers; an address joins the allowlist only when its reply's `result.ip` is empty or
   equals the source address, and stays on it until the remote restarts; *hash* refuses an address
   off the allowlist, and *broadcast* sends only the remote's own request — `TestForwardPolicy`,
   `TestRemoteProtocolPolicy` (§12 invariant 8).
10. No GET (`/api/remotes`, `/api/config`) returns the address, a private key or the env value;
    config redaction round-trips the wrapped fields; neither the tunnel wrapper's logs nor the
    `Error()` text of any error it returns, tailcat startup failures included, contains them; no
    log line around a *hash* call contains the SMB password — `TestRemotesAPIRedactsSecrets`,
    `TestConfigRedactRemotes`, `TestTunnelLogsNoSecrets` (§12 invariant 9).
11. Removing a remote stops dialling it at once, clears every `lastip:` key naming its site and
    drops its discovery sightings, and its rigs then show generic Offline —
    `TestRemoveRemoteClearsLastIP` (§5).
12. The remote card's state, latency and build-age notice, the rig's pill and Address fact, and the
    rig card for each remote state are pure derivations in `web/src/util/remote.ts` and
    `web/src/util/remoteRig.ts` with vitests beside them, and `tools/seedremote` screenshots of the
    Devices and Dashboard surfaces on both device profiles show every remote state (§10).
13. `go vet -tags integration ./...` and a `-tags demo` build pass with every caller on the typed
    endpoint, and `web.Supervisor` keeps its signature (§3).
14. Review of any change to the delete path or the site layer confirms that the three-way
    comparison and `deleteOne`'s decision logic are independent of the site, that only where the
    device digest is measured sits behind the site (the local site streaming exactly as the local
    re-hash does), and that the content-hash three-way check is intact
    ([delete-safety §9](2026-10-17-delete-safety.md)) (§12 invariant 1).
15. For a tailcat or `tailscale.com` bump, `dependabot-automerge.yml` runs the gate tests before
    enabling auto-merge: a bump that breaks the API, the wrapper's behaviour tests, the real-tunnel
    tests or the cross-version test is left with a comment and does not auto-merge, a compatible
    bump auto-merges, and other PRs are unaffected by the gate — checked in review of
    `.github/workflows/dependabot-automerge.yml` (§11).
16. A rig adopted directly and later reached through a remote, and back again, keeps its profile,
    frame history and media-id keys, and its card switches pill and Address fact with no
    re-adoption — `TestRigMovesBetweenSites` (§12 invariant 6).
17. The remote role keeps no durable state and makes no delete decision: its logic lives in its own
    package, which the `remote` verb in `cmd/asiair-commander` only calls; that package imports
    neither `internal/store` nor `internal/rig` (it compiles `internal/header` for the digests) and
    writes no file — `TestRemoteRoleIsStateless`, and review (§12 invariants 5 and 11).
18. A connection attempted from the remote's side toward Commander's tunnel address is refused —
    `TestClientAcceptsNoInbound` (§12 invariant 10).
19. Through a real tailcat tunnel, a dial to a powered-off device behind a live remote fails rather
    than connects, so the reachability probe reads unreachable and the rig takes the ordinary
    offline path and unreachable alert — `TestForwardRefusesDeadTarget` (integration, with a
    hermetic twin in the tunnel package) (§2.1).
20. The remote *hash* returns the guid and cpuId probed in its own session with the same digests
    the local re-hash computes for the same file, never receives an expected digest or identity,
    refuses an address off the allowlist, stops reading when the call is cancelled, and never logs
    or writes the credentials it carries; a probed identity that differs takes the foreign-identity
    handling (Error, Critical `identity_mismatch`) and a missing one ends the slice and defers, as
    an unconfirmed delete-session probe does, each under its own delete-blocked reason; on a
    data-unit digest-version mismatch Commander uses the remote's whole-file digest and records no
    data-unit digest, and on a whole-file mismatch streams the hash itself through the tunnel;
    Commander gives up a *hash* when no progress arrives within the SMB budget and keeps waiting
    while progress arrives, and *hello* succeeds while a *hash* runs — `TestRemoteHashContract`
    (§12 invariants 5 and 9).
21. Every timeout adds its stated number of round-trip margins from the site's latency; the local
    site's latency is 0, so local timeouts equal the plain LAN values —
    `TestTimeoutsScaleWithSiteLatency` (§8.1).
22. Each remote site admits one transfer at a time, pull, delete or restore, outside the global
    cap, while every rig's transfers count against the one disk reserve; with a bandwidth cap set,
    reads on a remote site's connections average no more than the cap, and the local site is never
    throttled; a remote rig's retarget is admitted as local work —
    `TestAdmissionPolicyPerRemoteSlot`, `TestSiteRateLimit` (§9).
23. For each remote state, a rig reached through it shows the rig card and raises the alerts §10
    gives for that remote state, and a rig's own offline state and alert apply only while its
    remote's tunnel is up — `TestRemoteDownRigState` (§10).
24. On every PR, `build-test` accepts every recorded protocol version's golden transcript in both
    roles and fails when a recorded transcript is modified or deleted relative to the PR's merge
    base; in the tailcat gate, the real-binary cross-version test, building the previous tag from
    source, passes in both directions — `TestRemoteProtocolTranscripts`, `TestRemoteCrossVersion`
    (§11).
25. A config with no remote saves at the unbumped schema version and round-trips; one with a remote
    saves at the bumped version, which a binary that predates it refuses —
    `TestConfigSchemaFollowsRemotes` (§5).
26. With the remote's uplink dropped for longer than its exit window and then restored, the remote
    exits, is restarted by its restart policy and reconnects with no human action, while with
    Commander stopped and the uplink up it keeps running past the exit window —
    `TestRemoteRecoversFromUplinkDrop`, `TestRemoteOutlivesAbsentCommander` (integration) (§11).
27. Each of the stalled-progress, another-remote-using-this-pairing, rejected-foreign-key and
    stale-build Warn alerts fires on its condition and auto-acknowledges when the condition clears,
    except the rejected-foreign-key alert, which clears only on Re-pair or manual acknowledgement —
    `TestRemoteStalledProgressAlert`, `TestPairingReuseAlert`, `TestRejectedForeignKeyAlert`,
    `TestStaleBuildAlert` (§8, §10).

## 14. Rejected alternatives

**Commander as the tailcat server**, with the remote dialling in. A tailcat server cannot open
connections, and Commander must open SMB and control sessions. Keeping Commander as the server
needs a reverse-multiplex protocol (the remote dials in, Commander asks it to open streams) with its
own liveness layer, and exposes Commander's host to inbound flows. The paste-into-`.env` experience
is the same either way.

**Trust on first use**, where the remote presents its key on first connection and both sides then
lock the peer's key. Pinning a pre-known key works on the released tailcat; TOFU needs the unreleased
`AllowClient` hook and still has a first-come window in which whoever presents the address first
wins. Commander mints both identities, so both pins exist before the first packet.

**In-band key rotation** authenticated by the existing key. tailcat has no rotation anywhere, so
this would be a key-exchange protocol of Commander's own, with a half-rotated lockout as its failure
mode, for a gain WireGuard mostly already gives: each session has forward secrecy, so rotating a
static key helps only after a key file is stolen, and then the thief holds the current key and can
race any rotation it authenticates. Re-pairing mints keys the thief never saw.

**One Commander key for every remote.** One client key per remote, beside each remote's own server
identity, means re-pairing or removing one remote never touches another.

**Remote-side verify-and-delete** against an expected checksum Commander sends, with the remote
logging its results in a durable message log (JSONL or SQLite) for an unreliable link. Rejected:

1. It cannot honour the culled frame's veto: a Restore click, a reappearing NAS copy, or an opened
   re-file or supersede must cancel *between hash and delete*
   ([library-reconciliation §9.1](2026-10-17-library-reconciliation.md)), and that veto would have
   to reach the remote mid-hash over the unreliable link.
2. A queued intent authorises on facts that go stale while disconnected; in Commander the facts and
   the delete are seconds apart on one goroutine.
3. It places delete authority in a binary watchtower updates on its own schedule.
4. It needs everything the stateless remote avoids: durable state, fsync ordering, replay,
   de-duplication and volume loss.

The outcome a remote log would recover, a delete whose reply was lost, is already recovered safely
by the vanished-source path; only its bookkeeping and alerting are wrong, and the record that would
fix them belongs on Commander's side (§7).

**Native binaries for every platform and a remote installer.** Commander's releases ship only the
Windows executable and installer besides the image, so per-platform binaries would be extra release
work for a role whose main deployment is the container. The same binary runs the `remote` role.

**A separate remote binary and image**, with the identity probe as a remote call. The probe call is
unnecessary, because the ASIAIR's unicast reply comes back through a forwarded UDP flow, and a
separate binary doubles the release surface.

**tsnet as the transport.** Tailscale's embeddable node is mature, with ACLs and key rotation, but
every user would need a Tailscale account.

**A "routed site" mode** for operators who already run Tailscale, ZeroTier or WireGuard to the
remote LAN, where Commander dials device IPs over that VPN like a local site and the remote runs only
the broadcast helper. Not built; the site layer admits it as one more `Site` implementation.

**The tunnel alone, with the remote hash later.** It would leave the delete-safety model untouched,
and a remote hash saves nothing when the ASIAIR's Wi-Fi rather than the uplink is the constraint.
Rejected because a slow uplink would carry every frame twice; the remote reading the file twice over
its own LAN is acceptable.

**A canonical string-encoded endpoint** carried wherever an address string flows, with no signature
changes, relying on an encoded string failing `net.ParseIP`, dialling and DNS on every local path.
Rejected for a typed `Endpoint{Site, IP}` at the Go boundaries and one site abstraction with the
local site as a null object, so the compiler finds every consumer and no code reasons about local
versus remote. The string form exists only for storage (§3).

**Direct always wins while fresh; between remotes, the freshest sighting wins.** It never flaps
while a LAN sighting is fresh, but needs two rules. One rule for every pair of paths (keep the
current path while fresh, switch on clearly lower latency) does the same, because the local site's
latency of 0 still sends a rig brought home direct.

**Forwarding to every address in the remote's attached private subnets**, excluding the remote PC
and its virtual bridges. It matches a shared home LAN and reaches a device the remote has not yet
seen. Rejected for forwarding only to addresses that answered the remote's own `scan_air`, which
leaves out the remote PC and its bridges by construction, at the cost of forwarding nothing until
the first broadcast after a restart.

**Each side speaking the current and the previous remote protocol**, so either may be one version
ahead and only Commander is ever updated by hand. Rejected because watchtower keeps the remote
current; supporting every version since the first, and using a later feature only when the peer
reports it, means a Commander updated much later never strands a remote.

**Hard-coded LAN and WAN timeouts**, with a warm tunnel or a longer probe budget for remote
endpoints. Fixed constants fail both ways: the 3 s unicast probe budget can lose to a cold
tunnel's first handshake, and the 30 s SMB stall window cuts transfers across outages. Rejected
for a per-site latency added to every timeout, 0 locally and measured for a remote, plus per-site
stall windows, with no LAN-versus-WAN constants in the callers.

**Excluding tailcat and `tailscale.com` from Dependabot auto-merge.** tailcat is 0.x, so 0.7 → 0.8
counts as non-major and would auto-merge a wire break into deployed remotes; excluding it entirely
would leave every compatible bump for a human. The tailcat gate (§11) lets compatible bumps land on
their own and holds the rest.

**The cross-version test in `release.yml`**, before tagging. A protocol break would surface only
after its PR merged and would hold every daily release until fixed; the golden transcripts catch it
on the PR itself.

**The real-binary cross-version test in `build-test`.** Every PR would pay, in billed CI minutes,
for building the previous tag and running a real tunnel, to guard a protocol the recorded
transcripts already guard hermetically.

**A link-format hint on the remote's card**, naming a tailcat format mismatch as a possible cause
when the link fails. Judged fragile; the tailcat gate and the ops guide handle a wire break.

## 15. Non-goals

- **The remote site's timezone.** A remote rig's observing night uses Commander's host timezone
  ([routing-and-filing §6.1](2026-10-17-routing-and-filing.md)), so for a site several hours east
  or west the noon-to-noon boundary can fall mid-session and split one night across two night
  folders. The operator docs state the limitation. A per-rig timezone setting, which would also
  help a rig taken on a trip, is its own change; the problem is not specific to remotes.
- **Overlapping subnets.** The home LAN and a remote site sharing one subnet (both
  `192.168.1.0/24`) is not an edge: every endpoint, `lastip:` included, is site-qualified.
- **Per-interface forwarding configuration** for a remote PC with several interfaces or VLANs. The
  remote forwards only to addresses that answered its own `scan_air` broadcast.
- **Pull resume** across a flapping link. Timeouts follow the remote's measured latency, and an
  interrupted frame is retried next cycle.
- **NAT traversal beyond relaying.** A remote behind symmetric or carrier-grade NAT relays through
  DERP, and its card says so.
- **A self-hosted DERP relay.** The remote relies on Tailscale's public DERP servers; the env
  value's format version leaves room for a self-hosted relay field without a Re-pair.
- **The Windows Firewall prompt** that may appear when Commander's tailcat client first binds UDP
  (the Windows service reaches *Connected* with no firewall rule and no prompt), and **Commander in
  a Docker bridge network**, which adds a NAT layer in front of the tunnel (a bridge network
  connects directly). Both are documentation notes (the docs recommend host networking), not
  design edges.
- **Several Commanders on one remote** (§5).
- **A drain-slower-than-capture alert or a minimum link speed.** The device-disk-low alert fires for
  a remote rig as for a local one, and the operator docs state the throughput a remote link needs
  to keep up with a night's capture.
- **Per-remote metrics in this doc.** They follow the metrics conventions
  ([architecture §17](2026-10-17-architecture.md)) and are listed in `docs/metrics.md`.
- **Fairness-based admission, bandwidth schedules and `linux/arm/v7` images** (§9, §11).
