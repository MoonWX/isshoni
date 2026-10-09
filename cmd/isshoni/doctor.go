package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server/config"
	"github.com/MoonWX/isshoni/internal/server/ops"
	"github.com/MoonWX/isshoni/internal/server/ops/doctor"
	"github.com/MoonWX/isshoni/internal/server/store"
)

type doctorOptions struct {
	clientOptions
	json, strict, listChecks bool
	only                     string // comma-separated check ids

	// Bandwidth calculator input (04 §13.4).
	people, sharing, thumbnails int
	quality, preset             string
	hours                       float64
}

func doctorCmd() *command {
	return &command{
		name:    "doctor",
		summary: "Diagnose the server and this machine",
		help: "Runs the checks inside the running server through its admin socket, or on this machine when the " +
			"server is stopped (then the checks that need a live server are skipped). Exits 5 when a check fails " +
			"(with --strict, also when one warns). The bandwidth flags describe a session for the bandwidth estimate.",
		config: true,
		setup: leaf(func(fs *flag.FlagSet, o *doctorOptions) {
			def := doctor.DefaultBandwidthInput()
			fs.BoolVar(&o.json, "json", false, "print the report as JSON")
			fs.BoolVar(&o.strict, "strict", false, "exit 5 on a warning too")
			fs.StringVar(&o.only, "only", "", "run only the checks with these comma-separated `IDS` (see --list-checks)")
			fs.BoolVar(&o.listChecks, "list-checks", false, "print the check ids and exit (a JSON array with --json)")
			fs.IntVar(&o.people, "people", def.People, "bandwidth estimate: `N` people in a session")
			fs.IntVar(&o.sharing, "sharing", def.Sharing, "bandwidth estimate: `N` of them sharing at the same time")
			fs.IntVar(&o.thumbnails, "thumbnails", def.Thumbnails, "bandwidth estimate: at most `N` thumbnails per viewer")
			fs.StringVar(&o.quality, "quality", string(def.Quality), "bandwidth estimate: `QUALITY` of the shares: 1080p60, 1440p60 or 2160p60")
			fs.StringVar(&o.preset, "preset", string(def.Preset), "bandwidth estimate: `PRESET` auto or movie (256 kbps audio)")
			fs.Float64Var(&o.hours, "hours", def.Hours, "bandwidth estimate: session length in `HOURS`")
			o.register(fs)
		}, runDoctor),
	}
}

// doctorCallTimeout bounds the call to a running server: its run, and the wait for a run that is under way.
const doctorCallTimeout = 90 * time.Second

// runDoctor runs the checks of 04 §13 and prints the report: as text, or with --json as the one JSON document of
// 04 §13.5.
//
//   - A server answers on the admin socket: it runs the checks in its own process (04 §13.1), and the report is
//     its answer.
//   - No server runs: the checks run here, offline.
//   - The socket refuses this user: the "run it with sudo" message and exit 4, without any check. As an ordinary
//     user the offline checks would only be a wall of false failures: a config and a data directory that can't be
//     read, ports "in use" by the running server (04 §12.1).
//
// It exits 5 when a check failed, with --strict also when one warned (04 §3.2).
func runDoctor(ctx context.Context, inv *invocation, o doctorOptions, _ []string) error {
	if o.listChecks {
		return listChecks(inv, o.json)
	}
	in, err := o.session()
	if err != nil {
		return err
	}
	only, err := parseOnly(o.only)
	if err != nil {
		return err
	}

	req := ops.AdminDoctorRequest{Only: only}
	if in != doctor.DefaultBandwidthInput() {
		req.Bandwidth = &in
	}
	c := o.admin(inv)
	callCtx, cancel := context.WithTimeout(ctx, doctorCallTimeout)
	report, err := c.Doctor(callCtx, req)
	cancel()
	switch {
	case err == nil:
	case errors.Is(err, ops.ErrAdminNotRunning):
		report = doctor.Run(ctx, offlineDoctorEnv(inv), in, only)
		if err := ctx.Err(); err != nil {
			return err // interrupted: a report with half its checks skipped helps nobody
		}
	default:
		return adminFailure(inv, o.clientOptions, c, err)
	}

	if o.json {
		if err := printJSON(inv.stdout, report); err != nil {
			return err
		}
	} else {
		doctor.RenderText(inv.stdout, report, inv.color(inv.stdout))
	}
	if report.Summary.Fail > 0 || (o.strict && report.Summary.Warn > 0) {
		return exitWith(exitDoctorFail, nil) // the report says why
	}
	return nil
}

