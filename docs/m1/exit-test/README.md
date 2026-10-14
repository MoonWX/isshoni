# M1 exit test: run book

The checklist for the M1 exit test. [06 §12](../06-deploy-and-ci.md) is the source of truth: where this page and
06 disagree, 06 wins and this page gets fixed. The collector and the summary are `tools/exittest` (its package
comment, `tools/exittest/main.go`, has every flag and column).

**Exit criterion (plan):** 5 friends finish a 2-hour session on a small VPS with no manual fixes; iPhone and Android
can watch. On a pass, `v0.1.0` is tagged from the rc's commit (06 §9.5).

**What goes into the repository:** only the results file, `docs/m1/exit-test/<yyyy-mm-dd>.md`, made from
[`results-template.md`](results-template.md). People are A to E. No names, no IP addresses, no domains, no
screenshots that show them. `run.csv`, the saved scrapes and the journal stay off the repository.

Times below are T+minutes from T+0, the moment A posts the invite link in the voice chat.

## 1. Before the day

### 1.1 What to rent and gather (a week or more before)

- [ ] **VPS:** 2 vCPU (shared is fine), 2 GB RAM, public IPv4, Ubuntu 24.04 or Debian 13; at least 1 TB of transfer
  a month and **no per-GB egress billing**; the common €4–6/month class. Expect about 45 Mbps of egress during the
  session (5 viewers × one 8.3 Mbps focus with audio + 0.3 Mbps per thumbnail), about 40 GB for 2 hours, and
  about 8.3 Mbps of ingress per sharer.
- [ ] **Domain:** a subdomain, called `<domain>` here, with an A record to the VPS set before the day, at a DNS
  provider that allows quick changes.
- [ ] **A second machine** for the load pre-flight (any machine with a good uplink that is not the VPS).
- [ ] **Four friends** who agree to the test, knowing that nothing is recorded except the server's metrics and what
  they tell you. Everyone talks on their usual voice app (Discord), as in real use.

| Who | Device | Role |
|---|---|---|
| A (owner, admin) | Windows 11, Chrome | sharer: the Movie preset, a 1080p60 film clip with sound, "window + its audio"; runs the collector on the VPS |
| B | macOS, Chrome or Edge | sharer: a game or a video in a window |
| C | iPhone, iOS ≥ 16.4, Safari, then the Home Screen app | viewer; on mobile data part of the time |
| D | Android, Chrome, then the installed app | viewer |
| E | Windows or Linux laptop, **Chrome** | viewer; **all outbound traffic to port 7882 blocked, UDP and TCP**, so ICE-TCP on 443 is the only path |

E uses Chrome, not Firefox: a Firefox viewer switches the whole room to Constrained Baseline, which takes B's
macOS Chrome share off hardware simulcast, and E shares at T+90. Firefox viewing is the M-FF-1 row of the device
matrix (05 §19.4), outside the exit test.

### 1.2 Install the rc and turn the metrics on (a few days before)

- [ ] Install the latest `v0.1.0-rc.N` with its own release script:

  ```sh
  curl -fsSL https://github.com/MoonWX/isshoni/releases/download/v0.1.0-rc.N/install.sh | sh -s -- --domain <domain>
  ```

- [ ] Create the admin with the setup link the installer prints (`sudo isshoni setup-url` makes a new one).
- [ ] Turn the metrics on **for the test only**: add to `/etc/isshoni/isshoni.toml`

  ```toml
  [metrics]
  enabled = true   # listens on 127.0.0.1:9469 (metrics.listen); never expose it
  ```

  then `sudo systemctl restart isshoni`, and check on the VPS:
  `curl -s http://127.0.0.1:9469/metrics | grep -E '^isshoni_(participants|ready)'`.
