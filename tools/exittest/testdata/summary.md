run.csv: recorded 2026-10-15 17:45:00 to 20:00:04 UTC (2h15m4s), T-15:20 to T+119:44. T+0 is 2026-10-15 18:00:20 UTC: 60 minutes before the first restart, the scripted one at T+60 (-t0 sets it).

811 samples (1 without isshoni), 271 scrapes (1 failed), 1 restart; the collector started 1 time, with no gap.

### Pass criteria (06 §12.3)

| Measure | Target | Measured | Result |
|---|---|---|---|
| Manual fixes | 0 (expected actions are not fixes) | the per-person notes | manual |
| Session | 2 h; the server restarts only at T+60 | recorded T-15:20 to T+119:44; restart at T+60:00; no isshoni process in 1 sample | pass |
| Server CPU | average < 50%, peak < 80% of all CPUs | isshoni T+0 to T+120: average 14.5%, peak 18.9% of 2 CPUs (whole machine: average 16.0%, peak 20.4%); the samples cover 99.4% of T+0 to T+120 | pass |
| Memory | RSS growth T+65 to T+120 < 20% | 55.8 MiB at T+65:00 to 57.3 MiB at T+119:40 (+2.7%); first process T+5 to T+59: 62.5 MiB to 64.5 MiB (+3.3%) | pass |
| Goroutines | back to the T+85 baseline after the stress (T+90 to T+100) | 151 at T+84:40 with 5 participants and 2 shares, peak 176 in the stress, 150 at T+119:40 with 5 participants and 2 shares (-0.7%) | pass |
| UDP socket errors | RcvbufErrors + SndbufErrors = 0, or < 0.01% of datagrams | 3 of 46,583,573 datagrams (RcvbufErrors 3, SndbufErrors 0), < 0.0001% | pass |
| Recovery | after the restart all 5 play in <= 30 s; after C's network switch <= 10 s; A's share survives a 10 s drop | 5 participants in the old process's last full scrape (T+59:10) and again from T+60:10: the outage lasted between 10s and 1m0s (scraped every 30s); the per-person notes | manual |
| Viewing quality | freezes < 1% of watch time on desktop, < 3% on phones; no decode errors that need a reload | 130.6 s frozen in 16.1 h of video tracks watched (0.23%, every device together); 0.05% of frames dropped; 0.06% of audio concealed; per device: the dashboard and the notes | manual |
| Phones | iPhone and Android: video and audio after one tap, Home Screen app, notification, playing after unlock | the per-person notes | manual |
| ICE-TCP | E's selected transport is TCP on 443 all session long | a PeerConnection on tcp443 in 235 of 239 scrapes T+0 to T+120; none at T+18:40, T+19:10, T+19:40, T+20:10; E's own transport: the dashboard | manual |
| Survey (1-5) | median >= 4 for picture and sound; nobody reports audio out of sync | the survey | manual |

pass: measured and met. FAIL: measured and missed. check: measured, needs a look. manual: from the
people, the admin dashboard or the survey. n/a: not in the recording.

### Processes

| # | PID | From | To | Length | CPU average | CPU peak | RSS first, last (max) | Threads max | FDs max | Goroutines |
|---|---|---|---|---|---|---|---|---|---|---|
| 1 | 4127 | T-15:20 | T+59:40 | 1h15m0s | 11.8% | 14.9% | 46.0 MiB, 64.5 MiB (64.6 MiB) | 15 | 43 | 38-151 |
| 2 | 31877 | T+60:00 | T+119:40 | 59m40s | 14.9% | 18.9% | 42.7 MiB, 57.3 MiB (62.3 MiB) | 17 | 46 | 126-176 |

### Counters

The increase per process (a counter that went down counts as reset), and for rates the average over the
process's time and the peak between two rows.

