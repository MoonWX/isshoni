// Package codes holds the desktop bridge's error codes (docs/m2/03-desktop-app.md §6.4): a failing bound method of
// internal/desktop/bridge returns a Go error whose Error() is the JSON {"code": <Code>, "params": {…}}, and the SPA
// parses it into BridgeError {code, params} and shows desktop.errors.<code> (03 §18.1). Codes are snake_case, stable,
// never renamed and never reused with another meaning (README §4 Errors).
//
// The package has no imports at all, so that tygo can export it to web/src/platform/desktop/codes.gen.ts
// (tygo.yaml; `task gen`, drift-checked by `task gen:check`). A new code needs its text in
// web/src/i18n/lazy/desktop.en.json: desktop.errors.<code> for every code here except the update.* codes, whose texts
// are the update screens' (desktop.update.error.<code>, README WA82). The SPA test
// web/src/platform/desktop/codes.node.test.ts fails when one is missing.
//
// Wire codes that the bridge passes through unchanged (01 §12.1's protocol.ErrorCode from C2, and the M1 REST codes
// that Auth.LinkWithPassword passes on: invalid_credentials, account_pending, account_disabled, server_busy,
// validation_failed, rate_limited) are not listed here: the SPA shows them with M1's errors.<code>.
package codes

// Code is a bridge error code.
type Code string

const (
	// CodeInvalidArgument is a bad argument from page script; params.field names it.
	CodeInvalidArgument Code = "invalid_argument"
	// CodeNotFound: the thing named doesn't exist (a server, a prompt), or the origin named isn't the active server's.
	CodeNotFound Code = "not_found"
	// CodeBusy is refused while a share is live (App.Quit, Auth.SwitchServer, Updates.Install, Audio.SoundCheck, …).
	CodeBusy Code = "busy"
	// CodeIOError: a local file or folder couldn't be opened or written.
	CodeIOError Code = "io_error"
	// CodeRateLimited: too many requests; params.retryAfter (seconds) when the server said.
	CodeRateLimited Code = "rate_limited"
	// CodeFeatureDisabled: the server lacks the feature (Share.SetPaused without share.pause).
	CodeFeatureDisabled Code = "feature_disabled"
	// CodeNotImplemented is a method whose slice hasn't landed yet (README §4 "Interfaces first"). No release has one.
	CodeNotImplemented Code = "not_implemented"
	// CodeInternal is a bug or a transport failure.
	CodeInternal Code = "internal"

	// CodeInvalidAddress: the typed address isn't a server address (03 §8.1).
	CodeInvalidAddress Code = "invalid_address"
	// CodeServerUnreachable: no answer from the server (also 02's ErrOffline).
	CodeServerUnreachable Code = "server_unreachable"
	// CodeTLSUntrusted: the server's certificate isn't trusted by the OS.
	CodeTLSUntrusted Code = "tls_untrusted"
	// CodeNotIsshoni: something answered, but not an isshoni server.
	CodeNotIsshoni Code = "not_isshoni"
	// CodeServerNotSetUp: the server's first-run setup isn't done.
	CodeServerNotSetUp Code = "server_not_set_up"
	// CodeIncompatibleServer carries 02 §13.2's compat.Check as params (verdict, reason, serverVersion,
	// minClientVersion); the SPA picks its text by verdict (02 §13.3).
	CodeIncompatibleServer Code = "incompatible_server"
	// CodeTooManyServers: 20 servers are listed (02 §11.6).
	CodeTooManyServers Code = "too_many_servers"
	// CodeNotLinked: no credential for the active server; the SPA shows the link step (03 §8.2).
	CodeNotLinked Code = "not_linked"

	// CodePublisherOffline: C2 wasn't ready within 10 s (03 §10.1).
	CodePublisherOffline Code = "publisher_offline"
	// CodeAlreadySharing: one share at a time (01 D12).
	CodeAlreadySharing Code = "already_sharing"
	// CodeNotSharing: the call needs a live share.
	CodeNotSharing Code = "not_sharing"
	// CodePickExpired: the pick was released or is older than its hold.
	CodePickExpired Code = "pick_expired"

	// CodeEnginePermissionDenied and the other engine codes are "engine." + 01's media.ErrorCode (01 §5.2), with
	// params.reason = media.Error.Reason (04 §5.7's detail token, "" when none). The SPA chooses
	// desktop.errors.engine.reason.<reason> before the code's own text (03 §10.5). media's "cancelled" is never
	// surfaced: a cancelled Pick returns null.
	CodeEnginePermissionDenied Code = "engine.permission_denied"
	CodeEngineEncoderLost      Code = "engine.encoder_lost"
	CodeEngineSourceGone       Code = "engine.source_gone"
	CodeEngineUnsupportedOS    Code = "engine.unsupported_os"
	CodeEngineNoEncoder        Code = "engine.no_encoder"
	// CodeEngineDependency: FFmpeg or OpenH264 missing, the wrong version or blocked; reason abi asks for a reinstall.
	CodeEngineDependency       Code = "engine.dependency"
	CodeEngineAudioUnavailable Code = "engine.audio_unavailable"
	CodeEngineAudioNotRunning  Code = "engine.audio_not_running"
	CodeEngineStopped          Code = "engine.stopped"
	CodeEngineTimeout          Code = "engine.timeout"
	CodeEngineInternal         Code = "engine.internal"
	// CodeEngineInvalid shares engine.internal's text.
	CodeEngineInvalid Code = "engine.invalid"
	// CodeEngineBusy shares engine.internal's text.
	CodeEngineBusy Code = "engine.busy"

	// CodeUpdateNetwork and the other update codes are "update." + 06 §8.10's code, plus update.not_ready
	// (Updates.Install without a verified installer). Their texts are desktop.update.error.<code> (03 §6.4), written
	// with the update screens (README WA82).
	CodeUpdateNetwork             Code = "update.network"
	CodeUpdateRateLimited         Code = "update.rate_limited"
	CodeUpdateBadSignature        Code = "update.bad_signature"
	CodeUpdateBadManifest         Code = "update.bad_manifest"
	CodeUpdateManifestUnsupported Code = "update.manifest_unsupported"
	CodeUpdateManifestExpired     Code = "update.manifest_expired"
	CodeUpdateWrongChannel        Code = "update.wrong_channel"
	CodeUpdateNoArtifact          Code = "update.no_artifact"
	CodeUpdateDownloadFailed      Code = "update.download_failed"
	CodeUpdateSizeMismatch        Code = "update.size_mismatch"
	CodeUpdateHashMismatch        Code = "update.hash_mismatch"
	CodeUpdateDisk                Code = "update.disk"
	CodeUpdateInstallFailed       Code = "update.install_failed"
	CodeUpdateNotInstalled        Code = "update.not_installed"
	CodeUpdateNoKeys              Code = "update.no_keys"
	CodeUpdateNotRelease          Code = "update.not_release"
	CodeUpdateNotReady            Code = "update.not_ready"
)

const (
	// EnginePrefix starts every engine code.
	EnginePrefix = "engine."
	// UpdatePrefix starts every update code.
	UpdatePrefix = "update."
)
