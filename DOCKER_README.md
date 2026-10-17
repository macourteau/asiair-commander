# ASIAIR Commander

ASIAIR Commander watches ZWO ASIAIR astrophotography rigs over SMB and files
their captured frames onto NAS storage, with a web UI for setup, monitoring and
history. Frames are organized by configurable path templates. Source, issues,
and releases: <https://github.com/macourteau/asiair-commander>; the project
overview, the Windows installer and building from source are in the
[README](README.md).

This page is the full guide to running the container image
`ghcr.io/macourteau/asiair-commander`.

## Tags

| Tag | Meaning |
| --- | --- |
| `latest` | Most recent release |
| `X.Y.Z` | Exact release, matching the GitHub release tags |

Images are multi-arch: `linux/amd64` and `linux/arm64` (Raspberry Pi 4/5,
ARM NAS).

## Quick start

```sh
docker run -d --name asiair-commander \
  --network host \
  -v asiair-data:/data \
  -v /mnt/nas/astro:/captures \
  ghcr.io/macourteau/asiair-commander:latest
```

Then browse `http://<host>:8686` from any machine on your network — no tunnel,
no shell on the host. The wizard opens with a **setup token** field; the token
is printed once at startup, so read it with:

```sh
docker logs asiair-commander
```

Paste it in, then complete the wizard (set a password, pick capture roots under
`/captures`, list the addresses you will reach Commander at, adopt a rig). The
token is what proves you can reach the machine Commander runs on, so it is
required from every browser — including one on the host itself. It is minted
fresh on every start and stops working the moment setup completes; if you copied
it before a restart, read the log again.

