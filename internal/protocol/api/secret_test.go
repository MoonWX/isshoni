package api

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"testing"
)

// Every secret in the samples below contains this marker; no fmt or slog output may.
const marker = "S3CR3T"

// leaks reports whether out shows any secret of the samples, also hex-encoded (%x, %X).
func leaks(out string) bool {
	h := hex.EncodeToString([]byte(marker))
	return strings.Contains(out, marker) || strings.Contains(out, h) || strings.Contains(out, strings.ToUpper(h))
}

const (
	testPassword = "correct horse " + marker
	testToken    = "EXAMPLE" + marker + "token0123456789abcdefghijklm"
	testEndpoint = "https://fcm.googleapis.com/fcm/send/dx1" + marker + "endpoint"
	testSDP      = "v=0\r\no=- 1 1 IN IP4 203.0.113.7\r\ns=" + marker + "\r\nt=0 0\r\n" +
		"a=candidate:1 1 udp 2130706431 203.0.113.7 7882 typ host\r\n"
)

// secretSamples holds one value of every type that carries a secret, and a non-secret part of it that must stay
// visible in %+v and in the slog output (so redaction doesn't blank the whole value).
func secretSamples() []struct {
	v       any
	visible string
} {
	return []struct {
		v       any
		visible string
	}{
		{LoginRequest{Username: "Alex", Password: testPassword}, "Alex"},
		{RegisterRequest{InviteToken: testToken, Username: "Alex", Password: testPassword}, "Alex"},
		{TokenRequest{Token: testToken}, redacted},
		{SetupCompleteRequest{Token: testToken, Username: "Alex", Password: testPassword, ServerName: "Home"}, "Home"},
		{ResetCompleteRequest{Token: testToken, Password: testPassword}, redacted},
		{ChangePasswordRequest{CurrentPassword: testPassword, NewPassword: "new " + testPassword}, redacted},
		{DeleteSelfRequest{Password: testPassword}, redacted},
		{PatchUserRequest{Role: ptr(RoleAdmin), CurrentPassword: testPassword}, redacted},
		{PasswordResetRequest{CurrentPassword: testPassword}, redacted},
		{CreateInviteResponse{
			Invite: Invite{ID: "h6j8k0m2n4p6", Note: "for Sam"},
			URL:    "https://watch.example.com/invite#" + testToken,
		}, "https://watch.example.com/invite#[redacted]"},
		{ResetLink{URL: "https://watch.example.com/reset#" + testToken}, "https://watch.example.com/reset#[redacted]"},
		{PushSubscribeRequest{
			Endpoint: testEndpoint,
			Keys:     PushKeys{P256dh: "BNcRdEXAMPLEp256dh", Auth: "tBHI" + marker + "Xg"},
		}, "https://fcm.googleapis.com/[redacted]"},
		{PushKeys{P256dh: "BNcRdEXAMPLEp256dh", Auth: "tBHI" + marker + "Xg"}, "BNcRdEXAMPLEp256dh"},
		{PushUnsubscribeRequest{Endpoint: testEndpoint}, "https://fcm.googleapis.com/[redacted]"},
		{ConnTestRequest{Transport: TransportUDP, Offer: testSDP}, fmt.Sprintf("[sdp %d B]", len(testSDP))},
		{ConnTestResponse{
			Answer: testSDP, ExpiresInS: 20, Server: ConnTestServerInfo{Provider: CloudProviderHetzner},
		}, fmt.Sprintf("[sdp %d B]", len(testSDP))},

		// M2 02 §3: device codes, access and refresh tokens, exchange ids and the link form's password (the user code
		// and device names these types also hide: TestDeviceSecretsRedacted).
		{DeviceCodeResponse{
			DeviceCode: testToken, UserCode: "WDJB-MJHT", VerificationURI: "https://watch.example.com/link",
			VerificationURIComplete: "https://watch.example.com/link?code=WDJB-MJHT", ExpiresIn: 600, Interval: 5,
		}, "https://watch.example.com/link"},
		{DeviceTokenRequest{GrantType: GrantTypeDeviceCode, DeviceCode: testToken}, "device_code"},
		{DeviceTokenRequest{
			GrantType: GrantTypeRefreshToken, RefreshToken: "isr_" + testToken, ExchangeID: "q9V3" + marker + "Rk7aZ4cW",
			AppVersion: "0.2.1",
		}, "0.2.1"},
		{DeviceTokenResponse{
			AccessToken: "isa_" + testToken, TokenType: "Bearer", ExpiresIn: 900, RefreshToken: "isr_" + testToken,
			RefreshExpiresIn: 7776000, Device: DeviceRef{ID: "m4q8x2c6v0bn", Name: "Gaming PC"},
			User: User{ID: "k3m9p2qxw7ht", Username: "Alex", Role: RoleAdmin},
		}, "m4q8x2c6v0bn"},
		{DevicePasswordRequest{
			Username: "Alex", Password: testPassword, ClientKind: "desktop", DeviceName: "DESKTOP-7F3K2LQ",
			OS: "windows", AppVersion: "0.2.0",
		}, "Alex"},
	}
}

