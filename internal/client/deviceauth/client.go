package deviceauth

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/MoonWX/isshoni/internal/client/deeplink"
	"github.com/MoonWX/isshoni/internal/protocol"
	"github.com/MoonWX/isshoni/internal/protocol/api"
)

// AppInfo describes this app to the server. 03-desktop-app fills it once at start.
type AppInfo struct {
	Version    string              // version.Version()
	ClientKind protocol.ClientKind // ClientKindDesktop
	OS         protocol.ClientOS   // ClientOSWindows
	DeviceName string              // os.Hostname() cut to 64 code points, or "Windows PC"
}

// Client talks to one server's /api/v1 (02 §11.2). Safe for concurrent use.
//
// Every request sends User-Agent: isshoni-desktop/<version>, Content-Type: application/json and no Origin (a
// request without browser headers passes the server's CSRF check). A non-2xx answer with the REST error envelope is
// returned as the *api.Error (RetryAfter filled from the Retry-After header when the body has none); a transport
// failure wraps ErrNetwork and its cause; an answer the client can't use wraps ErrBadResponse. When ctx ends first,
// ctx's error is returned as it is. Response bodies are read up to 64 KiB, and no redirect is ever followed: a 307
// or 308 would re-send a token body to another location.
type Client struct {
	origin                string
	app                   AppInfo
	http                  *http.Client
	allowInsecureLoopback bool
}

// ClientOptions configures NewClient.
type ClientOptions struct {
	HTTP                  *http.Client // nil = DefaultHTTPClient(); its CheckRedirect is replaced (no redirects)
	AllowInsecureLoopback bool         // dev builds: http://localhost, 127.0.0.1, [::1]
}

// The client's limits (02 §15).
const (
	requestTimeout = 15 * time.Second
	maxBodyBytes   = 64 << 10
	apiPrefix      = "/api/v1"
	userAgentName  = "isshoni-desktop/"
	linkPath       = "/link"
	exchangeIDLen  = 16 // random bytes of an exchange id
	tokenBytes     = 32 // random bytes of a device code, an access or a refresh token
	accessPrefix   = "isa_"
	refreshPrefix  = "isr_"
	maxAppVersion  = 32 // bytes of [0-9A-Za-z.+-] the server takes as appVersion (02 §5.2)
)

// NewClient takes an origin from deeplink.NormalizeOrigin (https; http only for loopback with
// ClientOptions.AllowInsecureLoopback). The origin is normalized again, so an equivalent spelling works; one that
// doesn't normalize is refused with an error wrapping deeplink.ErrInvalidOrigin.
func NewClient(origin string, app AppInfo, o ClientOptions) (*Client, error) {
	norm, err := deeplink.NormalizeOrigin(origin, o.AllowInsecureLoopback)
	if err != nil {
		return nil, fmt.Errorf("deviceauth: %w", err)
	}
	var hc *http.Client
	if o.HTTP == nil {
		hc = DefaultHTTPClient()
	} else {
		cp := *o.HTTP // a shallow copy shares the Transport (and its connections)
		hc = &cp
	}
	hc.CheckRedirect = noRedirect
	return &Client{origin: norm, app: app, http: hc, allowInsecureLoopback: o.AllowInsecureLoopback}, nil
}

// DefaultHTTPClient returns a new client with a timeout of 15 s, TLS 1.2 or later, the system roots,
// ProxyFromEnvironment (HTTPS_PROXY; Go reads no Windows system proxy setting), and CheckRedirect returning
// http.ErrUseLastResponse: a 307/308 would re-send a token body to another location. Its Transport is an
// *http.Transport of its own, so a caller may change a copy (03-desktop-app's probe sets a 5 s connect timeout).
func DefaultHTTPClient() *http.Client {
	return &http.Client{
		Timeout: requestTimeout,
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          16,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: time.Second,
			TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
		},
		CheckRedirect: noRedirect,
	}
}

func noRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// Info is GET /api/v1/info (#1).
func (c *Client) Info(ctx context.Context) (api.Info, error) {
	var info api.Info
	if err := c.do(ctx, http.MethodGet, "/info", nil, "", &info); err != nil {
		return api.Info{}, err
	}
	return info, nil
}

