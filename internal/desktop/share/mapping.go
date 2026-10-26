package share

import (
	"slices"

	"github.com/MoonWX/isshoni/internal/client/core"
	"github.com/MoonWX/isshoni/internal/media"
	"github.com/MoonWX/isshoni/internal/protocol"
)

// 03 §10.1's two tables and the rest of 01's snapshot → View.

// stateOf is 03 §10.1's state table: 01's ShareState and Paused → View.State. ended is endOf's (ended or failed).
//
//	joining, starting   starting
//	live with Paused    paused
//	live                live
//	reconnecting        reconnecting
//	unreachable         unreachable (5 rebuilds without connecting, 01 §11.6)
//	ended               ended, or failed for reason error
func stateOf(s *core.ShareSnapshot) State {
	switch s.State {
	case core.StateLive:
		if s.Paused {
			return StatePaused
		}
		return StateLive
	case core.StateReconnecting:
		return StateReconnecting
	case core.StateUnreachable:
		return StateUnreachable
	case core.StateEnded:
		st, _, _ := endOf(s.End, "")
		return st
	}
	return StateStarting // joining, starting, and a state a later core adds
}

// endOf is 03 §10.1's end-reason table: 01's EndInfo → the state, the end reason and a failed share's error. cause
// is the controller's own Stop's ("" when it didn't stop the share).
//
//	user                                       cause: user, hotkey, tray or system
//	source-gone                                source-gone
//	server, Server ∈ {stopped, left}, !ByThisApp  elsewhere ("Sharing was stopped from another device")
//	any other server                           server (e.g. media_timeout)
//	server-unreachable                         server-unreachable (01's 30 s hold ran out)
//	unlinked                                   unlinked (C2's session adapter told 02, 03 §9)
//	error                                      failed: engine.<media code> when EndInfo names one, else the protocol code
func endOf(e *core.EndInfo, cause Cause) (State, EndReason, error) {
	if e == nil { // an ended snapshot always has its EndInfo; a missing one is the controller's own stop
		return StateEnded, causeReason(cause), nil
	}
	switch e.Reason {
	case core.EndUser:
		return StateEnded, causeReason(cause), nil
	case core.EndSourceGone:
		return StateEnded, EndSourceGone, nil
	case core.EndServer:
		if (e.Server == protocol.EndReasonStopped || e.Server == protocol.EndReasonLeft) && !e.ByThisApp {
			return StateEnded, EndElsewhere, nil
		}
		return StateEnded, EndServer, nil
	case core.EndServerUnreachable:
		return StateEnded, EndServerUnreachable, nil
	case core.EndUnlinked:
		return StateEnded, EndUnlinked, nil
	}
	return StateFailed, EndError, endError(e) // error, and a reason a later core adds
}

// causeReason is the end reason of a share 01 ended with reason user: the controller's cause, user when none.
func causeReason(c Cause) EndReason {
	switch c {
	case CauseHotkey:
		return EndHotkey
	case CauseTray:
		return EndTray
	case CauseSystem:
		return EndSystem
	}
	return EndUser
}

// endError is a failed share's error: the media code (engine.<code>; 01's EndInfo carries no detail token, so
// params.reason is empty), else the protocol code, else internal.
func endError(e *core.EndInfo) error {
	switch {
	case e.Media != "":
		return &media.Error{Code: e.Media, Op: "share"}
	case e.Protocol != "":
		return &protocol.Error{Code: e.Protocol}
	}
	return &media.Error{Code: media.CodeInternal, Op: "share"}
}

// hintOf is View.Hint: upload-limited from Upload.Limited and ApproxHeight, cpu-limited from Overloaded (03 §6.3).
func hintOf(s *core.ShareSnapshot) Hint {
	switch {
	case s.Upload.Limited:
		return Hint{Kind: HintUploadLimited, ApproxHeight: s.Upload.ApproxHeight}
	case s.Overloaded:
		return Hint{Kind: HintCPULimited}
	}
	return Hint{}
}

// layersOf is View.Layers.
func layersOf(ls []core.LayerSnapshot) []Layer {
	out := make([]Layer, 0, len(ls))
	for _, l := range ls {
		out = append(out, Layer{
			RID: l.RID, Width: l.Width, Height: l.Height, FPS: l.FPS, TargetBps: l.TargetBps, SentBps: l.SentBps,
			CaptureToSendP50: l.CaptureToSend.P50, CaptureToSendP95: l.CaptureToSend.P95,
		})
	}
	return out
}

// statsOf is the share.stats sample (03 §6.3): each layer's sent rate, encoder and limit (bandwidth while
// Upload.Limited, cpu while Overloaded, else none), and the audio track's rate.
func statsOf(s *core.ShareSnapshot) *Stats {
	limit := LimitNone
	switch {
	case s.Upload.Limited:
		limit = LimitBandwidth
	case s.Overloaded:
		limit = LimitCPU
	}
	st := &Stats{Layers: make([]StatsLayer, 0, len(s.Layers)), AudioSentBps: s.AudioSentBps}
	for _, l := range s.Layers {
		enc, hw := l.Encoder, l.HW
		if enc == "" {
			enc, hw = s.Encoder, s.HW
		}
		st.Layers = append(st.Layers, StatsLayer{RID: l.RID, Width: l.Width, Height: l.Height, FPS: l.FPS,
			SentBps: l.SentBps, Encoder: enc, HW: hw, Limit: limit})
	}
	return st
}

// kindOf is the share kind of a picked source: window, else screen (01's mapping for share.start).
func kindOf(k media.SourceKind) protocol.ShareKind {
	if k == media.SourceWindow {
		return protocol.ShareKindWindow
	}
	return protocol.ShareKindScreen
}

// active reports a state in which the share runs (for the pill): live, paused, reconnecting, unreachable.
func (s State) active() bool {
	return s == StateLive || s == StatePaused || s == StateReconnecting || s == StateUnreachable
}

// final reports ended or failed.
func (s State) final() bool { return s == StateEnded || s == StateFailed }

// cloneView returns a copy of v with its own slices.
func cloneView(v View) *View {
	v.Layers = slices.Clone(v.Layers)
	v.Server = slices.Clone(v.Server)
	return &v
}
