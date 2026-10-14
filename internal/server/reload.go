package server

import (
	"context"
	"log/slog"
	"reflect"
	"strings"

	"github.com/MoonWX/isshoni/internal/server/config"
	"github.com/MoonWX/isshoni/internal/server/ops"
)

// This file is the reload of 04 §6.5 (README S90): what SIGHUP changes in a running server. `systemctl reload
// isshoni` sends the signal (06's ExecReload), and so does `docker compose kill -s HUP isshoni`.

// reloadKey is the one config key that a reload applies. Every other key takes a restart: the listeners, the site
// and its certificate, the limits that the components were built with.
const reloadKey = "log.level"

// Reload applies what SIGHUP reloads (04 §6.5): it reads the manual TLS files again (tlsmgr.Manager.Reload; in the
// other modes there is nothing to read), and it makes cfg's log.level the configured level of the process
// (ops.LogLevel.SetBase: a level that `isshoni admin log-level` set for a while stays until its time is up, and
// falls back to the new one). Nothing else of cfg is applied: the keys whose effective value differs from the
// config the server started with are logged by name (never their values), with the advice to restart.
//
// cfg is the config as cmd/isshoni loaded it again, from the same flags and environment and the file as it is now,
// so the precedence of 04 §4.1 holds: a log.level set by a flag or ISSHONI_LOG_LEVEL wins over the file. A cfg with
// errors changes no level (it is the config that a restart would refuse, with exit 78, and the log says so with
// every problem); a nil cfg is a config that could not be loaded at all. The certificate files are read again in
// both cases: they are no part of the config.
//
// A reload that comes during the startup waits for it; one that comes once the shutdown has begun does nothing.
// Before Start it only sets the level, which the start then takes as the configured one. Reload logs what it did,
// and returns the TLS manager's error when the new certificate files were rejected (the old pair keeps serving,
// and the manager has logged why and how to fix it).
func (s *Server) Reload(ctx context.Context, cfg *config.Config) error {
	s.life.Lock()
	defer s.life.Unlock()
	switch s.state {
	case stateStopping, stateStopped:
		s.log.Info("reload ignored: the server is shutting down")
		return nil
	}

	level, levelOK := s.reloadLevel(ctx, cfg)
	if cfg != nil {
		if changed := s.restartKeys(cfg); len(changed) > 0 {
			s.log.LogAttrs(ctx, slog.LevelWarn, "the config has changes that a reload does not apply; restart isshoni to "+
				"apply them", slog.Any("keys", changed))
		}
	}

	var tlsErr error
	certificate := "not read: the certificate is not from files (tls.mode is not manual)"
	switch {
	case s.tls == nil: // before Start: the start reads the files
		certificate = "not read: the server has not started"
	case s.cfg.EffectiveTLSMode() == config.TLSManual:
		certificate = "read again"
		if tlsErr = s.tls.Reload(); tlsErr != nil { // logged by the TLS manager, with the fix
			certificate = "rejected: the files can't be used (the line before says why)"
		}
	}

	attrs := []slog.Attr{slog.String("certificate", certificate)}
	if !levelOK {
		s.log.LogAttrs(ctx, slog.LevelInfo, "reloaded (SIGHUP) without the log level", attrs...)
		return tlsErr
	}
	name := strings.ToLower(level.String())
	attrs = append(attrs, slog.String("log_level", name))
	// The line goes out at the level it is written under: before the change while the old level shows it, after it
	// otherwise (a reload from warn to debug), so that it is there whichever way the level moves.
	before := s.log.Enabled(ctx, slog.LevelInfo)
	if before {
		s.log.LogAttrs(ctx, slog.LevelInfo, "reloaded (SIGHUP)", attrs...)
	}
	s.setBaseLevel(level)
	if !before {
		s.log.LogAttrs(ctx, slog.LevelInfo, "reloaded (SIGHUP)", attrs...)
	}
	return tlsErr
}

// reloadLevel returns the log level of a reloaded config, and false when there is none to apply: no level variable
// (Deps.LogLevel is nil), no config, or a config with errors, which are logged here.
func (s *Server) reloadLevel(ctx context.Context, cfg *config.Config) (slog.Level, bool) {
	if s.deps.LogLevel == nil {
		s.log.LogAttrs(ctx, slog.LevelWarn, "reload: the log level of this process is fixed; log.level is not reloaded")
		return 0, false
	}
	if cfg == nil {
		s.log.LogAttrs(ctx, slog.LevelWarn, "reload: the config could not be read again; log.level is not reloaded")
		return 0, false
	}
	all, warnings := configProblems(cfg)
	if len(all) > len(warnings) {
		log := s.log.With(slog.String("component", "config"))
		for _, p := range all {
			if p.Severity != config.SeverityError {
				continue
			}
			line, _, _ := strings.Cut(p.String(), "\n")
			attrs := []slog.Attr{slog.String("key", p.Key)}
			if p.Fix != "" {
				attrs = append(attrs, slog.String("fix", p.Fix))
			}
			log.LogAttrs(ctx, slog.LevelWarn, line, attrs...)
		}
		s.log.LogAttrs(ctx, slog.LevelWarn, "reload: the config has errors: log.level is not reloaded, and isshoni "+
			"would refuse to start with this config (exit 78) until they are fixed", slog.Int("errors", len(all)-len(warnings)))
		return 0, false
	}
	level, ok := ops.ParseLogLevel(cfg.Log.Level)
	if !ok { // config validates log.level; this would be a bug there
		s.log.LogAttrs(ctx, slog.LevelError, "reload: log.level is not a level", slog.String("log_level", cfg.Log.Level))
		return 0, false
	}
	return level, true
}

// setBaseLevel makes level the configured log level: through the runtime level of the admin socket once the server
// has built it, so that a level set there for a while is kept until its time is up; before that on the variable
// itself, which the start then takes as the configured level (ops.NewLogLevel). Called with life held.
func (s *Server) setBaseLevel(level slog.Level) {
	if s.logLevel != nil {
		s.logLevel.SetBase(level)
		return
	}
	s.deps.LogLevel.Set(level)
}

// restartKeys returns the keys other than log.level whose effective value in cfg differs from the config the server
// was built with, in the registry's order.
func (s *Server) restartKeys(cfg *config.Config) []string {
	was := map[string]any{}
	for _, k := range s.loaded.Printed().Keys {
		was[k.Key] = k.Value
	}
	var changed []string
	for _, k := range cfg.Printed().Keys {
		if k.Key != reloadKey && !reflect.DeepEqual(was[k.Key], k.Value) {
			changed = append(changed, k.Key)
		}
	}
	return changed
}
