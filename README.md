# torbridge

> Legal notice and terms of use: [LEGAL.md](LEGAL.md). License: [MIT](LICENSE).

A self-maintaining [Tor](https://www.torproject.org/) client for Windows 10/11
and Linux (x86_64, systemd). The installer sets up Tor from the official
Tor Project distribution as a system service; **torbridge** (Go, standard
library only) keeps it running on working bridges selected from public lists,
verifies the connection and restores the previous state if something fails.

## Install

Download the package for your platform from Releases, unpack it and run one script:

| Platform | Package | Install | Uninstall |
|---|---|---|---|
| Windows 10/11 x64 | `TorBridge-Setup-<version>-windows-x64.zip` | double-click `Install.cmd` | `Uninstall.cmd` |
| Linux x86_64 with systemd | `TorBridge-Setup-<version>-linux-x64.tar.gz` | `sudo ./install.sh` | `sudo ./install.sh --uninstall` |

Verify downloads with `SHA256SUMS`. The packages contain prebuilt binaries and
the unmodified Tor Expert Bundle, so the installation itself needs no internet
access. The installer prints six numbered steps, then a SUCCESS / INSTALLED /
FAILED banner, and waits for Enter.

After installation (default ports):

- SOCKS5 proxy `127.0.0.1:9050`, HTTP proxy `127.0.0.1:9080`
- ControlPort `127.0.0.1:9051` with cookie authentication
- exit relays restricted to the countries listed in `ExitNodes` of the base `torrc`

On Linux, if these ports are already taken (for example by a system tor), the
installer uses 9150/9151/9180 or 9250/9251/9280 and prints its choice.

## How it works

### 1. Installation

1. Checks the package files.
2. Unpacks Tor into `C:\Tor` (Windows) or `/opt/torbridge` (Linux) and copies `torbridge`.
3. Writes the base `torrc`: ports, `ExitNodes` with `StrictNodes 1`, and
   `%include bridges.conf`. Bridges live in that separate file, which only
   torbridge writes; an existing `torrc` is kept on upgrade.
4. Registers the Tor service under an unprivileged account (LocalService on
   Windows, system user `torbridge` on Linux), restarted automatically on failure.
5. Registers the schedule (see [Automation](#3-automation)).
6. Runs the first bridge selection (`torbridge update`).

### 2. Bridge selection (`torbridge update`)

1. **Download.** `<type>_tested.txt` lists from three public GitHub
   repositories (Delta-Kronecker, OnionHop, scriptzteam-v2), refreshed hourly
   by their maintainers. If GitHub is unreachable, the jsDelivr mirror is used,
   then GitHub through the running Tor client. Duplicates are removed.
2. **Fast check.** All bridges are shuffled and probed with 100 parallel
   workers: two TCP connections each (a TLS handshake with SNI for webtunnel),
   both must succeed.
3. **Diversity.** At most one bridge per operator (IP address, or second-level
   domain for webtunnel): bridges on one server tend to fail together.
4. **Order.** Candidates are put in a weighted random order based on the test
   history (see [History and priorities](#4-history-and-priorities)).
5. **Real test.** Each candidate is tested with a separate Tor process that uses
   only this bridge and the same `ExitNodes`, 4 at a time. A bridge passes if
   bootstrap reaches 100% within 90 s (and keeps progressing: 60 s without
   progress fails it) and the check URL (`https://api.anthropic.com/` by
   default) answers on one of two separate circuits. Testing stops as soon as
   10 bridges pass; usually 10 to 20 tests, 2 to 6 minutes.
6. **Apply.** With fewer than 3 working bridges nothing changes. Otherwise the
   current `bridges.conf` is saved as `bridges.conf.bak`, the new one is written
   atomically, and the Tor service is restarted. torbridge then waits up to
   4 minutes for bootstrap 100% (via ControlPort) and a response from the check
   URL through the system SOCKS port. If the connection does not come up, the
   previous `bridges.conf` is restored and the service restarted again.
7. **Record.** Every real test goes to `logs/history.jsonl`; the time of the
   last successful update is saved. The log is `logs/torbridge.log`.

### 3. Automation

| Windows task / Linux unit | When | What it does |
|---|---|---|
| `\TorBridge\Auto` / `torbridge-auto.timer` | daily at 10:00 and 10 minutes after boot; a missed run starts later | `torbridge auto` |
| `\TorBridge\UpdateNow` / `torbridge-update.service` | on demand | `torbridge update` in the background |

`torbridge auto` refreshes the bridges if they are older than 7 days or if the
connection through the system Tor client does not work; otherwise it does
nothing. Without network it exits with code 2 and the next run tries again (on
Windows the task also starts only when a network is available). Two updates
never run at once: a lock on local port 127.0.0.1:9058.

### 4. History and priorities

Every bridge tested with a real Tor client, by `update` or by `stats`, is
appended to `logs/history.jsonl`: type, port, hosting country (geoip database
from the Tor distribution), how far bootstrap got and whether the check URL
answered. No separate background measurements are made: the statistics come
from normal operation.

For each group (`webtunnel`, `vanilla@443/80`, `vanilla@other`,
`obfs4@443/80`, `obfs4@other`, `snowflake`, ...) the success rate over the last
30 days is computed and smoothed towards initial estimates while there are few
tests (`DefaultRates` in `internal/bridge/priority.go`). The candidate order is
a weighted random sample: a bridge from a group with twice the success rate is
about twice as likely to be tried early, and every group keeps a chance.

Default types are `webtunnel` and `vanilla`. `obfs4`, `snowflake`, `meek_lite`
and `conjure` are supported but off by default because of low success rates in
measurements. `update` starts using such a type by itself once the history shows
at least 40% success over 15 or more tests (data for types that are off comes
from `torbridge stats`). An explicit `-types` disables this automatic choice.

### 5. Diagnostics (`torbridge stats`, `torbridge report`)

`torbridge stats` is run manually. It downloads the lists of all bridge types
plus the default bridges shipped with Tor, probes all of them, tests an equal
random sample of each type (`-sample 15`) with a real Tor client, and appends
the results to the history. All working bridges are written to
`bridges-found.conf`, a ready-made replacement for `bridges.conf`; with
`-apply` the fastest of them are applied to the system Tor client right away,
with the same verification and rollback as `update`.

`torbridge report` summarizes the history by type, by group and by bridge
hosting country.

## Commands

```
torbridge update   select bridges and apply them now
torbridge auto     update if bridges are older than -max-age or the connection is down
torbridge check    check the connection through the system Tor client (exit code 0/1)
torbridge status   state of the service, bridges and connection
torbridge stats    test a sample of every bridge type, write bridges-found.conf
                   (-apply: also apply the best of them)
torbridge report   statistics from the test history
```

Run `update`, `stats` and `status` as administrator (Windows) or with `sudo`
(Linux): they need the ControlPort cookie and control the service.

Flags: `-want 10`, `-min 3`, `-parallel 4`, `-max-tested 80`,
`-bootstrap-timeout 90s`, `-stall-timeout 60s`, `-target`, `-max-age 168h`,
`-types webtunnel,vanilla`, `-history`, `-stats-window 720h`, `-root`
(`C:\Tor` or `/opt/torbridge`), `-socks`, `-control` (read from the base
`torrc` by default); for `stats` and `report`: `-stats-types`, `-sample 15`,
`-apply`, `-since`. Exit codes: 0 success, 1 error, 2 no network.

## Files

```
C:\Tor\  or  /opt/torbridge/
├── tor\tor.exe, tor\pluggable_transports\   Tor Project distribution
├── data\geoip, geoip6
├── torrc                base configuration: ports, ExitNodes, %include bridges.conf
├── bridges.conf         written by torbridge (previous one: bridges.conf.bak)
├── bridges-found.conf   written by torbridge stats
├── state\               service data directory
├── logs\tor.log, torbridge.log, history.jsonl
└── torbridge.exe        (Windows: C:\Tor is added to PATH; Linux: /usr/local/bin/torbridge)
```

## Building the packages

```sh
# put the Tor Expert Bundles for windows-x86_64 and linux-x86_64 into dist/
# (verify sha256 and signatures), then:
./bundle.sh   # -> bin/TorBridge-Setup-<version>-{windows-x64.zip,linux-x64.tar.gz}, bin/SHA256SUMS
```

The Tor Expert Bundle is verified against `sha256sums-signed-build.txt` and
the Tor Browser Developers signing key
`EF6E 286D DA85 EA2A 4BA7 DE68 4E2C 6E87 9329 8290`, and included unmodified.