- [ ] Build the collector on your machine, in the repository's checkout (one static file; `GOARCH=arm64` for an
  ARM VPS), into the ignored `bin/`, and copy it over:

  ```sh
  GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go -C tools build -o ../bin/exittest ./exittest
  scp bin/exittest <user>@<domain>:
  ```

- [ ] Try it on the VPS for a minute, then throw the try away:

  ```sh
  sudo ./exittest collect -o try.csv      # Ctrl-C after a minute: the last row says "stop"
  ./exittest summarize try.csv | head -20  # windows of the session are "n/a" in a one-minute recording
  sudo rm -r try.csv try-metrics          # root wrote them
  ```

- [ ] Make the invite link (Admin → Invites, at least 4 uses) and send it to B and E beforehand.

### 1.3 Pre-flight (T−1 day)

- [ ] 1. `sudo isshoni doctor --people 5 --sharing 3 --hours 2` passes.
- [ ] 2. The connection test from A (Admin → Doctor, or the wizard) shows UDP ✓, TCP ✓ and the RTT.
- [ ] 3. Load pre-flight from the second machine: 5 publishers × 5 subscribers for 15 minutes. The tool is not a
  release artifact: build it from the rc's tag (`go build -o isshoni-loadtest ./cmd/isshoni-loadtest`).

  ```sh
  ./isshoni-loadtest -server https://<domain> -admin <admin> \
    -scenario focus -publishers 5 -subscribers 5 -duration 15m -json preflight.json   # asks for the password
  ```

  **Pass:** the server's CPU under 40% of 2 vCPU, loss (after NACK/RTX) under 0.5%, RSS flat. Read the CPU and the
  loss from the report: the tool's own exit status judges the 4 vCPU load gate of 02 §15.2, not this pre-flight.
  For the RSS, run the collector on the VPS meanwhile (`sudo ./exittest collect -o preflight.csv`, Ctrl-C after
  the load test) and compare the first, last and largest RSS in the Processes table of
  `./exittest summarize preflight.csv` (its pass-criteria table means nothing for a pre-flight). The load test
  deletes its `lt-…` accounts at the end.
- [ ] 4. Prepare E's firewall rules (below); on the day, before T+0, the admin dashboard must show E's connection
  as `tcp443`.

**E's firewall rules**, put in place before T−5 and removed afterwards:

- Windows (these cover IPv4 and IPv6), in an administrator PowerShell:

  ```powershell
  New-NetFirewallRule -DisplayName isshoni-test -Direction Outbound -Protocol UDP -RemotePort 7882 -Action Block
  New-NetFirewallRule -DisplayName isshoni-test -Direction Outbound -Protocol TCP -RemotePort 7882 -Action Block
  # afterwards:
  Remove-NetFirewallRule -DisplayName isshoni-test
  ```

- Linux (one nft rule for both protocols and both IP families):

  ```sh
  sudo nft add table inet isshoni_test
  sudo nft add chain inet isshoni_test out '{ type filter hook output priority 0; }'
  sudo nft add rule inet isshoni_test out meta l4proto '{ tcp, udp }' th dport 7882 reject
  # afterwards:
  sudo nft delete table inet isshoni_test
  ```

Blocking only UDP would leave TCP 7882, which the server offers too, and ICE may pick it.

## 2. The day

