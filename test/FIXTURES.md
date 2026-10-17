# Canonical test fixtures

Every test, seeder, doc example and code comment that needs a device identity,
an address or a credential uses the synthetic values below. Never paste a value
from a real device, a log or a user report: GUIDs and CPU IDs are hardware
serials, and SSIDs and LAN addresses identify a person's equipment and network.
When a fixture needs a value not listed here, invent one in the same style and
add it to this table.

| What | Value | Notes |
| ---- | ----- | ----- |
| Device A guid | `0a1b2c3d` | ZWO ASIAIR Plus; model string `ZWO AirPlus-RK3568 (Linux)` (a public product name) |
| Device A cpuId | `0011223344556677` | |
| Device A SSID | `ASIAIR_0a1b2c3d` | the firmware's default `ASIAIR_<guid>` form |
| Device A IP | `192.168.1.75` | |
| Device B guid | `5eed0001` | Raspberry Pi CM4 build, model `Raspberry Pi Compute Module 4 Rev 1.0`; a longer form pads with `0` (`5eed00010000`) |
| Device B SSID | `MyRig` | a user-renamed SSID; derived names follow it (`MyRig-Hilltop`) |
| Home LAN | `192.168.1.x` | any host octet; other RFC 1918, RFC 5737 (`192.0.2.x`, `198.51.100.x`, `203.0.113.x`) and loopback addresses are fine too |
| ASIAIR default login | `guest` / `12345678` | the documented factory credentials |
| Seeder web login | `test1234` or `12345678` | `seedreal` and `seedmosaic` set `test1234`; the other seeders set `12345678` |
| Seeder fleet (`tools/seedscan`) | `f4a00001` (`FRA300-Pier`, `10.77.0.11`), `5pa00002` (`MyRig`, `10.77.0.12`), `0no00003` (no SSID, `10.77.0.13`), `unseen001` | cpuIds `beef…`; hosts `asiair-fra.invalid`, `asiair-uc.invalid` |
| Seeder remotes (`tools/seedremote`) | Spring Camp, Hilltop Observatory, Dark Sky Lodge, Pier Shed, Farm Barn, Valley Field, Ridge Pier, Creek Shed, Desert Camp | rig LANs `192.168.20.x`–`192.168.60.x`; guids such as `hl0redcat01`, `ck0seestar1`; cpuIds `beef<guid>` |
| Other seeder hosts | `asiair.invalid`, `asiair-mosaic.invalid`, `asiair-night.invalid`, `127.0.0.1` | `.invalid` never resolves (RFC 2606) |
| Timezone | `America/Toronto` | any DST-observing IANA zone works where a test needs one |
| Tailscale relay codes | `tor`, `nyc`, `fra` | public DERP region codes |