// TestSecretsNeverFormatted: no fmt verb shows a secret, whether the value is printed directly, through a pointer, or
// nested in a struct, slice, map or error; %#v still names the real type.
func TestSecretsNeverFormatted(t *testing.T) {
	verbs := []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X", "%d", "%10.3s", "%-8v", "%T"}
	for _, s := range secretSamples() {
		name := reflect.TypeOf(s.v).Name()
		p := reflect.New(reflect.TypeOf(s.v))
		p.Elem().Set(reflect.ValueOf(s.v))
		forms := map[string]any{
			"value":   s.v,
			"pointer": p.Interface(),
			"struct":  struct{ Req any }{s.v},
			"slice":   []any{s.v, p.Interface()},
			"map":     map[string]any{"req": s.v},
		}
		for form, v := range forms {
			for _, verb := range verbs {
				if out := fmt.Sprintf(verb, v); leaks(out) {
					t.Errorf("%s %s with %s leaks: %s", name, form, verb, out)
				}
			}
			for _, out := range []string{fmt.Sprint(v), fmt.Sprintln(v), fmt.Errorf("wrapped: %v", v).Error()} {
				if leaks(out) {
					t.Errorf("%s %s leaks through Sprint/Errorf: %s", name, form, out)
				}
			}
		}
		if out := fmt.Sprintf("%+v", s.v); !strings.Contains(out, s.visible) {
			t.Errorf("%s: %%+v = %s, want it to show %q", name, out, s.visible)
		}
		if out, want := fmt.Sprintf("%#v", s.v), "api."+name+"{"; !strings.HasPrefix(out, want) {
			t.Errorf("%s: %%#v = %s, want the prefix %s", name, out, want)
		}
		if out, want := fmt.Sprintf("%#v", p.Interface()), "api."+name+"{"; !strings.HasPrefix(out, want) {
			t.Errorf("%s: %%#v of a pointer = %s, want the prefix %s", name, out, want)
		}
	}
}