Keep the results template open and write down, per person, the times and seconds each row asks for. A clock with
seconds (a phone's stopwatch) is enough.

### 2.1 Timeline

- [ ] **T−15** A starts the collector on the VPS, **detached from the SSH session** (A's Wi-Fi drops at T+75):

  ```sh
  sudo systemd-run --collect --unit=exittest-collect --working-directory="$PWD" "$PWD/exittest" collect \
    -o run.csv -metrics http://127.0.0.1:9469/metrics
  systemctl status exittest-collect   # active (running)
  tail -n 3 run.csv                   # a start row, then sample and scrape rows
  ```

  If the status is not "active (running)" (with `--collect` a collector that exited is unloaded at once, so the
  status may say the unit "could not be found"), `sudo journalctl -u exittest-collect -n 20` says why; fix it and
  run the same `systemd-run` command again. (`tmux` works too:
  `sudo ./exittest collect -o run.csv -metrics http://127.0.0.1:9469/metrics` inside it.)
- [ ] **T−5** B and E sign up with the invite link. B starts a window share (a video in a window), so the others
  have something to watch from their first second. E's firewall rules are in place.
  *Record:* the admin dashboard shows E's connection as `tcp443` (pre-flight 4; if not, fix E's rules before
  T+0); E's seconds from opening the link to video playing.
- [ ] **T+0** A posts the invite link in the voice chat. C and D **long-press it → Open in Safari (C) / Chrome
  (D)**, not the chat app's built-in browser, and sign up there.
  *Record, for C and D:* seconds from opening the link to video playing (link → sign-up form → Lounge → B's share
  plays → one tap to unmute), the plan's first-join journey.
- [ ] **T+5** A shares (Movie). Nobody should have clicked a tile before this: once a friend has picked a tile, a
  new share only shows a "… started sharing [Watch]" toast and the stage stays on the picked share (05 §24).
  *Record:* A's share, the newest, takes focus; audio and video OK for everyone.
- [ ] **T+10** Everyone switches focus between A's and B's shares.
  *Record:* switching focus moves the audio (audio follows focus).
- [ ] **T+15** C and D: Add to Home Screen and allow notifications; E allows notifications.
  *Record:* C and D log in once inside the Home Screen app (it has its own cookie jar: an expected action).
- [ ] **T+19** C, D and E leave: C swipes the Home Screen app away, D closes the installed app, E closes the tab.
  Opening another page of the app is not leaving: the room session lives on (05 §19.4). Everyone waits at least
  40 s, so the 30 s resume grace ends and they are no longer in the room.
- [ ] **T+20** A stops and restarts the share, at least 10 minutes after A's share start at T+5 (`share.started`
  pushes skip people present in the room and go out at most once per room and sharer per 10 minutes, 04 §14.3–14.4).
  *Record:* C, D and E get "A started streaming": each one's delivery time. Each taps the notification and must
  land on A's share (through `?focus=`) within 10 s. If the timing slips, Account → Notifications → "Send a test
  notification" (`push.test`) is the fallback check for delivery.
- [ ] **T+25–60** Watch together, normal use; E uses fullscreen and keyboard navigation.
  *Record:* freezes longer than 2 s (who, when).
- [ ] **T+60** A announces it, then runs `sudo systemctl restart isshoni` and notes the clock time.
  *Record:* seconds until each client plays again, with no action (target ≤ 30 s).
- [ ] **T+70** C switches Wi-Fi → mobile data, **T+80** back.
  *Record:* seconds until C plays again (target ≤ 10 s), both times.
- [ ] **T+75** A turns Wi-Fi off for 10 s.
  *Record:* A's share resumes without re-picking the window (inside the 30 s grace).
- [ ] **T+85** C locks the phone for 2 minutes, then unlocks.
  *Record:* video plays again after the unlock (iOS: foreground only), with at most one tap (an expected action).
- [ ] **T+90–100** Stress: every desktop participant (A, B, E) shares at once.
  *Record:* anything that stutters; CPU, egress and loss come from the collector.
- [ ] **T+100–120** Normal watching.
- [ ] **T+120** End. A stops the collector, and everyone fills in the survey (§4):

  ```sh
  sudo systemctl stop exittest-collect
  tail -n 1 run.csv   # the stop row: "stopped by a signal"
  ```

### 2.2 Expected actions and manual fixes

These are **expected actions**, not manual fixes; write each down per person with its time:
- one tap to unmute after each join (the first join, and the return through the notification at T+20);
- one login inside each Home Screen app (C's iOS app, D's Android app; the app has its own cookie jar, 05 §16.3);
- at most one tap after the unlock at T+85 (iOS may need it after an interruption, 05 §12.8).

