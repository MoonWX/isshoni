package settings

import (
	"encoding/json"
	"log/slog"
	"math"
)

// migrations[n] turns schema n into schema n+1, on the file's raw JSON object (03 §14.2: forward-only, one function
// per schema step, run at load). len(migrations) is CurrentSchema.
var migrations = []func(raw map[string]any){
	0: migrate0to1,
}

// migrate0to1: schema 0 is a settings.json without a schema field (or with "schema": 0), as hand edits and builds
// from before schema 1 left it. Its fields already have schema 1's names and meanings; a null in place of a known
// object or value means "not set", so it is removed and the default applies.
func migrate0to1(raw map[string]any) {
	for _, k := range []string{"serverPrefs", "wizardDone", "window", "closeToTray", "autostart", "hideFromCapture",
		"notifications", "hotkeys", "pill", "updates", "tips", "log", "audio"} {
		if v, ok := raw[k]; ok && v == nil {
			delete(raw, k)
		}
	}
	raw["schema"] = json.Number("1")
}

// schemaOf reads the file's schema number: absent (or not a whole number ≥ 0) is 0.
func schemaOf(raw map[string]any) int {
	n, ok := raw["schema"].(json.Number)
	if !ok {
		return 0
	}
	f, err := n.Float64()
	if err != nil || f < 0 || f != math.Trunc(f) || f > math.MaxInt32 {
		return 0
	}
	return int(f)
}

// migrate runs the migrations from the file's schema up to CurrentSchema. A file with a newer schema is left as it
// is (read best-effort). migrated reports that a migration ran.
func migrate(raw map[string]any, log *slog.Logger) (out map[string]any, migrated bool) {
	from := schemaOf(raw)
	if from >= CurrentSchema {
		if from > CurrentSchema {
			log.Info("settings: settings.json is from a newer app; read as far as this build knows it",
				"schema", from, "known", CurrentSchema)
		}
		return raw, false
	}
	for v := from; v < CurrentSchema; v++ {
		migrations[v](raw)
	}
	log.Info("settings: settings.json migrated", "from", from, "to", CurrentSchema)
	return raw, true
}
