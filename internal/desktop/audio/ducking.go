package audio

// duckingPref is what Windows does to other sounds during a call: Sound settings → More sound settings →
// Communications (05 §14.7).
type duckingPref uint8

const (
	// duckingUnknown: not known (not Windows, or the value can't be read). It hides the tip.
	duckingUnknown duckingPref = iota
	// duckingReduces: Windows mutes or turns other sounds down (the values 0–2, or absent: Windows' default, "Reduce
	// the volume of other sounds by 80%"). It shows the tip when a mic user or a voice app runs.
	duckingReduces
	// duckingDoNothing: "Do nothing" (3). It hides the tip.
	duckingDoNothing
)