Anything beyond these, such as a reload, a second tap, a re-join or a second login, is a **manual fix**. So is any
server command other than the scripted restart.

## 3. Measurements

**The collector** (`tools/exittest collect`, Go, standard library only, as root) appends to `run.csv`:
- every 10 s, a sample: the isshoni PID, looked up again each time with `systemctl show -p MainPID --value
  isshoni`; isshoni's CPU % (of all CPUs), RSS, threads and open fds from `/proc/<pid>`; the load average; the NIC's
  received and sent bytes (`/proc/net/dev`, the default route's interface); UDP `InDatagrams`, `OutDatagrams`,
  `RcvbufErrors`, `SndbufErrors` and `InErrors` (`/proc/net/snmp` and `snmp6`); TCP `RetransSegs`. A PID that
  changed is first written as a `restart` row, so the T+60 restart is marked; a sample without a process is a
  `down` row.
- every 30 s, a `/metrics` scrape: saved whole into `run-metrics/<UTC time>.prom`, and the series of 06 §12.3 as
  a `scrape` row: the Go and process collectors (`go_goroutines`, `process_resident_memory_bytes`,
  `process_cpu_seconds_total`), `isshoni_participants`, `isshoni_shares`, `isshoni_sfu_downtracks`, the SFU's
  ingress and egress bytes, NACKs served, PLIs sent and layer switches, `isshoni_transfer_bytes_total` by
  direction, `isshoni_ws_resume_total{result}`, `isshoni_sfu_selected_transport{transport}` (udp, tcp443,
  tcp7882), and the viewers' own numbers (`isshoni_client_frames_decoded_total`, `…_frames_dropped_total`,
  `…_freeze_seconds_total`, `…_packets_lost_total`, `…_audio_samples_total`, `…_audio_concealed_samples_total`).
  A scrape that fails is a `scrape_error` row.

Neither the restart nor a metrics endpoint that is gone for a while stops it; `systemctl stop` (SIGTERM) ends it
with a `stop` row. Started again with the same `-o`, it appends.

**The summary:** `exittest summarize run.csv` prints Markdown for the results file: the pass-criteria table with
every row the recording can measure (server CPU, memory, goroutines, UDP socket errors, and supporting numbers for
recovery, viewing quality and ICE-TCP), each process's CPU and memory, and every counter's total and rates per
process and in all. Rates and totals are per process, split at the restart, and a counter that goes down counts
as reset, the Prometheus way (the scraped counters start again at zero at T+60). A row passes only on what was
recorded: a gap of more than 2 minutes in the samples, CPU samples that miss the stress or a tenth of the session,
or counters that reset without a PID change (a restart in place, 04 §6.5) make it `check`. The recovery row gives
the outage as a range, because the participants are scraped every 30 s; the viewers' freeze time is a share of the
video tracks they watched, as they report freezes per track. T+0 is taken as 60 minutes before the restart; if the
restart was not at T+60, or did not happen, give T+0 yourself: `-t0 18:00` (UTC) or `-t0 2026-10-15T20:00:00+02:00`.

**From the people and the admin dashboard:** the per-person times of §2.1, the expected actions and manual fixes,
freezes longer than 2 s, E's transport (the dashboard's connections show each one's transport and last stats) and
C's freezes, the survey.

**The journal:** the warnings and errors of the session. isshoni writes its log as JSON lines that the journal
stores at one priority, so `-p warning` doesn't find its warnings: filter on the JSON `level` instead, and take
systemd's own messages about the unit (a crash, the failed state) with `-p warning`. Give the times with their
zone, such as `"2026-10-15 17:45 UTC"`; without one, journalctl reads them in the VPS's time zone:

