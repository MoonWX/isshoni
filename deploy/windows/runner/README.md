# The audiotest runner (owner's PC)

`.github/workflows/audiotest.yml` runs the audio exit gate (`isshoni-audiotest run -gate`) and the hardware run
(`hwcheck.ps1`) on the owner's Windows PC through a GitHub Actions **self-hosted runner** (design:
`docs/m2/06-windows-release.md` §10.4–§10.5, `docs/m2/05-windows-audio.md` §18.8). This page is how that runner is
set up and used.

The runner is optional (docs/m2/README.md §7 question 6, the owner's call). Without it the same tools run by hand:
download the `isshoni-exit-tools` artifact of a `release.yml` run and follow `cmd/isshoni-audiotest/README.md` ("Run
the gate") and 04 §18.4 (`hwcheck.ps1`). The gate accepts either.

## Why the owner's PC and not a GitHub runner

What GitHub's `windows-2025` runner can check, and what it can't (README WA09, `.github/workflows/s1-audio.yml`):

| On GitHub's runner | |
|---|---|
| Builds and unit tests: the MSVC DLL, the native tests, the Go tests with the engine DLL, `isshoni-audiotest selfcheck` (no sound) | yes (`ci.yml`) |
| Per-app process loopback with a virtual cable (VB-CABLE, installed on the throwaway runner only) | yes: the exclusion scenarios of spike S1 pass there |
| Plain endpoint loopback (case 1 `baseline-endpoint`, Windows 10's endpoint engine) | no: the cable's endpoint loopback records only zeros, so case 1 fails and every later case is INCONCLUSIVE |
| Windows 11 client builds, Windows 10 22H2, a real sound card and microphone | no: the runner is Windows Server 2025 (build 26100) |
| Hardware encoders (`hwcheck.ps1`: NVENC, QSV, AMF, MF hardware), WGC and DD capture on a real GPU | no: the runner has no GPU (Basic Render Driver, no `ID3D11VideoDevice`); MF's software encoder there yields AUs only on drain |

So the audio gate (05 §18.10) and the hardware run need the owner's PC: by hand, or through this runner.

## How the runner is protected

The repository is public, and a self-hosted runner on a public repository runs whatever a workflow sends it. The
protections, from the strongest:

1. **The runner is offline except during a test session.** It is registered as **ephemeral** (`--ephemeral`): it
   takes one job and then deregisters itself, so each session registers it again. Between sessions there is no
   runner for any workflow to reach.
2. **No pull request reaches it.** `audiotest.yml` has only the `workflow_dispatch` trigger (never `pull_request`,
   `pull_request_target` or `workflow_run`); `tools/repocheck`'s `TestAudiotestWorkflow` reads the file and
   `TestNoPullRequestToSelfHosted` checks every workflow with a self-hosted job. A pull request can still bring a
   workflow file of its own, which is why (1) and (4) matter.
3. **Every job waits for the owner.** The `run` job uses the environment `audiotest`, whose required reviewer is the
   owner, and runs only when `github.repository == 'MoonWX/isshoni'` and the actor is the repository owner. The
   workflow's `environment` job fails the run unless that environment exists with the owner as reviewer (GitHub
   creates an unprotected environment when a workflow names one that doesn't exist).
4. **Fork workflows need approval**: Settings → Actions → General → "Require approval for all external
   contributors" (stricter than the first-time-contributor default).
5. **The job gets nothing to leak.** `run` has no checkout, no secrets and no token (`permissions: {}`); everything it
   executes comes from this run's artifact, which `build` produced on GitHub's runner or `fetch` copied from the
   release run after checking `isshoni-media.dll`'s SHA-256 against `native-manifest.json` (and `run` checks it
   again). The only token that reads another run (`actions: read`) is used by `fetch`, on GitHub's runner.
6. **A dedicated non-admin account** runs the runner, not as a service, with no access to the owner's files or
   credentials.

## One-time setup

### Repository settings (once)

1. Settings → Actions → General → Fork pull request workflows from outside collaborators → **Require approval for
   all external contributors**.
2. Settings → Environments → New environment → `audiotest`:
   - Deployment protection rules → **Required reviewers**: the repository owner. Leave "Prevent self-review" **off**:
     the owner both dispatches and approves the run.
   - No secrets and no variables: the job uses none.
   - Do this before the first dispatch (`docs/m2/06-windows-release.md` §14 item 5).

### On each Windows install (Win11, and Win10 22H2 on the dual boot)

1. **A local standard account** for the runner, for example `isshoni-ci` (Settings → Accounts → Other users → Add
   account → "I don't have this person's sign-in information" → "Add a user without a Microsoft account"). Keep it
   a **standard user**, never an administrator, and don't give it access to the owner's folders.
2. **Sign in to that account once** and set up its audio as for spike S1:
   - Windows volume at 50 % or more, not muted, on the output the tests should use (one output is simplest).
   - Sound settings → More sound settings → Communications → **Do nothing**.
   - A microphone connected, and Settings → Privacy & security → Microphone → "Let desktop apps access your
     microphone" on (the mic cases SKIP without it, which fails the gate with exit code 3).
   - No other app playing sound; no voice app running (Discord, TeamSpeak, Steam: tray icons included).
3. **The runner software** in that account: on GitHub, Settings → Actions → Runners → **New self-hosted runner** →
   Windows, x64. Follow the page's "Download" commands in PowerShell, into `C:\actions-runner` (check the SHA-256 the
   page prints, as its commands do). Don't run its "Configure" commands yet: registration is per session, below.

## A test session

1. **Sign in** to the runner account: at the PC, or over Remote Desktop with Local Resources → Remote audio →
   "Play on this computer" (the session then has the "Remote Audio" endpoint; spike S1's second run used it). WASAPI
   process loopback needs an interactive session with an audio endpoint, which is also why the runner is never a
   service (session 0 has neither).
2. **Register** the runner for this session. On GitHub, Settings → Actions → Runners → New self-hosted runner shows
   a registration token (valid for one hour). In PowerShell, in `C:\actions-runner`:

   ```powershell
   # Win11; on the Win10 install use win10 in both places.
   .\config.cmd --url https://github.com/MoonWX/isshoni --token <registration token> `
     --ephemeral --unattended --name isshoni-audio-win11 --labels isshoni-audio,win11 --work _work
   .\run.cmd
   ```

   GitHub adds the labels `self-hosted`, `Windows` and `X64` itself. The name is a generic label, never the
   machine's host name. Never pass `--runasservice`.
3. **Dispatch**: Actions → Audiotest → Run workflow. Pick the `os` of the install you are on and the `suite`:
   - `release`: the exit gate on a release candidate. `release_run_id` is required: the number at the end of the
     `release.yml` run's URL (`…/actions/runs/<id>`). `fetch` brings that run's `isshoni-exit-tools`; only this
     form counts for the exit gate and the release tiers (05 §18.10).
   - `full`: `run -gate -long`, the gate plus the 30-minute soak (minor releases). With `release_run_id` it runs on
     the release's tools and counts; without it, `build` rebuilds from the chosen ref and the report is diagnostic.
   - `hwcheck`: `hwcheck.ps1` (probe, `capture` per encoder, `bench`, `sessions -count 5`, `protect-check`; 04 §14,
     §18.4). M-LATENCY stays manual.
4. **Approve**: the run page shows "Review deployments" for `audiotest` once `build` or `fetch` is done; approve it.
   `run.cmd` picks up the job, and when it ends the runner deregisters itself and `run.cmd` exits.
5. **The manual checks** of the same session (05 §18.9, 06 §11.8): the real Discord check (Discord desktop in a call,
   YouTube in a browser, `isshoni-audiotest record`) and R1–R7 with `record`, from the downloaded tools as
   `cmd/isshoni-audiotest/README.md` says. Their one-line results go into the release-prep PR.
6. **End the session**: sign out of the runner account. If no job ran, the runner is still registered: remove it
   with `.\config.cmd remove --token <removal token>` (Settings → Actions → Runners → the runner → Remove shows the
   token), or delete it there.

Every dispatch needs its own registration (ephemeral). For both OSes, reboot into the other install and repeat the
session with its label.

## Results

- **Suites `release` and `full`**: the run's summary has a table (scenario, verdict, excluded-tone level: the
  loudest tone a case expected absent) and the tallies; the artifact `audiotest-report-<os>-<suite>` holds the
  redacted report JSON (05 §18.7: no user, host or device names, no absolute paths). Exit codes are 05's: 0 every
  required case PASS, 1 a FAIL, 2 INCONCLUSIVE (re-run that case with `-case <id>`), 3 a required case SKIPPED, 4 a
  setup error; anything but 0 fails the job. Merge the manual entries and commit the report as
  `cmd/isshoni-audiotest/README.md` ("Record the results") describes.
- **Suite `hwcheck`**: the artifact `hwcheck-<os>` holds `hwcheck.ps1`'s result bundle (no machine names or paths, 04
  §14), to attach to the PR that needs it.

## Troubleshooting

| Symptom | Cause |
|---|---|
| The `environment` job fails | The environment `audiotest` is missing or has no required reviewer: set it up as above |
| `build` fails with "suite release runs the isshoni-exit-tools of a release.yml run" | Suite `release` needs `release_run_id`; use `full` for a diagnostic rebuild |
| `fetch` fails "is … not .github/workflows/release.yml" | `release_run_id` names a run of another workflow |
| `fetch` fails on a SHA-256 | The artifact's DLLs differ from its `native-manifest.json`: don't run it; look at the release run |
| `run` waits for a runner | The runner isn't online in this session, or its label (`win11`/`win10`) differs from the `os` input |
| Exit code 3, mic cases SKIP | No microphone, or desktop apps may not use it (setup step 2) |
| Case 1 FAIL, everything else INCONCLUSIVE | The output is muted or below 50 %, or another app holds it exclusively |
