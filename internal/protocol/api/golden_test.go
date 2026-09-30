package api

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

var update = flag.Bool("update", false, "rewrite testdata/*.json from the samples in golden_test.go")

// testdata holds the golden files.
var testdata = os.DirFS("testdata")

// goldenCase is one golden file: testdata/<name>.json holds the JSON of v. Names are <Type>[.<variant>].
type goldenCase struct {
	name string
	v    any
}

// at parses an RFC 3339 timestamp for the samples.
func at(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		panic(err)
	}
	return t.UTC()
}

func ptr[T any](v T) *T { return &v }

// Sample values follow the examples of 03 §12.4 and 04 §7.7, §11.4, §13.4–§13.5 and §14.3. Values inside
// map[string]any are strings or float64, the types encoding/json decodes them to, so the round trip is exact.
func goldenCases() []goldenCase {
	alex := User{ID: "k3m9p2qxw7ht", Username: "Alex", Role: RoleAdmin}
	alexRef := UserRef{ID: "k3m9p2qxw7ht", Username: "Alex"}
	samRef := UserRef{ID: "b8f2n4r6t0vz", Username: "Sam"}
	lounge := Room{
		ID: "lounge", Name: "Lounge", IsDefault: true, CreatedAt: at("2026-10-01T12:00:00Z"),
		Live: RoomPresence{Participants: 3, Shares: 1},
	}
	movies := Room{
		ID: "p4t7w2m9k1qs", Name: "🎬 Movie night", CreatedAt: at("2026-10-02T18:30:00.250Z"),
		Live: RoomPresence{},
	}
	invite := Invite{
		ID: "h6j8k0m2n4p6", Note: "for Sam", CreatedBy: &alexRef,
		CreatedAt: at("2026-10-01T12:00:00Z"), ExpiresAt: at("2026-10-08T12:00:00Z"),
		MaxUses: 10, Uses: 0, State: InviteStateActive, RedeemedBy: []UserRef{},
	}
	roleChanged := AuditEntry{
		ID: 4811, At: at("2026-10-03T19:22:05.114Z"), Action: "user.role_changed", Outcome: AuditOutcomeOK,
		Actor:  AuditRef{Kind: "user", ID: "k3m9p2qxw7ht", Name: "Alex"},
		Target: &AuditRef{Kind: "user", ID: "b8f2n4r6t0vz", Name: "Sam"},
		IP:     "203.0.113.7", Detail: map[string]any{"from": "user", "to": "admin"},
	}
	accounts := DashboardAccounts{
		Users:            DashboardUserCounts{Active: 6, Pending: 1, Disabled: 0, Admins: 1},
		Invites:          DashboardInviteCounts{Active: 2},
		Sessions:         DashboardSessionCounts{Active: 9},
		Devices:          DashboardDeviceCounts{Linked: 0},
		RegistrationMode: RegistrationModeInvite,
		SecurityEvents:   []AuditEntry{roleChanged},
	}
	defaults := Settings{
		RegistrationMode: RegistrationModeInvite, InviteDefaultTTLHours: 168, InviteDefaultMaxUses: 10,
		UpdateCheck: true,
	}
	settings := defaults
	settings.ServerName = "Alex's server"
	settings.RegistrationMode = RegistrationModeApproval
	settings.MaxSharesPerRoom = 4
	settings.SetupWizardDone = true
	tlsIP := TLSInfo{
		Mode: TLSModeIP, Names: []string{"203.0.113.7"}, Ready: true,
		NotAfter: at("2026-10-05T10:00:00Z"), NextRenewal: at("2026-10-02T12:00:00Z"),
	}
	advertised := []AdvertisedAddr{
		{Proto: "udp", Addr: "203.0.113.7:7882", Via: TransportUDP},
		{Proto: "tcp", Addr: "203.0.113.7:443", Via: TransportTCP443},
	}
	updateInfo := &UpdateInfo{
		Latest: "0.3.1", URL: "https://github.com/MoonWX/isshoni/releases/tag/v0.3.1", Security: true,
		CheckedAt: at("2026-09-29T09:12:00Z"),
	}
	transfer := TransferInfo{
		Month: "2026-09", EgressBytes: 412000000000, IngressBytes: 98000000000, AlertGB: 1000,
		ProjectedEgressBytes: 428000000000, EgressBps: 41500000, IngressBps: 17000000,
	}
	bandwidth := BandwidthEstimate{
		Input: BandwidthInput{
			People: 5, Sharing: 2, Thumbnails: 8, Quality: BandwidthQuality1080p60, Preset: BandwidthPresetAuto,
			Hours: 2,
		},
		PerViewerMbps:   BandwidthPerViewer{Sharer: 8.13, Viewer: 8.43},
		EgressMediaMbps: 41.54, EgressWireMbps: 43.61, IngressMediaMbps: 16.86, TransferPerSessionGB: 39.2,
	}

	return []goldenCase{
		// ---- 03: info and auth ----
		{"Info", Info{
			Server:           InfoServer{Name: "Alex's server", Version: "0.1.0", PublicURL: "https://watch.example.com"},
			Protocol:         InfoProtocol{Current: 1, Min: 1},
			MinClientVersion: "0.1.0",
			Registration:     RegistrationModeInvite,
			Features:         []string{InfoFeaturePush, InfoFeaturePasswordReset},
			AccountRules:     AccountRules{UsernameMinLength: 2, UsernameMaxLength: 32, PasswordMinLength: 8, PasswordMaxLength: 128},
			Push:             &InfoPush{VAPIDPublicKey: "BEXAMPLEvapidPublicKey0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXY"},
		}},
		{"Info.setup-required", Info{
			Server:        InfoServer{Name: "watch.example.com", Version: "0.1.0", PublicURL: "https://watch.example.com"},
			Protocol:      InfoProtocol{Current: 1, Min: 1},
			Registration:  RegistrationModeInvite,
			SetupRequired: true,
			Features:      []string{InfoFeaturePasswordReset},
			AccountRules:  AccountRules{UsernameMinLength: 2, UsernameMaxLength: 32, PasswordMinLength: 8, PasswordMaxLength: 128},
		}},
		{"LoginRequest", LoginRequest{Username: "Alex", Password: "correct horse battery"}},
		{"UserResponse", UserResponse{User: alex}},
		{"RegisterRequest", RegisterRequest{
			InviteToken: "EXAMPLEinviteTOKEN0123456789abcd", Username: "太郎", Password: "a long enough passphrase",
		}},
		{"RegisterRequest.signup", RegisterRequest{Username: "sam_k", Password: "another long passphrase"}},
		{"RegisterResponse.active", RegisterResponse{
			Status: UserStatusActive, User: &User{ID: "b8f2n4r6t0vz", Username: "太郎", Role: RoleUser},
		}},
		{"RegisterResponse.pending", RegisterResponse{Status: UserStatusPending}},
		{"TokenRequest", TokenRequest{Token: "EXAMPLEinviteTOKEN0123456789abcd"}},
		{"InviteInfo", InviteInfo{
			ServerName: "Alex's server", InvitedBy: "Alex", ExpiresAt: at("2026-10-08T12:00:00Z"), UsesLeft: 9,
		}},
		// Created with isshoni admin invite create (or its creator was deleted): no invitedBy.
		{"InviteInfo.cli", InviteInfo{ServerName: "Alex's server", ExpiresAt: at("2026-10-08T12:00:00Z"), UsesLeft: 10}},
		{"SetupCompleteRequest", SetupCompleteRequest{
			Token: "EXAMPLEsetupTOKEN0123456789abcdefghijklmnop", Username: "Alex",
			Password: "correct horse battery", ServerName: "Alex's server",
		}},
		{"ResetCheckResponse", ResetCheckResponse{Username: "Sam"}},
		{"ResetCompleteRequest", ResetCompleteRequest{
			Token: "EXAMPLEresetTOKEN0123456789abcdefghijklmnop", Password: "a brand new passphrase",
		}},

		// ---- 03: me ----
		{"Me.admin", Me{
			User: User{ID: "k3m9p2qxw7ht", Username: "Alex", Role: RoleAdmin, CreatedAt: at("2026-10-01T12:00:00Z")},
			Session: &SessionInfo{
				ID: "q1w2e3r4t5y6", Name: "Chrome on Windows", CreatedAt: at("2026-10-01T12:00:00Z"),
				ExpiresAt: at("2027-03-30T12:00:00Z"), Current: true,
			},
			Permissions: Permissions{Admin: true, CreateInvites: true},
			Badges:      &Badges{PendingApprovals: 1},
		}},
		{"Me.user", Me{
			User: User{ID: "b8f2n4r6t0vz", Username: "Sam", Role: RoleUser, CreatedAt: at("2026-10-02T08:15:30.500Z")},
			Session: &SessionInfo{
				ID: "z9x8c7v6b5n4", Name: "Safari on iPhone", CreatedAt: at("2026-10-02T08:15:30.500Z"),
				ExpiresAt: at("2027-03-31T08:15:30.500Z"), Current: true,
			},
			Permissions: Permissions{},
		}},
		{"ChangePasswordRequest", ChangePasswordRequest{
			CurrentPassword: "correct horse battery", NewPassword: "a different long passphrase",
		}},
		{"DeleteSelfRequest", DeleteSelfRequest{Password: "correct horse battery"}},
		{"Empty", Empty{}},
		{"SessionsResponse", SessionsResponse{Sessions: []SessionInfo{
			{
				ID: "q1w2e3r4t5y6", Name: "Chrome on Windows", CreatedAt: at("2026-10-01T12:00:00Z"),
				LastSeenAt: at("2026-10-03T19:20:00.042Z"), LastIP: "203.0.113.7", Current: true,
			},
			{
				ID: "z9x8c7v6b5n4", Name: "Safari on iPhone", CreatedAt: at("2026-10-02T08:15:30.500Z"),
				LastSeenAt: at("2026-10-03T07:00:00Z"), LastIP: "198.51.100.23", Current: false,
			},
		}}},
		{"RevokeOthersResponse", RevokeOthersResponse{Revoked: 2}},
		{"DevicesResponse", DevicesResponse{Devices: []DeviceInfo{{
			ID: "d2f4h6k8m0p2", Name: "Alex-PC", ClientKind: "desktop", OS: "windows", AppVersion: "0.2.0",
			CreatedAt: at("2026-11-01T10:00:00Z"), LastSeenAt: at("2026-11-02T21:45:10.300Z"), LastIP: "203.0.113.7",
		}}}},
		{"DevicesResponse.empty", DevicesResponse{Devices: []DeviceInfo{}}},

		// ---- 03: rooms ----
		{"Rooms", Rooms{DefaultRoomID: "lounge", ShowRoomList: true, Rooms: []Room{lounge, movies}}},
		{"CreateRoomRequest", CreateRoomRequest{Name: "🎬 Movie night"}},
		{"PatchRoomRequest", PatchRoomRequest{Name: ptr("Games")}},
		{"RoomResponse", RoomResponse{Room: movies}},

		// ---- 03: invites ----
		{"CreateInviteRequest", CreateInviteRequest{Note: "for Sam", ExpiresInHours: 168, MaxUses: 10}},
		{"CreateInviteRequest.defaults", CreateInviteRequest{}},
		{"CreateInviteResponse", CreateInviteResponse{
			Invite: invite, URL: "https://watch.example.com/invite#EXAMPLEinviteTOKEN0123456789abcd",
		}},
		{"InvitesResponse", InvitesResponse{Invites: []Invite{
			invite,
			{
				ID: "j7k9m1n3p5r7", Note: "", CreatedBy: &alexRef,
				CreatedAt: at("2026-09-20T09:00:00Z"), ExpiresAt: at("2026-09-27T09:00:00Z"),
				MaxUses: 1, Uses: 1, State: InviteStateUsedUp, RedeemedBy: []UserRef{samRef},
			},
			{
				ID: "a1c3e5g7j9m1", Note: "", CreatedAt: at("2026-09-18T07:30:00Z"),
				ExpiresAt: at("2026-09-19T07:30:00Z"), MaxUses: 10, Uses: 0, State: InviteStateExpired,
				RedeemedBy: []UserRef{},
			},
		}}},

		// ---- 03: push subscriptions and preferences ----
		{"PushSubscribeRequest", PushSubscribeRequest{
			Endpoint: "https://fcm.googleapis.com/fcm/send/dx1EXAMPLE",
			Keys: PushKeys{
				P256dh: "BNcRdEXAMPLEp256dhKey0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXY",
				Auth:   "tBHItJI5svbpez7KI4CCXg",
			},
		}},
		{"PushSubscribeResponse", PushSubscribeResponse{ID: "m5n7p9r1s3t5"}},
		{"PushUnsubscribeRequest", PushUnsubscribeRequest{Endpoint: "https://fcm.googleapis.com/fcm/send/dx1EXAMPLE"}},
		{"PushPreferences", PushPreferences{ShareStarted: ShareStartedPrefAll, AdminAlerts: true}},

		// ---- 03: admin ----
		{"AdminUsersResponse", AdminUsersResponse{Users: []AdminUser{
			{
				ID: "b8f2n4r6t0vz", Username: "Sam", Role: RoleUser, Status: UserStatusActive,
				CreatedVia: CreatedViaInvite, CreatedAt: at("2026-10-02T08:15:30.500Z"),
				LastLoginAt: at("2026-10-02T08:15:30.500Z"), LastSeenAt: at("2026-10-03T07:00:00Z"),
				InvitedBy: &alexRef, Sessions: 2, Devices: 0, Online: true, ResetPending: false,
			},
			{
				ID: "c4d6f8h0j2k4", Username: "sam_k", Role: RoleUser, Status: UserStatusPending,
				CreatedVia: CreatedViaSignup, CreatedAt: at("2026-10-03T11:00:00Z"),
			},
		}}},
		{"PatchUserRequest", PatchUserRequest{
			Username: ptr("Samuel"), Role: ptr(RoleAdmin), Status: ptr(UserStatusDisabled),
			CurrentPassword: "correct horse battery",
		}},
		{"PatchUserRequest.role", PatchUserRequest{Role: ptr(RoleUser)}},
		{"AdminUserResponse", AdminUserResponse{User: AdminUser{
			ID: "b8f2n4r6t0vz", Username: "Samuel", Role: RoleAdmin, Status: UserStatusActive,
			CreatedVia: CreatedViaInvite, CreatedAt: at("2026-10-02T08:15:30.500Z"),
			LastLoginAt: at("2026-10-02T08:15:30.500Z"), LastSeenAt: at("2026-10-03T07:00:00Z"),
			InvitedBy: &alexRef, Sessions: 2, Online: true,
		}}},
		{"PasswordResetRequest", PasswordResetRequest{CurrentPassword: "correct horse battery"}},
		{"ResetLink", ResetLink{
			URL:       "https://watch.example.com/reset#EXAMPLEresetTOKEN0123456789abcdefghijklmnop",
			ExpiresAt: at("2026-10-04T19:22:05.114Z"),
		}},
		{"SignOutResponse", SignOutResponse{Sessions: 2, Devices: 0}},
		{"ApprovalsResponse", ApprovalsResponse{Pending: []PendingUser{{
			ID: "c4d6f8h0j2k4", Username: "sam_k", RequestedAt: at("2026-10-03T11:00:00Z"), IP: "198.51.100.23",
		}}}},
		{"RejectRequest.all", RejectRequest{All: true}},
		{"RejectAllResponse", RejectAllResponse{Rejected: 37}},
		{"SettingsResponse", SettingsResponse{Settings: settings, Defaults: defaults, Locked: []string{"updateCheck"}}},
		{"AuditPage", AuditPage{
			Entries: []AuditEntry{
				roleChanged,
				{
					ID: 4810, At: at("2026-10-03T19:01:44.020Z"), Action: "auth.throttled", Outcome: AuditOutcomeDenied,
					Actor: AuditRef{Kind: "anonymous"}, IP: "198.51.100.23",
					Detail: map[string]any{"scope": "ip", "key": "198.51.100.23"},
				},
			},
			NextBefore: ptr(int64(4810)),
		}},
		{"AuditPage.last", AuditPage{
			Entries: []AuditEntry{{
				ID: 1, At: at("2026-10-01T11:58:00Z"), Action: "setup.token_issued", Outcome: AuditOutcomeOK,
				Actor: AuditRef{Kind: "cli", Name: "cli"}, Detail: map[string]any{},
			}},
		}},
		{"DashboardAccounts", accounts},

		// ---- 03 §12.2 and 04 §9.4: the error envelope ----
		{"ErrorResponse.validation", ErrorResponse{Error: Error{
			Code: CodeValidationFailed, Fields: map[string]string{"username": FieldInvalid, "password": FieldTooCommon},
		}}},
		{"ErrorResponse.rate-limited", ErrorResponse{Error: Error{Code: CodeRateLimited, RetryAfter: 42}}},
		{"ErrorResponse.internal", ErrorResponse{Error: Error{Code: CodeInternal, RequestID: "9f2c41d07a1be355"}}},
		{"ErrorResponse.push-rejected", ErrorResponse{Error: Error{
			Code: CodePushEndpointRejected, Params: map[string]any{ParamReason: "private_address"},
		}}},
		{"ErrorResponse.limit-reached", ErrorResponse{Error: Error{
			Code: CodeLimitReached, Params: map[string]any{ParamLimit: "rooms"},
		}}},
		{"ErrorResponse.setting-locked", ErrorResponse{Error: Error{
			Code: CodeSettingLocked, Params: map[string]any{ParamField: "updateCheck"},
		}}},
		{"ErrorResponse.transport-disabled", ErrorResponse{Error: Error{
			Code: CodeTransportDisabled, Params: map[string]any{ParamTransport: "tcp443"},
		}}},

		// ---- 04: dashboard and server status ----
		{"OpsDashboard", OpsDashboard{
			GeneratedAt: at("2026-09-29T20:14:05Z"),
			Server: ServerInfo{
				Version: "0.3.0", StartedAt: at("2026-09-29T18:00:00Z"), Origin: "https://203.0.113.7",
				Process:    ProcessInfo{CPUSeconds: 1834.2, RSSBytes: 187000000, NumCPU: 2, Goroutines: 412},
				TLS:        tlsIP,
				PublicIPv4: "203.0.113.7", PublicIPv6: "", NAT: NATKindNone,
				Advertised: advertised,
				Update:     updateInfo,
			},
			Transfer: transfer,
			Media: MediaTotals{
				IngressBps: 16800000, EgressBps: 41200000, DownTracks: 18,
				PeerConnections: TransportCounts{UDP: 7, TCP443: 1, TCP7882: 0},
			},
			Rooms: []RoomLive{{
				ID: "lounge", Name: "Lounge",
				Participants: []ParticipantLive{{
					UserID: "k3m9p2qxw7ht", Username: "alex",
					Connections: []ConnectionLive{{
						ID: "c_k3v9q2m7xw4pa8d1", Kind: "web", Role: "full", Version: "0.3.0", OS: "ios",
						Transport: TransportUDP, RTTMs: 38, ConnectedAt: at("2026-09-29T19:02:11Z"),
					}},
					Watching: []string{"s_q7m2x9c4v8b1n5k3"},
				}},
				Shares: []ShareLive{{
					ShareID: "s_q7m2x9c4v8b1n5k3", OwnerUserID: "b8f2n4r6t0vz", Kind: "window",
					StartedAt: at("2026-09-29T19:05:00Z"), Codec: "h264/640c",
					Layers: []LayerLive{
						{RID: "f", Width: 1920, Height: 1080, FPS: 59.8, Bitrate: 7900000, LossPct: 0.1},
						{RID: "q", Width: 640, Height: 360, FPS: 15, Bitrate: 290000, LossPct: 0},
					},
					Viewers:    ViewerCounts{High: 3, Low: 1, Audio: 3},
					IngressBps: 8400000, EgressBps: 24300000,
				}},
			}},
			Clients: []ClientVersionCount{{Kind: "web", Version: "0.3.0", Count: 5, Outdated: false}},
			Doctor:  &DoctorSummary{RanAt: at("2026-09-29T18:00:07Z"), OK: 12, Warn: 1, Fail: 0},
			Alerts: []Alert{{
				Code: AlertCodeReleaseSecurityUpdate, Severity: AlertSeverityWarn,
				Params: map[string]any{"version": "0.3.1"},
			}},
			Accounts: &accounts,
		}},
		{"OpsDashboard.source-failed", OpsDashboard{
			GeneratedAt: at("2026-09-29T18:00:03.500Z"),
			Server: ServerInfo{
				Version: "0.3.0", StartedAt: at("2026-09-29T18:00:00Z"), Origin: "http://localhost:8080",
				Process: ProcessInfo{CPUSeconds: 0.4, RSSBytes: 52000000, NumCPU: 8, Goroutines: 37},
				TLS:     TLSInfo{Mode: TLSModeOff, Names: []string{}, Ready: true},
				NAT:     NATKindUnknown, Advertised: []AdvertisedAddr{},
			},
			Transfer: TransferInfo{Month: "2026-09"},
			Rooms:    []RoomLive{},
			Clients:  []ClientVersionCount{},
			Alerts: []Alert{{
				Code: AlertCodeDashboardSourceFailed, Severity: AlertSeverityWarn,
				Params: map[string]any{"source": "accounts"},
			}},
		}},
		{"ServerStatus", ServerStatus{
			Version: "0.3.0", StartedAt: at("2026-09-29T18:00:00Z"), UptimeS: 8045, Origin: "https://203.0.113.7",
			TLS: TLSInfo{
				Mode: TLSModeIP, Names: []string{"203.0.113.7"}, Ready: true, Issuer: "Let's Encrypt",
				NotBefore: at("2026-09-28T18:00:00Z"), NotAfter: at("2026-10-05T10:00:00Z"),
				NextRenewal: at("2026-10-02T12:00:00Z"), LastErrorCode: "tls.acme_unreachable",
				LastErrorAt: at("2026-09-28T17:58:30Z"),
			},
			PublicIPv4: "203.0.113.7", PublicIPv4Method: "interface", PublicIPv6Method: "none",
			LocalIPv4: "203.0.113.7", NAT: NATKindNone, Advertised: advertised,
			Listeners: []ListenerInfo{
				{Key: "listen.https", Network: "tcp", Addr: "[::]:443"},
				{Key: "listen.http", Network: "tcp", Addr: "[::]:80"},
				{Key: "listen.ice_udp", Network: "udp", Addr: "203.0.113.7:7882"},
				{Key: "listen.ice_tcp", Network: "tcp", Addr: "[::]:7882"},
				{Key: "listen.admin_socket", Network: "unix", Addr: "/run/isshoni/admin.sock"},
			},
			// Linux reports twice the value set with setsockopt, capped at twice rmem_max/wmem_max (212992 here).
			UDPRcvBufBytes: 425984, UDPSndBufBytes: 425984,
			Rooms: 1, Participants: 3, Shares: 1, SchemaVersion: 1,
			Transfer: &transfer, Update: updateInfo,
		}},

		// ---- 04: doctor and bandwidth ----
		{"DoctorReport", DoctorReport{
			Schema: DoctorReportSchema, Version: "0.3.0", RanAt: at("2026-09-29T20:15:00Z"), Mode: DoctorModeServer,
			Summary: DoctorCounts{OK: 12, Warn: 1, Fail: 0, Skip: 0, Info: 4},
			Env: DoctorEnv{
				OS: "linux", Arch: "amd64", Kernel: "6.8.0-45-generic", Container: ContainerKindNone, Systemd: true,
				Provider: CloudProviderHetzner, User: "isshoni",
			},
			Checks: []DoctorCheck{
				{
					ID: "udp_buffers", Status: DoctorStatusWarn, Code: "udp_buffers.low",
					Params:  map[string]any{"rmemMax": float64(212992), "want": float64(8388608)},
					Message: "net.core.rmem_max is 212992, isshoni wants 8388608",
					FixCode: "udp_buffers.fix_sysctl",
					Fix: "printf 'net.core.rmem_max=8388608\\nnet.core.wmem_max=8388608\\n' | " +
						"sudo tee /etc/sysctl.d/60-isshoni.conf && sudo sysctl --system",
					LocalOnly: false, DurationMs: 1,
				},
				{
					ID: "ports", Status: DoctorStatusInfo, Code: "ports.listening",
					Message:   "listening on 443/tcp, 80/tcp, 7882/udp, 7882/tcp (local check only)",
					LocalOnly: true, DurationMs: 0,
				},
			},
			Bandwidth: &bandwidth,
		}},
		{"BandwidthEstimate", bandwidth},

		// ---- 04: connection test ----
		{"ConnTestRequest", ConnTestRequest{
			Transport: TransportUDP,
			Offer:     "v=0\r\no=- 4611731400430051336 2 IN IP4 127.0.0.1\r\ns=-\r\nt=0 0\r\nm=application 9 UDP/DTLS/SCTP webrtc-datachannel\r\n",
		}},
		{"ConnTestResponse", ConnTestResponse{
			Answer:     "v=0\r\no=- 1 1 IN IP4 203.0.113.7\r\ns=-\r\nt=0 0\r\nm=application 7882 UDP/DTLS/SCTP webrtc-datachannel\r\n",
			ExpiresInS: 20,
			Server: ConnTestServerInfo{
				PublicIP: "203.0.113.7", Provider: CloudProviderHetzner, Container: ContainerKindNone,
				UDPPort: 7882, TCPPorts: []int{443, 7882}, NAT: NATKindNone,
			},
		}},

		// ---- 04: push payloads ----
		{"PushPayload.share-started", PushPayload{
			V: PushPayloadVersion, Type: PushTypeShareStarted, TS: 1790712000000, Tag: "share:lounge:k3m9p2qxw7ht",
			URL:  "/r/lounge?focus=s_q7m2x9c4v8b1n5k3",
			Room: &NameRef{ID: "lounge", Name: "Lounge"}, User: &NameRef{ID: "k3m9p2qxw7ht", Name: "Alex"},
			ShareID: "s_q7m2x9c4v8b1n5k3",
		}},
		{"PushPayload.admin-alert", PushPayload{
			V: PushPayloadVersion, Type: PushTypeAdminAlert, TS: 1790712000000, Tag: "admin:signup_pending",
			URL: "/admin/approvals", Kind: AdminAlertKindSignupPending, Actor: "system", Target: "sam_k",
		}},
		{"PushPayload.test", PushPayload{
			V: PushPayloadVersion, Type: PushTypeTest, TS: 1790712000000, Tag: "test", URL: "/account/notifications",
		}},
	}
}

