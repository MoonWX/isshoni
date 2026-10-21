package auth

import (
	"context"

	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server/store"
)

// The in-app username/password fallback (M2 02 §14, decision 12; README WA30): POST /api/v1/device/password links a
// device without a browser.

// PasswordDeviceLogin links a device with the account's username and password (POST /api/v1/device/password, #48).
// It takes exactly Login's order and buckets (checkLogin), every step before the next:
//
//  1. auth-ip → 429 rate_limited;
//  2. every field before answering → 422 validation_failed: username and password as Login's, and clientKind, os,
//     deviceName and appVersion as StartDeviceFlow's (checkDeviceFields);
//  3. auth-user-ip and auth-user, reserved before the hash with Login's known-IP rule → 429 rate_limited;
//  4. auth-hash → 503 server_busy, then the hash under the semaphore (a full queue → 503 server_busy); an unknown
//     username, or an account without a password, hashes the dummy value;
//  5. a wrong password → 401 invalid_credentials with auth.login_failed; a correct password for a pending or
//     disabled account → 403 account_pending or account_disabled with the same row. Login's rehash rule applies;
//  6. one Write: the account again (currentAccount: hash unchanged and active, else as step 5; the rehash is stored),
//     then the device_code grant's success steps (linkDevice) with linked_via password and the name, kind, OS and
//     version of the request: the cap's evictions with their rows, the device, its first pair and device.linked
//     {clientKind, os, via: "password"} with the account as the actor and this request's IP.
//
// None of the device flow's buckets apply: a guess here costs what one at /auth/login costs. No web session is
// created and the account's last-login time stays. After the commit the attempt ends like a completed login (the
// auth-user token back, this address's auth-user-ip bucket refilled), the link is logged and the evicted devices'
// connections are closed; httpapi notifies {UserID}: devices.
func (s *Service) PasswordDeviceLogin(ctx context.Context, in api.DevicePasswordRequest, m ReqMeta) (api.DeviceTokenResponse, error) {
	var dev deviceFields
	v, err := s.checkLogin(ctx, "password device login", in.Username, in.Password, m, func(fields map[string]string) {
		dev = checkDeviceFields(in.ClientKind, in.OS, in.DeviceName, in.AppVersion, fields)
	})
	if err != nil {
		return api.DeviceTokenResponse{}, err
	}
	defer v.attempt.release()

	access, refresh := newDeviceTokenPair()
	now := s.now()
	var linked linkedDevice
	err = s.db.Write(ctx, func(q *store.Q) error {
		cur, err := currentAccount(q, v, now)
		if err != nil {
			return err
		}
		linked, err = s.linkDevice(q, cur, store.Device{Name: dev.Name, ClientKind: dev.ClientKind, OS: dev.OS,
			AppVersion: dev.AppVersion, LinkedVia: linkedViaPassword}, access, refresh, m, now)
		return err
	})
	if err != nil {
		return api.DeviceTokenResponse{}, serviceErr("password device login", err)
	}
	v.attempt.loggedIn()
	s.deviceLinked(ctx, linked)
	return linked.response(access, refresh), nil
}
