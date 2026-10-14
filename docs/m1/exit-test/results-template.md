# M1 exit test: <yyyy-mm-dd>

<!-- Copy this file to docs/m1/exit-test/<yyyy-mm-dd>.md and fill it in (run book: README.md in this folder).
     People are A to E only: no names, no IP addresses, no domains, no screenshots that show them. -->

- **Build:** `v0.1.0-rc.N` (commit `<short hash>`)
- **VPS:** <vCPU> vCPU, <RAM> GB RAM, <distro>; transfer allowance <TB>/month, no per-GB egress billing
- **T+0:** <hh:mm> UTC; the session ran T−15 to T+120
- **Result:** PASS / FAIL (<the rows that failed>)

## Participants

| Who | Device and OS version | Browser / app | Role |
|---|---|---|---|
| A (owner, admin) | Windows 11 | Chrome <version> | sharer (Movie preset) |
| B | macOS <version> | Chrome / Edge <version> | sharer (window) |
| C | iPhone, iOS <version> | Safari, then the Home Screen app | viewer, on mobile data part of the time |
| D | Android <version> | Chrome <version>, then the installed app | viewer |
| E | Windows / Linux | Chrome <version>, port 7882 blocked (UDP and TCP) | viewer; shares at T+90 |

## Pre-flight (T−1 day)

| Check | Result |
|---|---|
| `isshoni doctor` passes | |
| Connection test from A: UDP ✓, TCP ✓, RTT | |
| Load pre-flight, 5 × 5 for 15 minutes: CPU < 40% of 2 vCPU, loss < 0.5%, RSS flat | CPU …%, loss …%, RSS … → … MiB |
| On the day, before T+0: E's connection shows `tcp443` on the dashboard | |

## Timeline

| Time | What to record | Result |
|---|---|---|
| T−5 | the dashboard shows E as `tcp443`; E: seconds from the link to video playing | |
| T+0 | C and D: seconds from the link to video playing (sign-up → Lounge → B's share → one tap to unmute) | C …, D … |
| T+5 | A's share takes focus; audio and video OK for everyone | |
| T+10 | switching focus moves the audio | |
| T+15 | C and D install the app and log in once inside it; notifications allowed (C, D, E) | |
| T+20 | "A started streaming": delivery time per person; the tap lands on A's share within 10 s | C …, D …, E … |
| T+25–60 | freezes longer than 2 s (who, when) | |
| T+60 | restart: seconds until each client plays again with no action (target ≤ 30 s) | A …, B …, C …, D …, E … |
| T+70, T+80 | C's network switch: seconds until C plays again (target ≤ 10 s) | … / … |
| T+75 | A's share resumes after the 10 s Wi-Fi drop, without re-picking | |
| T+85 | C: playing again after the unlock, with at most one tap | |
| T+90–100 | the stress (A, B and E share): what stuttered | |

## Per person

Expected actions (one tap to unmute after each join, one login inside each Home Screen app, at most one tap after
the unlock) are not manual fixes. Anything else is.

| Who | Expected actions (time) | Manual fixes (time, what) | Freezes > 2 s (time) | Notes |
|---|---|---|---|---|
| A | | | | |
| B | | | | |
| C | | | | |
| D | | | | |
| E | | | | |

## Summary

<!-- Paste the output of `exittest summarize run.csv` below, as it is: it has no title, and its sections are ###
     headings that go under this one. Its pass-criteria table is the recording's half; the table below is the
     verdict. -->

## Pass criteria

| Measure | Result | Notes |
|---|---|---|
| Manual fixes = 0 | | |
| Session: 2 h, the server restarts only at T+60 | | |
| Server CPU: average < 50%, peak < 80% of 2 vCPU | | |
| Memory: RSS growth T+65 → T+120 < 20%; goroutines back to the T+85 baseline | | |
| UDP socket errors: 0, or < 0.01% of datagrams | | |
| Recovery: all 5 in ≤ 30 s after the restart; C ≤ 10 s after the switch; A's share survives the drop | | |
| Viewing quality: freezes < 1% on desktop, < 3% on phones; no decode error that needs a reload | | |
| Phones: video + audio after one tap, app installed, notification received, playing after unlock | | |
| ICE-TCP: E on TCP 443 all session long | | |
| Survey: median ≥ 4 for picture and sound; nobody reports audio out of sync | | |

## Survey

| Who | 1. Reloaded or fixed anything? | 2. Picture (1–5) | 3. Sound and sync (1–5) | 4. Freezes > 2 s | 5. Notification arrived? |
|---|---|---|---|---|---|
| A | | | | | |
| B | | | | | |
| C | | | | | |
| D | | | | | |
| E | | | | | |
| **Median** | | | | | |

## Device matrix (05 §19.4)

The rows checked on the day; the others in their own run.

| ID | Device | Result | Notes |
|---|---|---|---|
| M-IOS-1 | iPhone, Safari tab | | |
| M-IOS-2 | iPhone, Home Screen app | | |
| M-IOS-3 | iPhone with a Discord call on the same phone | | |
| M-IOS-4 (optional) | iPhone, invite link opened from a chat app | | |
| M-IPAD | iPad Safari | | |
| M-AND-1 | Android Chrome | | |
| M-FF-1 | Firefox, fresh profile | | |
| M-SAF | Safari macOS viewer | | |
| M-EDGE | Edge on Windows 11 sharer | | |
| M-CHR-MAC | Chrome on macOS sharer | | |
| M-LAN | Firefox on macOS against a LAN server | | |
| M-WIFI | phone viewer switching Wi-Fi → LTE | | |
| M-BW | Chrome viewer on a throttled downlink | | |

## Journal

<!-- T−15 to T+120 (run book §3, "The journal"): isshoni's warnings and errors,
       journalctl -u isshoni --since … --until … -o cat | grep -E '"level":"(WARN|ERROR)|^(panic|fatal error): '
     and systemd's own messages about the unit,
       journalctl -u isshoni --since … --until … -p warning --no-hostname
     with every IP address replaced by <ip> and every name by A–E. -->

```
```

## Issues and follow-up

- Issues filed: <links>
- Repeated: <the segment, or the full 2 hours on rc.N+1>