// encodeGolden is the canonical encoding of a golden file: two-space indent, no HTML escaping (for readability; it
// decodes the same either way) and a final newline.
func encodeGolden(t *testing.T, v any) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		t.Fatalf("marshal %T: %v", v, err)
	}
	return buf.Bytes()
}

// TestGolden round-trips every DTO: the sample encodes to the golden bytes, the golden decodes (with no unknown
// fields) to a value deeply equal to the sample, and that value encodes to the same bytes again.
func TestGolden(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range goldenCases() {
		t.Run(c.name, func(t *testing.T) {
			if seen[c.name] {
				t.Fatalf("duplicate golden case %q", c.name)
			}
			seen[c.name] = true
			typeName := reflect.TypeOf(c.v).Name()
			if base, _, _ := strings.Cut(c.name, "."); base != typeName {
				t.Fatalf("case %q holds a %s; name it %s[.variant]", c.name, typeName, typeName)
			}
			path := filepath.Join("testdata", c.name+".json")
			got := encodeGolden(t, c.v)
			if *update {
				if err := os.WriteFile(path, got, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			want, err := fs.ReadFile(testdata, c.name+".json")
			if err != nil {
				t.Fatalf("%v (run go test -run TestGolden -update to create it)", err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("%s is stale or the type changed shape:\n--- got\n%s\n--- want\n%s", path, got, want)
			}

			dst := reflect.New(reflect.TypeOf(c.v))
			dec := json.NewDecoder(bytes.NewReader(want))
			dec.DisallowUnknownFields() // tests only: every key of the fixture is a field of the type
			if err := dec.Decode(dst.Interface()); err != nil {
				t.Fatalf("decode %s: %v", path, err)
			}
			decoded := dst.Elem().Interface()
			if !reflect.DeepEqual(decoded, c.v) {
				t.Fatalf("decode %s: got %#v, want %#v", path, decoded, c.v)
			}
			if again := encodeGolden(t, decoded); !bytes.Equal(again, want) {
				t.Fatalf("re-encoding %s changed it:\n%s", path, again)
			}
		})
	}
}

// TestGoldenFilesHaveCases fails on fixtures that no case produces, so testdata can't drift from the samples.
func TestGoldenFilesHaveCases(t *testing.T) {
	names := map[string]bool{}
	for _, c := range goldenCases() {
		names[c.name+".json"] = true
	}
	entries, err := os.ReadDir("testdata")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		if !names[e.Name()] {
			t.Errorf("testdata/%s has no golden case", e.Name())
		}
	}
}

// TestGoldenCoversEveryDTO checks that every exported struct type of the package appears in at least one golden
// case, directly or as a nested field.
func TestGoldenCoversEveryDTO(t *testing.T) {
	pkg := reflect.TypeOf(Error{}).PkgPath()
	reached := map[string]bool{}
	for _, c := range goldenCases() {
		walkTypes(reflect.TypeOf(c.v), func(rt reflect.Type) {
			if rt.PkgPath() == pkg && rt.Name() != "" {
				reached[rt.Name()] = true
			}
		})
	}
	for _, name := range exportedStructs(parseSources(t, ".")) {
		if !reached[name] {
			t.Errorf("%s has no golden JSON: add a case to goldenCases", name)
		}
	}
}

// TestGoldenNoNull enforces "lists are [], never null": the only null allowed is AuditPage.nextBefore.
func TestGoldenNoNull(t *testing.T) {
	allowed := map[string]bool{"AuditPage.last:.nextBefore": true}
	for _, c := range goldenCases() {
		walkJSON(t, c.name, func(path string, v any) {
			if v == nil && !allowed[c.name+":"+path] {
				t.Errorf("%s: null at %s", c.name, path)
			}
		})
	}
}

var camelCase = regexp.MustCompile(`^[a-z][a-zA-Z0-9]*$`)

// TestGoldenKeysCamelCase checks every object key in every golden file, including map keys such as Error.Fields,
// Error.Params and AuditEntry.Detail.
func TestGoldenKeysCamelCase(t *testing.T) {
	for _, c := range goldenCases() {
		walkJSON(t, c.name, func(path string, v any) {
			obj, ok := v.(map[string]any)
			if !ok {
				return
			}
			for k := range obj {
				if !camelCase.MatchString(k) {
					t.Errorf("%s: key %q at %s is not camelCase", c.name, k, path)
				}
			}
		})
	}
}

// walkJSON decodes testdata/<name>.json generically and calls fn for every value with its path.
func walkJSON(t *testing.T, name string, fn func(path string, v any)) {
	t.Helper()
	b, err := fs.ReadFile(testdata, name+".json")
	if err != nil {
		t.Fatal(err)
	}
	var root any
	if err := json.Unmarshal(b, &root); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	var walk func(path string, v any)
	walk = func(path string, v any) {
		fn(path, v)
		switch x := v.(type) {
		case map[string]any:
			keys := make([]string, 0, len(x))
			for k := range x {
				keys = append(keys, k)
			}
			slices.Sort(keys)
			for _, k := range keys {
				walk(path+"."+k, x[k])
			}
		case []any:
			for i, e := range x {
				walk(fmt.Sprintf("%s[%d]", path, i), e)
			}
		}
	}
	walk("", root)
}

// walkTypes calls fn for rt and every type reachable from it through fields, pointers, slices, arrays and maps.
func walkTypes(rt reflect.Type, fn func(reflect.Type)) {
	seen := map[reflect.Type]bool{}
	var walk func(reflect.Type)
	walk = func(rt reflect.Type) {
		if seen[rt] {
			return
		}
		seen[rt] = true
		fn(rt)
		switch rt.Kind() {
		case reflect.Pointer, reflect.Slice, reflect.Array:
			walk(rt.Elem())
		case reflect.Map:
			walk(rt.Key())
			walk(rt.Elem())
		case reflect.Struct:
			for i := range rt.NumField() {
				walk(rt.Field(i).Type)
			}
		}
	}
	walk(rt)
}