| Counter | Process 1 | Process 2 | All |
|---|---|---|---|
| NIC received | 7.5 GiB (14.4 Mbit/s, peak 18.0 Mbit/s) | 8.1 GiB (19.4 Mbit/s, peak 26.7 Mbit/s) | 15.7 GiB (16.6 Mbit/s, peak 26.7 Mbit/s) |
| NIC sent | 18.3 GiB (34.9 Mbit/s, peak 44.5 Mbit/s) | 18.6 GiB (44.6 Mbit/s, peak 46.1 Mbit/s) | 36.9 GiB (39.1 Mbit/s, peak 46.1 Mbit/s) |
| UDP datagrams in | 6,796,907 (1,510/s, peak 1,888/s) | 7,301,982 (2,040/s, peak 2,801/s) | 14,117,769 (1,742/s, peak 2,801/s) |
| UDP datagrams out | 16,082,796 (3,574/s, peak 4,557/s) | 16,337,438 (4,563/s, peak 4,715/s) | 32,465,804 (4,006/s, peak 4,715/s) |
| UDP RcvbufErrors | 0 | 3 | 3 |
| UDP SndbufErrors | 0 | 0 | 0 |
| UDP InErrors | 0 | 3 | 3 |
| TCP RetransSegs | 3,364 (0.7/s, peak 0.9/s) | 2,984 (0.8/s, peak 0.9/s) | 6,351 (0.8/s, peak 0.9/s) |
| isshoni CPU time | 1049.1 s | 1068.5 s | 2117.6 s |
| SFU ingress (RTP) | 6.7 GiB (12.8 Mbit/s, peak 16.3 Mbit/s) | 7.4 GiB (17.6 Mbit/s, peak 24.4 Mbit/s) | 14.1 GiB (14.9 Mbit/s, peak 24.4 Mbit/s) |
| SFU egress (RTP) | 17.0 GiB (32.4 Mbit/s, peak 41.7 Mbit/s) | 17.4 GiB (41.8 Mbit/s, peak 43.2 Mbit/s) | 34.4 GiB (36.5 Mbit/s, peak 43.2 Mbit/s) |
| Transfer ingress (sockets) | 6.9 GiB (13.2 Mbit/s, peak 16.8 Mbit/s) | 7.6 GiB (18.2 Mbit/s, peak 25.2 Mbit/s) | 14.5 GiB (15.4 Mbit/s, peak 25.2 Mbit/s) |
| Transfer egress (sockets) | 17.6 GiB (33.5 Mbit/s, peak 43.1 Mbit/s) | 18.0 GiB (43.1 Mbit/s, peak 44.6 Mbit/s) | 35.5 GiB (37.7 Mbit/s, peak 44.6 Mbit/s) |
| NACKs served | 48,815 (10.8/s, peak 14.0/s) | 49,887 (13.9/s, peak 14.0/s) | 98,702 (12.2/s, peak 14.0/s) |
| PLIs sent | 354 (0.1/s, peak 0.1/s) | 388 (0.1/s, peak 0.2/s) | 742 (0.1/s, peak 0.2/s) |
| Layer switches | 316 (0.1/s, peak 0.6/s) | 298 (0.1/s, peak 0.2/s) | 614 (0.1/s, peak 0.6/s) |
| Resumes: resumed | 0 | 3 | 3 |
| Resumes: not resumed | 0 | 6 | 6 |
| Client frames decoded | 1,295,205 (288/s, peak 375/s) | 1,381,470 (386/s, peak 450/s) | 2,676,675 (330/s, peak 450/s) |
| Client frames dropped | 671 | 709 | 1,380 |
| Client freeze time | 54.4 s | 76.3 s | 130.6 s |
| Client packets lost | 17,542 | 17,822 | 35,364 |
| Client audio samples | 842,016,000 | 855,456,000 | 1,697,472,000 |
| Client audio concealed | 526,260 | 534,660 | 1,060,920 |

### Gauges

The largest value per process.

| Gauge | Process 1 | Process 2 |
|---|---|---|
| Participants | 5 | 5 |
| Shares | 2 | 3 |
| DownTracks | 16 | 24 |
| Video DownTracks, high | 4 | 4 |
| Video DownTracks, low | 4 | 8 |
| PCs on udp | 7 | 6 |
| PCs on tcp443 | 1 | 2 |
| PCs on tcp7882 | 0 | 0 |
| Load average (1 min) | 0.43 | 0.55 |
