package api

import (
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"reflect"
	"strconv"
	"strings"
)

// Redaction (README §4 Logging, 04 §10): passwords, tokens, SDP and full push endpoints are never logged. The DTOs
// that carry one redact it in fmt output (Format: every verb, including %v, %+v, %#v and %s, directly, through a
// pointer and nested in other values) and in log/slog (LogValue: a group of the fields, keyed by their JSON names).
// JSON encoding is unchanged: the wire carries the real values.
//
// Replacements: a password or token becomes "[redacted]" (an empty one stays empty); an SDP becomes "[sdp N B]", its
// length only, like 01's protocol.SDP; a push endpoint becomes its scheme and host (04 §10 logs only push_host); a
// link whose fragment is a one-time token (CreateInviteResponse.URL, ResetLink.URL) keeps everything before the
// fragment.
//
// A new field that carries a secret needs its type's redact method updated. TestSecretFieldsRedacted fails on a field
// whose JSON name marks it as one (password, token, endpoint, offer, answer, auth) in a type without these methods.

const redacted = "[redacted]"

// secret redacts a password or token.
func secret(s string) string {
	if s == "" {
		return ""
	}
	return redacted
}

// redactSDP keeps only an SDP's length.
func redactSDP(s string) string {
	if s == "" {
		return ""
	}
	return "[sdp " + strconv.Itoa(len(s)) + " B]"
}

// redactEndpoint keeps a push endpoint's scheme and host: its path is the capability.
func redactEndpoint(s string) string {
	if s == "" {
		return ""
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return redacted
	}
	return u.Scheme + "://" + u.Host + "/" + redacted
}

// redactLink keeps a link up to its fragment, which holds a one-time token (/invite#<token>, /reset#<token>).
func redactLink(s string) string {
	if s == "" {
		return ""
	}
	base, _, ok := strings.Cut(s, "#")
	if !ok {
		return redacted
	}
	return base + "#" + redacted
}

// formatRedacted prints r, a redacted copy of orig, for verb. r's type is a local type without methods (so printing
// it cannot recurse), which %#v would name "api.plain"; the name of orig's type is put back.
func formatRedacted(f fmt.State, verb rune, orig, r any) {
	if verb == 'v' && f.Flag('#') {
		s := fmt.Sprintf("%#v", r)
		if _, fields, ok := strings.Cut(s, "{"); ok {
			s = fmt.Sprintf("%T{%s", orig, fields)
		}
		_, _ = io.WriteString(f, s)
		return
	}
	_, _ = fmt.Fprintf(f, fmt.FormatString(f, verb), r)
}

// logGroup returns the fields of r, a redacted copy of a DTO, as a slog group keyed by their JSON names. Nil
// pointers are left out and other pointers are followed.
func logGroup(r any) slog.Value {
	rv := reflect.ValueOf(r)
	rt := rv.Type()
	attrs := make([]slog.Attr, 0, rt.NumField())
	for i := range rt.NumField() {
		key, _, _ := strings.Cut(rt.Field(i).Tag.Get("json"), ",")
		v := rv.Field(i)
		if v.Kind() == reflect.Pointer {
			if v.IsNil() {
				continue
			}
			v = v.Elem()
		}
		if v.Kind() == reflect.String {
			attrs = append(attrs, slog.String(key, v.String()))
			continue
		}
		attrs = append(attrs, slog.Any(key, v.Interface()))
	}
	return slog.GroupValue(attrs...)
}

// ---- types.go ----

func (r LoginRequest) redact() any {
	type plain LoginRequest
	p := plain(r)
	p.Password = secret(p.Password)
	return p
}

// Format implements fmt.Formatter: the password is redacted for every verb.
func (r LoginRequest) Format(f fmt.State, verb rune) { formatRedacted(f, verb, r, r.redact()) }

// LogValue implements slog.LogValuer: the password is redacted.
func (r LoginRequest) LogValue() slog.Value { return logGroup(r.redact()) }

func (r RegisterRequest) redact() any {
	type plain RegisterRequest
	p := plain(r)
	p.InviteToken = secret(p.InviteToken)
	p.Password = secret(p.Password)
	return p
}