// TestSecretsNeverLogged: slog's JSON and text handlers show no secret, for values and pointers, at the top level and
// inside groups, and keep the fields under their JSON names.
func TestSecretsNeverLogged(t *testing.T) {
	handlers := map[string]func(*bytes.Buffer) slog.Handler{
		"json": func(b *bytes.Buffer) slog.Handler { return slog.NewJSONHandler(b, nil) },
		"text": func(b *bytes.Buffer) slog.Handler { return slog.NewTextHandler(b, nil) },
	}
	for _, s := range secretSamples() {
		name := reflect.TypeOf(s.v).Name()
		p := reflect.New(reflect.TypeOf(s.v))
		p.Elem().Set(reflect.ValueOf(s.v))
		for hname, newHandler := range handlers {
			var b bytes.Buffer
			log := slog.New(newHandler(&b))
			log.Info("req", "req", s.v)
			log.Info("req", slog.Any("req", p.Interface()))
			log.Info("req", slog.Group("g", slog.Any("req", s.v)))
			log.With("req", s.v).Info("with")
			out := b.String()
			if leaks(out) {
				t.Errorf("%s through the %s handler leaks:\n%s", name, hname, out)
			}
			if !strings.Contains(out, s.visible) {
				t.Errorf("%s through the %s handler hides %q:\n%s", name, hname, s.visible, out)
			}
		}
	}

	// The group uses the JSON names, so logx's ReplaceAttr key list (04 §10) applies to the fields too.
	var b bytes.Buffer
	slog.New(slog.NewJSONHandler(&b, nil)).Info("m", "req", RegisterRequest{InviteToken: testToken, Username: "Alex"})
	var line struct {
		Req map[string]string `json:"req"`
	}
	if err := json.Unmarshal(b.Bytes(), &line); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"inviteToken": redacted, "username": "Alex", "password": ""}
	if !reflect.DeepEqual(line.Req, want) {
		t.Errorf("logged %v, want %v", line.Req, want)
	}
}

// TestSecretsStayOnTheWire: redaction is for fmt and slog only; JSON carries the real values.
func TestSecretsStayOnTheWire(t *testing.T) {
	for _, s := range secretSamples() {
		b, err := json.Marshal(s.v)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), marker) || strings.Contains(string(b), redacted) {
			t.Errorf("%T encodes %s, want the real secret", s.v, b)
		}
	}
}

// secretName matches the JSON names of fields that carry a secret: M2 02 §3 adds device codes and exchange ids.
func secretName(name string) bool {
	n := strings.ToLower(name)
	return strings.Contains(n, "password") || strings.Contains(n, "token") || strings.Contains(n, "endpoint") ||
		n == "offer" || n == "answer" || n == "auth" || n == "devicecode" || n == "exchangeid"
}

// TestSecretFieldsRedacted: every string field whose JSON name marks it as a secret belongs to a type that redacts
// itself in fmt and slog, and every such type has a sample above. Links with a token fragment can't be told by name
// (UpdateInfo.URL is public), so CreateInviteResponse and ResetLink are listed by hand.
func TestSecretFieldsRedacted(t *testing.T) {
	formatter := reflect.TypeFor[fmt.Formatter]()
	valuer := reflect.TypeFor[slog.LogValuer]()
	sampled := map[reflect.Type]bool{}
	for _, s := range secretSamples() {
		sampled[reflect.TypeOf(s.v)] = true
	}
	need := map[reflect.Type]bool{
		reflect.TypeFor[CreateInviteResponse](): true,
		reflect.TypeFor[ResetLink]():            true,
	}
	for _, rt := range dtoTypes() {
		for i := range rt.NumField() {
			f := rt.Field(i)
			name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
			if f.Type.Kind() == reflect.String && secretName(name) {
				need[rt] = true
			}
		}
	}
	for rt := range need {
		if !rt.Implements(formatter) || !rt.Implements(valuer) {
			t.Errorf("%s carries a secret: give it Format and LogValue in secret.go", rt.Name())
		}
		if !sampled[rt] {
			t.Errorf("%s carries a secret but has no sample in secretSamples", rt.Name())
		}
	}
	if len(need) < 20 {
		t.Errorf("found only %d types with secrets", len(need))
	}
}