```sh
# isshoni's warnings and errors, and a crash's panic if there was one:
sudo journalctl -u isshoni --since "<T−15>" --until "<T+120>" -o cat |
  grep -E '"level":"(WARN|ERROR)|^(panic|fatal error): '
# systemd's own messages about the unit:
sudo journalctl -u isshoni --since "<T−15>" --until "<T+120>" -p warning --no-hostname
```

Replace any IP address by `<ip>` and any name by A–E before the excerpt goes into the results.

## 4. The survey

Each of A to E answers, right after T+120:
1. Did you have to reload or fix anything?
2. Picture quality (1–5).
3. Sound quality and sync (1–5).
4. How many freezes longer than 2 s?
5. Did the notification arrive?

## 5. Pass criteria (06 §12.3)

Every row must pass. "From" says where the answer comes from.

| Measure | Target | From |
|---|---|---|
| Manual fixes (reloads, re-joins, server commands other than the scripted restart) | **0**. Expected actions (§2.2) are not manual fixes | the per-person notes |
| Session | 2 h; the server never restarts except at T+60 | summary: "Session" |
| Server CPU | average < 50% of 2 vCPU, peak < 80% | summary: "Server CPU" |
| Memory | RSS growth from T+65 to T+120 < 20% (the process started at T+60, about 55 minutes; no leak); goroutines back to the T+85 baseline (taken before the stress) after it. T+5 → T+59, the first process, is reported as a second sample | summary: "Memory", "Goroutines" |
| UDP socket errors | `RcvbufErrors + SndbufErrors` = 0, or < 0.01% of datagrams | summary: "UDP socket errors" |
| Recovery | after the server restart: all 5 play again in ≤ 30 s; after C's network switch: ≤ 10 s; A's share survives a 10 s drop | the per-person notes (the summary shows when the participants were back) |
| Viewing quality | total freeze time < 1% of watch time on desktop and < 3% on phones; no decode errors that need a reload | the dashboard and the notes, per device (the summary has the total over every device) |
| Phones | iPhone and Android: video + audio after one tap, Home Screen app installed, notification received, playing again after unlock | the per-person notes |
| ICE-TCP | E's selected transport is TCP on 443 all session long | the dashboard (the summary shows the scrapes with a PeerConnection on tcp443) |
| Survey (1–5) | median ≥ 4 for picture and sound; nobody reports audio out of sync | the survey |

The summary marks each row `pass`, `FAIL`, `check` (measured, needs a look), `manual` (from the people, the
dashboard or the survey) or `n/a` (not in the recording).

## 6. Afterwards

- [ ] Copy the recording off the VPS and keep it private: into the checkout's ignored `.dev/` (or anywhere outside
  the checkout), never next to tracked files, where `git add` would pick it up:

  ```sh
  mkdir -p .dev/exit-test
  scp -r <user>@<domain>:run.csv <user>@<domain>:run-metrics .dev/exit-test/
  ```

- [ ] Summarize, from the checkout (`go -C` runs in `tools/`, so give an absolute path):
  `go -C tools run ./exittest summarize "$PWD/.dev/exit-test/run.csv" > .dev/exit-test/summary.md`
  (or on the VPS: `./exittest summarize run.csv > summary.md`).
- [ ] Turn the metrics off again (remove the `[metrics]` block, `sudo systemctl restart isshoni`), remove E's
  firewall rules. The collector's unit is gone once stopped (`systemd-run --collect` units are transient and
  unloaded however they end).
- [ ] Write `docs/m1/exit-test/<yyyy-mm-dd>.md` from [`results-template.md`](results-template.md): the summary,
  the per-person notes, the survey, the rows of the device matrix (05 §19.4) checked that day, and the journal
  excerpt. A to E only: no names, IPs or domains.
- [ ] Every failure becomes an issue.
- [ ] After fixes, the failed segment is repeated. A crash, a leak or any manual fix means repeating the full 2
  hours on the next rc. On a pass, `v0.1.0` is tagged from the rc's commit (06 §9.5).