// Format implements fmt.Formatter: the invite token and the password are redacted for every verb.
func (r RegisterRequest) Format(f fmt.State, verb rune) { formatRedacted(f, verb, r, r.redact()) }

// LogValue implements slog.LogValuer: the invite token and the password are redacted.
func (r RegisterRequest) LogValue() slog.Value { return logGroup(r.redact()) }

func (r TokenRequest) redact() any {
	type plain TokenRequest
	p := plain(r)
	p.Token = secret(p.Token)
	return p
}

// Format implements fmt.Formatter: the token is redacted for every verb.
func (r TokenRequest) Format(f fmt.State, verb rune) { formatRedacted(f, verb, r, r.redact()) }

// LogValue implements slog.LogValuer: the token is redacted.
func (r TokenRequest) LogValue() slog.Value { return logGroup(r.redact()) }

func (r SetupCompleteRequest) redact() any {
	type plain SetupCompleteRequest
	p := plain(r)
	p.Token = secret(p.Token)
	p.Password = secret(p.Password)
	return p
}

// Format implements fmt.Formatter: the setup token and the password are redacted for every verb.
func (r SetupCompleteRequest) Format(f fmt.State, verb rune) { formatRedacted(f, verb, r, r.redact()) }

// LogValue implements slog.LogValuer: the setup token and the password are redacted.
func (r SetupCompleteRequest) LogValue() slog.Value { return logGroup(r.redact()) }

func (r ResetCompleteRequest) redact() any {
	type plain ResetCompleteRequest
	p := plain(r)
	p.Token = secret(p.Token)
	p.Password = secret(p.Password)
	return p
}

// Format implements fmt.Formatter: the reset token and the password are redacted for every verb.
func (r ResetCompleteRequest) Format(f fmt.State, verb rune) { formatRedacted(f, verb, r, r.redact()) }

// LogValue implements slog.LogValuer: the reset token and the password are redacted.
func (r ResetCompleteRequest) LogValue() slog.Value { return logGroup(r.redact()) }

func (r ChangePasswordRequest) redact() any {
	type plain ChangePasswordRequest
	p := plain(r)
	p.CurrentPassword = secret(p.CurrentPassword)
	p.NewPassword = secret(p.NewPassword)
	return p
}

// Format implements fmt.Formatter: both passwords are redacted for every verb.
func (r ChangePasswordRequest) Format(f fmt.State, verb rune) { formatRedacted(f, verb, r, r.redact()) }

// LogValue implements slog.LogValuer: both passwords are redacted.
func (r ChangePasswordRequest) LogValue() slog.Value { return logGroup(r.redact()) }

func (r DeleteSelfRequest) redact() any {
	type plain DeleteSelfRequest
	p := plain(r)
	p.Password = secret(p.Password)
	return p
}

// Format implements fmt.Formatter: the password is redacted for every verb.
func (r DeleteSelfRequest) Format(f fmt.State, verb rune) { formatRedacted(f, verb, r, r.redact()) }

// LogValue implements slog.LogValuer: the password is redacted.
func (r DeleteSelfRequest) LogValue() slog.Value { return logGroup(r.redact()) }

func (r CreateInviteResponse) redact() any {
	type plain CreateInviteResponse
	p := plain(r)
	p.URL = redactLink(p.URL)
	return p
}

// Format implements fmt.Formatter: the link's token is redacted for every verb.
func (r CreateInviteResponse) Format(f fmt.State, verb rune) { formatRedacted(f, verb, r, r.redact()) }

// LogValue implements slog.LogValuer: the link's token is redacted.
func (r CreateInviteResponse) LogValue() slog.Value { return logGroup(r.redact()) }

func (r PushSubscribeRequest) redact() any {
	type plain PushSubscribeRequest
	p := plain(r)
	p.Endpoint = redactEndpoint(p.Endpoint)
	return p // Keys redacts itself
}

// Format implements fmt.Formatter: only the endpoint's host is shown and the auth secret is redacted, for every verb.
func (r PushSubscribeRequest) Format(f fmt.State, verb rune) { formatRedacted(f, verb, r, r.redact()) }