// listChecks prints the check ids of 04 §13.2 in table order, one per line, or as a JSON array (04 §13.1). The
// project site has a troubleshooting section per id, and its anchor test reads this list.
func listChecks(inv *invocation, asJSON bool) error {
	ids := doctor.CheckIDs()
	if asJSON {
		return printJSON(inv.stdout, ids)
	}
	_, err := fmt.Fprintln(inv.stdout, strings.Join(ids, "\n"))
	return err
}

// session returns the session of the bandwidth flags, or a usage error that names the flag with a value the
// calculator does not take.
func (o doctorOptions) session() (api.BandwidthInput, error) {
	in := api.BandwidthInput{
		People: o.people, Sharing: o.sharing, Thumbnails: o.thumbnails,
		Quality: api.BandwidthQuality(o.quality), Preset: api.BandwidthPreset(o.preset), Hours: o.hours,
	}
	field, _, ok := doctor.BandwidthInputProblem(in)
	if ok {
		return in, nil
	}
	switch field {
	case "people":
		return in, usageErrorf("--people %d: want 1 to %d", o.people, doctor.MaxBandwidthPeople)
	case "sharing":
		return in, usageErrorf("--sharing %d: want 0 to --people (%d)", o.sharing, o.people)
	case "thumbnails":
		return in, usageErrorf("--thumbnails %d: want 0 to %d", o.thumbnails, doctor.MaxBandwidthThumbnails)
	case "quality":
		return in, usageErrorf("--quality %q: want 1080p60, 1440p60 or 2160p60", o.quality)
	case "preset":
		return in, usageErrorf("--preset %q: want auto or movie", o.preset)
	default:
		return in, usageErrorf("--hours %v: want more than 0, up to %d", o.hours, doctor.MaxBandwidthHours)
	}
}

// parseOnly splits --only into check ids. An id that doctor does not have is a usage error: a typo must not turn
// into a run that silently checks less.
func parseOnly(value string) ([]string, error) {
	if value == "" {
		return nil, nil
	}
	known := doctor.CheckIDs()
	var only []string
	for _, id := range strings.Split(value, ",") {
		id = strings.TrimSpace(id)
		switch {
		case id == "":
		case !slices.Contains(known, id):
			if s := suggest(id, known); s != "" {
				return nil, usageErrorf("--only: no check %q (did you mean %q?); --list-checks prints the ids", id, s)
			}
			return nil, usageErrorf("--only: no check %q; --list-checks prints the ids", id)
		case !slices.Contains(only, id):
			only = append(only, id)
		}
	}
	if len(only) == 0 {
		return nil, usageErrorf("--only %q names no check; --list-checks prints the ids", value)
	}
	return only, nil
}

// offlineDoctorEnv is what doctor sees when it runs in this process (04 §13.1): the loaded config (also one with
// errors, which the config check then reports), this machine, and 03's file-level store functions, which read the
// database without migrating it or creating any file next to it. Everything else is doctor's own default.
func offlineDoctorEnv(inv *invocation) doctor.Env {
	backups := inv.cfg.Paths().Backups
	env := doctor.Env{
		Config: inv.cfg,
		UID:    os.Getuid(),
		Host:   config.Host{Environ: inv.environ},
		DB: doctor.DBFiles{
			LatestSchemaVersion: store.LatestSchemaVersion,
			Inspect: func(ctx context.Context, dbPath string) (doctor.DBInfo, error) {
				info, err := store.InspectFile(ctx, dbPath, backups)
				if err != nil {
					return doctor.DBInfo{}, err
				}
				out := doctor.DBInfo{
					SchemaVersion:  info.SchemaVersion,
					LastAppVersion: info.LastAppVersion,
					HistoryOK:      info.HistoryOK,
					Integrity:      info.Integrity,
				}
				if info.TooNew != nil {
					out.TooNewBackup, out.TooNewMessage = info.TooNew.Backup, info.TooNew.Error()
				}
				return out, nil
			},
			Backup: store.BackupFile,
		},
	}
	if inv.doctorEnv != nil {
		inv.doctorEnv(&env)
	}
	return env
}