// StartLink is POST /device/code (#44) with AppInfo's kind, device name, OS and version. ErrBadResponse unless the
// answer passes the verification-URI check of 02 §11.2: each URI parses, has the scheme https (http only for a
// loopback host AllowInsecureLoopback allows), no opaque part, userinfo or fragment, the client's own origin
// (normalized components, so an upper-case host or an explicit default port pass) and the path exactly /link;
// verificationUri has no query, and verificationUriComplete's query is exactly "code=" + the formatted user code,
// which must normalize. The device code must have 02 §3's shape, and interval and expiresIn must be positive. On
// success UserCode is the formatted code, and VerificationURI and VerificationURIComplete are replaced by Go's
// canonical strings, origin + "/link" and origin + "/link?code=" + UserCode.
func (c *Client) StartLink(ctx context.Context) (api.DeviceCodeResponse, error) {
	req := api.DeviceCodeRequest{
		ClientKind: string(c.app.ClientKind),
		DeviceName: c.app.DeviceName,
		OS:         string(c.app.OS),
		AppVersion: c.app.Version,
	}
	var resp api.DeviceCodeResponse
	if err := c.do(ctx, http.MethodPost, "/device/code", req, "", &resp); err != nil {
		return api.DeviceCodeResponse{}, err
	}
	return c.checkCodeResponse(resp)
}

// Poll is POST /device/token with the device_code grant (#45, 02 §5.4). authorization_pending, slow_down,
// access_denied, expired_token and invalid_grant come back as the *api.Error.
func (c *Client) Poll(ctx context.Context, deviceCode string) (api.DeviceTokenResponse, error) {
	req := api.DeviceTokenRequest{GrantType: api.GrantTypeDeviceCode, DeviceCode: deviceCode}
	return c.tokens(ctx, "/device/token", req)
}

// PasswordLink is POST /device/password (#48, 02 §14) with AppInfo's kind, OS, version and device name. The
// password is sent once and never kept.
func (c *Client) PasswordLink(ctx context.Context, username string, password protocol.Secret) (api.DeviceTokenResponse, error) {
	req := api.DevicePasswordRequest{
		Username:   username,
		Password:   password.Reveal(),
		ClientKind: string(c.app.ClientKind),
		DeviceName: c.app.DeviceName,
		OS:         string(c.app.OS),
		AppVersion: c.app.Version,
	}
	return c.tokens(ctx, "/device/password", req)
}

// Refresh is POST /device/token with the refresh_token grant (#45, 02 §6.2). exchangeID may be "" (none is sent);
// AppInfo's version goes along when it follows the server's appVersion rule, so the Devices page shows what runs.
func (c *Client) Refresh(ctx context.Context, refreshToken, exchangeID string) (api.DeviceTokenResponse, error) {
	req := api.DeviceTokenRequest{
		GrantType:    api.GrantTypeRefreshToken,
		RefreshToken: refreshToken,
		ExchangeID:   exchangeID,
	}
	if validAppVersion(c.app.Version) {
		req.AppVersion = c.app.Version
	}
	return c.tokens(ctx, "/device/token", req)
}

// Revoke is POST /device/revoke (#49) with the access token as the bearer: the server deletes this device.
func (c *Client) Revoke(ctx context.Context, accessToken string) error {
	return c.do(ctx, http.MethodPost, "/device/revoke", api.Empty{}, accessToken, nil)
}

// tokens posts body and checks the token pair it gets back.
func (c *Client) tokens(ctx context.Context, path string, body any) (api.DeviceTokenResponse, error) {
	var resp api.DeviceTokenResponse
	if err := c.do(ctx, http.MethodPost, path, body, "", &resp); err != nil {
		return api.DeviceTokenResponse{}, err
	}
	if err := checkTokenResponse(resp); err != nil {
		return api.DeviceTokenResponse{}, err
	}
	return resp, nil
}

// do sends one request and decodes a 2xx body into out (when out isn't nil).
func (c *Client) do(ctx context.Context, method, path string, body any, bearer string, out any) error {
	var rd io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("deviceauth: encode the request: %w", err)
		}
		rd = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.origin+apiPrefix+path, rd)
	if err != nil {
		return fmt.Errorf("deviceauth: build the request: %w", err)
	}
	req.Header.Set("User-Agent", userAgentName+c.app.Version)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return fmt.Errorf("%w: %w", ErrNetwork, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return fmt.Errorf("%w: %w", ErrNetwork, err)
	}
	if len(data) > maxBodyBytes {
		return badResponse(resp.StatusCode, "the body is larger than 64 KiB")
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if out == nil {
			return nil
		}
		if err := json.Unmarshal(data, out); err != nil {
			return badResponse(resp.StatusCode, "the body doesn't parse")
		}
		return nil
	}
	var env api.ErrorResponse
	if err := json.Unmarshal(data, &env); err != nil || env.Error.Code == "" {
		return badResponse(resp.StatusCode, "no error envelope")
	}
	e := env.Error
	if e.RetryAfter <= 0 {
		e.RetryAfter = 0
		if s, err := strconv.Atoi(strings.TrimSpace(resp.Header.Get("Retry-After"))); err == nil && s > 0 {
			e.RetryAfter = s
		}
	}
	return &e
}