**Host networking is strongly recommended.** Rig discovery uses a UDP
broadcast (`scan_air`, port 4720) that cannot cross Docker's bridge NAT — with
the default bridge network the device scan will not find your ASIAIR. See
[Networking](#networking).

## Deployment scenarios

### Plain `docker run`

Host networking (recommended — discovery works, web UI on the host's `:8686`):

```sh
docker run -d --name asiair-commander \
  --network host \
  --restart unless-stopped \
  -e TZ=America/Toronto \
  -v asiair-data:/data \
  -v /mnt/nas/astro:/captures \
  ghcr.io/macourteau/asiair-commander:latest
```

Bridge networking (web UI only reachable via the published port; the device
scan cannot see the LAN — only workable if your rigs were already adopted and
are reachable by routed unicast):

```sh
docker run -d --name asiair-commander \
  -p 8686:8686 \
  -p 8687:8687 \
  --restart unless-stopped \
  -v asiair-data:/data \
  -v /mnt/nas/astro:/captures \
  ghcr.io/macourteau/asiair-commander:latest
```

### docker-compose

```yaml
services:
  asiair-commander:
    image: ghcr.io/macourteau/asiair-commander:latest
    container_name: asiair-commander
    network_mode: host        # required for UDP broadcast rig discovery
    restart: unless-stopped
    environment:
      TZ: America/Toronto
    volumes:
      - asiair-data:/data     # config.yaml, state.db, logs
      - /mnt/nas/astro:/captures

volumes:
  asiair-data:
```

### Kubernetes

Single replica with `Recreate` — the state database is SQLite and must never
see two writers. `hostNetwork: true` is needed for rig discovery, which also
pins the pod to a node on the same L2 network as the rigs.

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: asiair-commander
spec:
  replicas: 1
  strategy:
    type: Recreate
  selector:
    matchLabels:
      app: asiair-commander
  template:
    metadata:
      labels:
        app: asiair-commander
    spec:
      hostNetwork: true            # UDP broadcast discovery + SMB to rigs
      nodeSelector:
        kubernetes.io/hostname: nas-node   # node on the rigs' LAN
      containers:
        - name: asiair-commander
          image: ghcr.io/macourteau/asiair-commander:latest
          env:
            - name: TZ
              value: America/Toronto
          ports:
            - containerPort: 8686
            - containerPort: 8687
          volumeMounts:
            - name: data
              mountPath: /data
            - name: captures
              mountPath: /captures
          livenessProbe:
            httpGet:
              path: /healthz
              port: 8686
            initialDelaySeconds: 10
            periodSeconds: 30
      volumes:
        - name: data
          persistentVolumeClaim:
            claimName: asiair-commander-data
        - name: captures
          hostPath:                 # or a PVC backed by your NAS storage
            path: /mnt/nas/astro
            type: Directory
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: asiair-commander-data
spec:
  accessModes: ["ReadWriteOnce"]
  resources:
    requests:
      storage: 1Gi
```

With `hostNetwork: true` the UI is on the node's `:8686`; no Service is
required. If you drop host networking (accepting that discovery will not
work), add a `Service`/`Ingress` in front of port 8686 instead.

## Configuration

All operational configuration lives in `/data/config.yaml` and is managed
through the web UI — there is no environment-variable configuration beyond the
table below.

- **First-run setup** happens in the web wizard, from any browser on your
  network, and is gated on the one-time token printed in the startup log
  (`docker logs <container>`, or `kubectl logs <pod>`). The wizard's first field
  asks for it; a scripted setup sends the same value as an `X-Setup-Token`
  header on `POST /api/setup`. The token is also written to
  `/data/setup-token` — line 1 is the URL the service bound, line 2 is the token
  — and both the file and the in-memory value are discarded once setup
  completes.
- **Addresses** you will use to reach Commander are listed in the wizard and
  editable afterwards in Settings. Commander answers only to the addresses on
  that list, which is what stops a malicious web page from talking to it behind
  your back. The address you completed setup from is filled in for you; add the
  host's name, its IP, or any domain you put in front of it. An IP entry works
  but is fragile — a DHCP change breaks it, so prefer a name.
- **Capture roots** (Light, Calibration, Staging, Logs) chosen in the wizard
  must live under a mounted volume (e.g. `/captures/...`), and Staging must be
  on the same filesystem as the Light/Calibration destinations so completed
  transfers are atomic renames.
- **Web port** defaults to `8686` (`web_port` in `config.yaml`). The same UI is
  also served over HTTPS on the next port up (`8687`), so a reverse proxy or
  tunnel can encrypt the hop to Commander. The certificate is **self-signed** —
  no client trusts it, so disable certificate verification at the proxy. If you
  change `web_port` under bridge networking, adjust both published ports to match.
- **Uploads through a reverse proxy.** The File browser's Upload sends the whole
  file to Commander in one request (`POST /api/files/upload/stage`) before
  anything is written to the ASIAIR, and Commander holds it under
  `/data/upload-staging/` until the upload dialog closes. A proxy in front of
  Commander must let that body through: nginx refuses anything over its default
  `client_max_body_size` of 1 MB with `413`, so raise it (or set it to `0` for no
  limit) on Commander's location, and turn off request buffering
  (`proxy_request_buffering off;`) so the browser's progress bar tracks what
  Commander has actually received rather than the proxy's spool. Other proxies
  have equivalent body-size and buffering settings.

### Environment variables

| Variable | Default | Purpose |
| --- | --- | --- |
| `ASIAIR_DEBUG` | unset | Any non-empty value enables debug logging (same as the `-debug` flag). |
| `TZ` | `UTC` | IANA time zone; affects log timestamps and the date-based `<YEAR>/<MONTH>/<DAY>` path-template variables. |

### Ports

| Port | Direction | Purpose |
| --- | --- | --- |
| `8686/tcp` | inbound | Web UI, API, `/healthz`, `/metrics` |
| `8687/tcp` | inbound | The same, over HTTPS with a self-signed certificate |
| `4720/udp` | outbound broadcast | `scan_air` rig discovery (replies return to an ephemeral port) |
| `445/tcp` | outbound | SMB to the ASIAIR rigs' file shares |

### Volumes

| Path | Recommended mount | Contents |
| --- | --- | --- |
| `/data` | Named volume or dedicated host dir | `config.yaml`, `state.db` (SQLite), rotating logs under `logs/`, and `setup-token` while first-run setup is still pending |
| `/captures` (convention) | Your NAS astro share/dataset | Destination for filed FITS frames; the wizard's capture roots must point under it |

`/data` must persist across container replacement — it holds the rig bindings,
transfer history, and your settings. Logs also stream to stdout, so
`docker logs` works regardless.

## Networking

- **Discovery is UDP broadcast.** The scan sends `scan_air` datagrams to
  `255.255.255.255:4720` (and each interface's directed broadcast). Docker's
  bridge network cannot deliver these to your LAN, so rig discovery — including
  adopting a rig in the first place — requires `--network host` /
  `network_mode: host` / `hostNetwork: true` on a Linux host attached to the
  same L2 network as the rigs.
- Docker Desktop on macOS/Windows runs containers inside a VM; even "host"
  networking there does not reach your physical LAN's broadcast domain. Deploy
  on a Linux host for real use.
- File transfer is outbound SMB (TCP 445) to the rig's address and works under
  any network mode with a route to the rig.
- `/metrics` (Prometheus) is loopback-only unless `web.metrics_expose_lan` is
  enabled in `config.yaml`.

## User and permissions

The container runs as the unprivileged `nonroot` user (uid/gid `65532`). Both
`/data` and your captures mount must be writable by that uid, or run with an
explicit `--user` matching the owner of those directories, e.g.:

```sh
docker run --user 1000:1000 -v /srv/asiair/data:/data ... 
```

(The wizard's root-writability check will tell you immediately if the captures
mount isn't writable.)

## Health and monitoring

- `GET /healthz` returns `200 OK` when the service is up (unauthenticated).
- The image ships a `HEALTHCHECK` that drives the binary's own `healthcheck`
  verb against the configured port, so `docker ps` shows `healthy`/`unhealthy`.
- Prometheus metrics at `GET /metrics` (loopback-only by default, see above);
  every metric is described in [docs/metrics.md](docs/metrics.md).
- Logs: stdout (`docker logs`) and rotating files under `/data/logs/`.

## Remote sites

A **remote** reaches ASIAIRs on another network, such as a second observing
site, without a VPN or an open port. A small always-on machine at the remote
site runs the same image in the `remote` role; Commander (at home, next to the
NAS) dials it through an encrypted tunnel and pulls frames as it does from a
local rig. The remote keeps no state and has no web UI: Commander makes every
decision, and the remote forwards traffic to the ASIAIRs on its own LAN.

Add a remote from Commander's web UI (**+ Add a remote**). Commander shows a
one-time `.env` value and the compose file, and **Download setup files (.zip)**
saves both as `asiair-remote.zip`: an `asiair-remote` folder holding `.env` and
`docker-compose.yml`. Copy that folder to the remote machine.

### What a remote needs

- **Shell access to the remote computer** (SSH, remote desktop or a visit):
  setting up and every later fix is done there.
- **A 64-bit Linux OS running Docker**, on a Raspberry Pi 4/5 (64-bit
  Raspberry Pi OS) or a mini PC (`linux/amd64` or `linux/arm64`). A 32-bit OS
  cannot pull the image. On 32-bit Raspberry Pi OS the pull fails with
  `no matching manifest for linux/arm/v7 in the manifest list entries`;
  reinstall a 64-bit OS.
- **Internet access to Tailscale's public DERP relays.** Both ends find each
  other through them, and when a direct path cannot be punched (many carrier
  and satellite connections sit behind CGNAT) every frame travels through the
  relay. The public relays are rate-limited, carry no service guarantee and may
  be withdrawn.
- **Enough uplink at the remote site.** As a guide, a direct path moves about 7 MB/s
  and a relayed one about 1 MB/s. A night of capture is 23 to 70 GB, so over a
  relayed path the pull takes roughly 6 to 18 hours, and only the light end
  keeps up with the night. To drain a night in an 8-hour night, the remote
  site's upload needs about 6.4 Mbit/s for 23 GB and about 19 Mbit/s for 70 GB.
  Commander does not declare a minimum link speed. A remote rig's drain that
  falls behind its capture shows up as the *device disk low* alert,
  as for a local rig. A per-remote bandwidth cap on the remote's card limits
  how much of the link the transfers take.
- **Host networking** (`network_mode: host` in the compose file). The remote
  finds the ASIAIRs by UDP broadcast, which Docker's bridge network cannot
  deliver, and host networking helps hole-punching.
- **Loopback port 4748.** The remote's healthcheck listens on
  `127.0.0.1:4748` only; nothing outside the machine can reach it, and no other
  port is opened inbound.

### Setting up the remote machine

1. Unzip the setup files into the remote computer's home folder. You get a
   folder named `asiair-remote`, holding `.env` and `docker-compose.yml`; the
   zip stores `.env` readable by its owner only, so it needs no `chmod`.

   The `.env` holds the remote's private key and the pinned Commander key. Do
   not keep the downloaded zip in a chat, a ticket or a synced folder. Set `TZ`
   in `.env` to the remote site's zone, so updates run in daytime there (daily
   at 14:00).

2. Inside the `asiair-remote` folder, run:

   ```sh
   docker compose up -d
   ```

3. Wait for the remote's card to read *Connected*.

   Until the first contact succeeds the card reads *Waiting for first
   connection*. The remote refuses to start without its pairing
   (`docker compose logs remote` says so), and it exits and is restarted by
   `restart: unless-stopped` if it cannot reach its relay for ten minutes. It
   does not exit when Commander is off or the remote has been removed; it keeps
   running and is simply never dialled.

The container is healthy (`docker ps`) when its tunnel server is up, its
pairing is loaded and it is connected to its relay. The check never depends on
Commander or on an ASIAIR. The compose file runs
`asiair-commander remote healthcheck` for it.

### Updates

Watchtower (`nickfedor/watchtower`, on its moving `latest` tag) follows the
`ghcr.io` image's `latest` tag, daily at 14:00 in `TZ`, scoped to this compose
project only. It updates itself and removes the superseded image. The image is
public, so watchtower pulls it without registry credentials and needs only the
Docker socket. To roll back, set `REMOTE_TAG` in `.env` to a release version
and run `docker compose up -d`.

The remote and Commander speak a versioned protocol that only grows, so each
release works with every earlier one, and a feature is used only when the other
side reports it. Two cases need care:

- A remote that has stopped updating is flagged on its card.
- **If a release changes the tunnel library's wire format** (the release notes
  say so), the two ends cannot connect until both run it, and under watchtower
  the remote usually updates first:

  > 1. Update Commander.
  > 2. Remotes update themselves within a week; to update one now, run `docker compose pull && docker compose up -d` next to its compose file.
  >
  > Until both run this release the remote reads *Unreachable*; nothing is deleted while the link is down.

### Re-pairing, removing and security

Re-pairing, the remote card's **Make new setup files** button, is the only key
rotation. It mints new keys on both sides, and the old `.env` stops working the
moment Commander applies it; replace the `.env` on the remote and run
`docker compose up -d --force-recreate remote`.

- **Re-pair after a retired relay region.** A pairing fixes one relay region.
  When Tailscale retires that region, the remote cannot connect and its card
  reads *Unreachable*; make new setup files for it (a new pairing picks a live
  region) and replace the `.env`.
- **A stolen remote `.env`** lets whoever holds it stand up an impostor remote
  that Commander dials. The impostor cannot reach Commander's host or UI and
  cannot make Commander delete a frame that has neither a verified NAS copy nor
  your cull decision, but it can fake what the remote's LAN reports: it can pass
  as one of your rigs and report no unpulled frames, so *Safe to power off* can
  read Yes while frames remain on the real device. It also receives the SMB
  password of every rig Commander asks it to hash. Make new setup files for the
  remote, or Remove it.
- **Treat Commander's `/data` and its backups as secret.** `config.yaml` holds
  every remote's pairing in plain text on Linux. Whoever has a copy can reach
  the ASIAIRs behind every remote (SMB, the control port and discovery) from
  anywhere. If a copy leaves your control, **Re-pair every remote**.
- **Re-pair a remote before running a second Commander from a copy of the data
  directory** (a migration, a test instance). Two Commanders holding one
  pairing both pass the remote's pin, the tunnel flaps between them and both
  act on the same ASIAIR. Several Commanders on one remote are not supported.
- **Remove every remote before downgrading Commander.** A configuration that
  holds a remote carries a newer schema version, so an older Commander refuses
  to start with it rather than dropping the keys silently. Remove stops dialling
  at once; stop the remote's containers yourself.

### Limitations

- **Remote rigs use Commander's time zone** to decide where a night starts and
  ends (noon to noon). For a site several hours east or west, the boundary can
  fall mid-session and split one night across two night folders.
- **Windows Firewall** may ask whether to allow Commander when its tunnel client
  first binds a UDP port. Allow it on private networks; the client accepts no
  inbound connections.
- **Run Commander itself with host networking.** In a Docker bridge network
  there is a NAT layer in front of the tunnel, which works against hole-punching;
  host networking (see [Networking](#networking)) avoids it.

### Windows as a remote (manual)

The Windows executable runs in the remote role too, with no service and no
updater. Set the pairing in the environment of a console and start it:

```bat
set ASIAIR_REMOTE_PAIRING=acremote:...
asiair-commander.exe remote
```

It runs until the console closes and logs to the console. You update it by
replacing the executable. Windows Firewall may ask to allow it on first start;
allow private networks. The container route above is the supported one.