// TestDeviceSecretsRedacted pins M2 02 §16.1's list: in a DTO's fmt and slog output, deviceCode, accessToken,
// refreshToken, exchangeId and DevicePasswordRequest.password are "[redacted]", under their JSON names in slog, while
// the other fields stay readable. The user code and device names, never logged either (M2 README §4 Logging, 02 §3),
// are hidden by the types that redact anyway: DeviceCodeResponse's userCode and verificationUriComplete (which carries
// the code), DeviceTokenResponse's device.name and DevicePasswordRequest's deviceName.
func TestDeviceSecretsRedacted(t *testing.T) {
	const userCode, deviceName = "WDJB-MJHT", "Gaming PC"
	cases := []struct {
		v       any
		hidden  []string // JSON names whose value must be "[redacted]" in the slog group
		visible string   // a field that stays readable
	}{
		{DeviceCodeResponse{
			DeviceCode: testToken, UserCode: userCode, VerificationURI: "https://watch.example.com/link",
			VerificationURIComplete: "https://watch.example.com/link?code=" + userCode,
		}, []string{"deviceCode", "userCode", "verificationUriComplete"}, "https://watch.example.com/link"},
		{DeviceTokenRequest{
			GrantType: GrantTypeRefreshToken, DeviceCode: testToken, RefreshToken: "isr_" + testToken,
			ExchangeID: marker + "exchange",
		}, []string{"deviceCode", "refreshToken", "exchangeId"}, "refresh_token"},
		{DeviceTokenResponse{
			AccessToken: "isa_" + testToken, RefreshToken: "isr_" + testToken, TokenType: "Bearer",
			Device: DeviceRef{ID: "m4q8x2c6v0bn", Name: deviceName},
		}, []string{"accessToken", "refreshToken"}, "m4q8x2c6v0bn"},
		{DevicePasswordRequest{Username: "Alex", Password: testPassword, DeviceName: deviceName},
			[]string{"password", "deviceName"}, "Alex"},
	}
	for _, c := range cases {
		var b bytes.Buffer
		slog.New(slog.NewJSONHandler(&b, nil)).Info("m", "req", c.v)
		var line struct {
			Req map[string]any `json:"req"`
		}
		if err := json.Unmarshal(b.Bytes(), &line); err != nil {
			t.Fatal(err)
		}
		for _, key := range c.hidden {
			if line.Req[key] != redacted {
				t.Errorf("%T: slog shows %s = %v, want %q", c.v, key, line.Req[key], redacted)
			}
		}
		if dev, ok := line.Req["device"].(map[string]any); ok && dev["name"] != redacted {
			t.Errorf("%T: slog shows device.name = %v, want %q", c.v, dev["name"], redacted)
		}
		fmtOut := fmt.Sprintf("%+v", c.v)
		if leaks(fmtOut) || !strings.Contains(fmtOut, redacted) {
			t.Errorf("%T: %%+v = %s", c.v, fmtOut)
		}
		for _, out := range []string{fmtOut, fmt.Sprintf("%#v", c.v), b.String()} {
			if strings.Contains(out, "WDJB") || strings.Contains(out, deviceName) {
				t.Errorf("%T shows the user code or the device name: %s", c.v, out)
			}
			if !strings.Contains(out, c.visible) {
				t.Errorf("%T hides %q: %s", c.v, c.visible, out)
			}
		}
	}

	// An empty secret stays empty, so a log line still tells which grant a request used.
	var b bytes.Buffer
	slog.New(slog.NewJSONHandler(&b, nil)).Info("m", "req", DeviceTokenRequest{GrantType: GrantTypeDeviceCode})
	if !strings.Contains(b.String(), `"grantType":"device_code","deviceCode":"","refreshToken":"","exchangeId":""`) {
		t.Errorf("logged %s", b.String())
	}
}

// TestRedactHelpers covers the shapes of the helpers' input that the samples don't.
func TestRedactHelpers(t *testing.T) {
	cases := []struct{ got, want string }{
		{secret(""), ""},
		{redactSDP(""), ""},
		{redactEndpoint(""), ""},
		{
			redactEndpoint("https://user:" + marker + "@push.example.net:8443/p/" + marker),
			"https://push.example.net:8443/[redacted]",
		},
		{redactEndpoint("not a url " + marker), redacted},
		{redactEndpoint("%zz" + marker), redacted},
		{redactLink(""), ""},
		{redactLink("https://watch.example.com/invite?t=" + marker), redacted},
	}
	for i, c := range cases {
		if c.got != c.want {
			t.Errorf("case %d: got %q, want %q", i, c.got, c.want)
		}
	}
}