// checkCodeResponse applies 02 §11.2's checks to a /device/code answer and returns it with Go's canonical strings.
func (c *Client) checkCodeResponse(r api.DeviceCodeResponse) (api.DeviceCodeResponse, error) {
	n, ok := api.NormalizeUserCode(r.UserCode)
	if !ok {
		return api.DeviceCodeResponse{}, badResponse(0, "the user code is not a user code")
	}
	code := api.FormatUserCode(n)
	switch {
	case !validDeviceCode(r.DeviceCode):
		return api.DeviceCodeResponse{}, badResponse(0, "the device code has the wrong shape")
	case r.Interval <= 0 || r.ExpiresIn <= 0:
		return api.DeviceCodeResponse{}, badResponse(0, "interval and expiresIn must be positive")
	}
	if why := c.checkVerificationURI(r.VerificationURI, ""); why != "" {
		return api.DeviceCodeResponse{}, badResponse(0, "verificationUri: "+why)
	}
	if why := c.checkVerificationURI(r.VerificationURIComplete, "code="+code); why != "" {
		return api.DeviceCodeResponse{}, badResponse(0, "verificationUriComplete: "+why)
	}
	r.UserCode = code
	r.VerificationURI = c.origin + linkPath
	r.VerificationURIComplete = c.origin + linkPath + "?code=" + code
	return r, nil
}

// checkVerificationURI returns why raw isn't the client's own origin + "/link" with exactly the query query, or "".
// The reasons name the rule, never the URI.
func (c *Client) checkVerificationURI(raw, query string) string {
	if strings.ContainsRune(raw, '#') {
		return "it has a fragment"
	}
	u, err := url.Parse(raw)
	switch {
	case err != nil:
		return "it doesn't parse"
	case u.Scheme != "https" && u.Scheme != "http":
		return "the scheme is neither https nor http"
	case u.Opaque != "":
		return "it has an opaque part"
	case u.User != nil:
		return "it has userinfo"
	case u.Host == "":
		return "it has no host"
	}
	origin, err := deeplink.NormalizeOrigin(u.Scheme+"://"+u.Host, c.allowInsecureLoopback)
	switch {
	case err != nil || origin != c.origin:
		return "it isn't the server's origin"
	case u.EscapedPath() != linkPath:
		return "the path isn't /link"
	case u.RawQuery != query || u.ForceQuery:
		return "the query isn't the expected one"
	}
	return ""
}

// checkTokenResponse checks a token pair before anything stores or uses it.
func checkTokenResponse(r api.DeviceTokenResponse) error {
	switch {
	case !validToken(r.AccessToken, accessPrefix):
		return badResponse(0, "the access token has the wrong shape")
	case !validToken(r.RefreshToken, refreshPrefix):
		return badResponse(0, "the refresh token has the wrong shape")
	case r.ExpiresIn <= 0:
		return badResponse(0, "expiresIn must be positive")
	case r.Device.ID == "":
		return badResponse(0, "the device has no id")
	}
	return nil
}

// validB64 reports whether s is the canonical unpadded base64url form of exactly n bytes (02 §3's shape checks).
func validB64(s string, n int) bool {
	if len(s) != base64.RawURLEncoding.EncodedLen(n) {
		return false
	}
	b, err := base64.RawURLEncoding.Strict().DecodeString(s)
	return err == nil && len(b) == n
}

func validDeviceCode(s string) bool { return validB64(s, tokenBytes) }

func validToken(s, prefix string) bool {
	rest, ok := strings.CutPrefix(s, prefix)
	return ok && validB64(rest, tokenBytes)
}

// validAppVersion is the server's appVersion rule (02 §5.2): 1–32 bytes of [0-9A-Za-z.+-].
func validAppVersion(v string) bool {
	if v == "" || len(v) > maxAppVersion {
		return false
	}
	for i := range len(v) {
		switch b := v[i]; {
		case b >= '0' && b <= '9', b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b == '.', b == '+', b == '-':
		default:
			return false
		}
	}
	return true
}

// newExchangeID draws a refresh's exchange id: 16 random bytes, 22 base64url characters (02 §3).
func newExchangeID(r io.Reader) (string, error) {
	b := make([]byte, exchangeIDLen)
	if _, err := io.ReadFull(r, b); err != nil {
		return "", fmt.Errorf("deviceauth: draw an exchange id: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// isCode reports whether err carries the REST error code.
func isCode(err error, code string) bool {
	e := apiError(err)
	return e != nil && e.Code == code
}