// LogValue implements slog.LogValuer: only the endpoint's host is shown and the auth secret is redacted.
func (r PushSubscribeRequest) LogValue() slog.Value { return logGroup(r.redact()) }

func (k PushKeys) redact() any {
	type plain PushKeys
	p := plain(k)
	p.Auth = secret(p.Auth) // RFC 8291's authentication secret; p256dh is a public key
	return p
}

// Format implements fmt.Formatter: the auth secret is redacted for every verb.
func (k PushKeys) Format(f fmt.State, verb rune) { formatRedacted(f, verb, k, k.redact()) }

// LogValue implements slog.LogValuer: the auth secret is redacted.
func (k PushKeys) LogValue() slog.Value { return logGroup(k.redact()) }

func (r PushUnsubscribeRequest) redact() any {
	type plain PushUnsubscribeRequest
	p := plain(r)
	p.Endpoint = redactEndpoint(p.Endpoint)
	return p
}

// Format implements fmt.Formatter: only the endpoint's host is shown, for every verb.
func (r PushUnsubscribeRequest) Format(f fmt.State, verb rune) {
	formatRedacted(f, verb, r, r.redact())
}

// LogValue implements slog.LogValuer: only the endpoint's host is shown.
func (r PushUnsubscribeRequest) LogValue() slog.Value { return logGroup(r.redact()) }

func (r PatchUserRequest) redact() any {
	type plain PatchUserRequest
	p := plain(r)
	p.CurrentPassword = secret(p.CurrentPassword)
	return p
}

// Format implements fmt.Formatter: the current password is redacted for every verb.
func (r PatchUserRequest) Format(f fmt.State, verb rune) { formatRedacted(f, verb, r, r.redact()) }

// LogValue implements slog.LogValuer: the current password is redacted.
func (r PatchUserRequest) LogValue() slog.Value { return logGroup(r.redact()) }

func (r PasswordResetRequest) redact() any {
	type plain PasswordResetRequest
	p := plain(r)
	p.CurrentPassword = secret(p.CurrentPassword)
	return p
}

// Format implements fmt.Formatter: the current password is redacted for every verb.
func (r PasswordResetRequest) Format(f fmt.State, verb rune) { formatRedacted(f, verb, r, r.redact()) }

// LogValue implements slog.LogValuer: the current password is redacted.
func (r PasswordResetRequest) LogValue() slog.Value { return logGroup(r.redact()) }

func (l ResetLink) redact() any {
	type plain ResetLink
	p := plain(l)
	p.URL = redactLink(p.URL)
	return p
}

// Format implements fmt.Formatter: the link's token is redacted for every verb.
func (l ResetLink) Format(f fmt.State, verb rune) { formatRedacted(f, verb, l, l.redact()) }

// LogValue implements slog.LogValuer: the link's token is redacted.
func (l ResetLink) LogValue() slog.Value { return logGroup(l.redact()) }

// ---- conntest.go ----

func (r ConnTestRequest) redact() any {
	type plain ConnTestRequest
	p := plain(r)
	p.Offer = redactSDP(p.Offer)
	return p
}

// Format implements fmt.Formatter: only the offer's length is shown, for every verb.
func (r ConnTestRequest) Format(f fmt.State, verb rune) { formatRedacted(f, verb, r, r.redact()) }

// LogValue implements slog.LogValuer: only the offer's length is shown.
func (r ConnTestRequest) LogValue() slog.Value { return logGroup(r.redact()) }

func (r ConnTestResponse) redact() any {
	type plain ConnTestResponse
	p := plain(r)
	p.Answer = redactSDP(p.Answer)
	return p
}

// Format implements fmt.Formatter: only the answer's length is shown, for every verb.
func (r ConnTestResponse) Format(f fmt.State, verb rune) { formatRedacted(f, verb, r, r.redact()) }

// LogValue implements slog.LogValuer: only the answer's length is shown.
func (r ConnTestResponse) LogValue() slog.Value { return logGroup(r.redact()) }
