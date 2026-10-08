package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"net/url"

	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server/auth"
	"github.com/MoonWX/isshoni/internal/server/store"
)

// accountRules are the form rules of GET /info: auth's username and password lengths, in characters (03 §7.1–7.2).
var accountRules = api.AccountRules{
	UsernameMinLength: auth.UsernameMinRunes,
	UsernameMaxLength: auth.UsernameMaxRunes,
	PasswordMinLength: auth.PasswordMinRunes,
	PasswordMaxLength: auth.PasswordMaxRunes,
}

// getInfo is GET /api/v1/info (03 §12.3 #1, §12.4.1), Public.
func (a *API) getInfo(w http.ResponseWriter, r *http.Request) {
	info, err := a.info(r.Context())
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, info)
}

// info builds the Info reply:
//   - server.name is the serverName setting, or else the host of the public origin; server.version comes from
//     Deps.Info and server.publicUrl is Site.Origin;
//   - protocol comes from Deps.Info (01's range);
//   - minClientVersion and registration are the settings of the same names;
//   - setupRequired is true while no admin row exists, whatever its status: the rule of auth's SetupAvailable
//     (03 §7.8), read from the store;
//   - features has passwordReset (M1), preceded by push exactly when the push object is present;
//   - push is present when push is on (Deps.Push) and has a VAPID key.
func (a *API) info(ctx context.Context) (api.Info, error) {
	var hasAdmin bool
	err := a.d.DB.Read(ctx, func(q *store.Q) error {
		var err error
		hasAdmin, err = q.AnyAdmin()
		return err
	})
	if err != nil {
		return api.Info{}, fmt.Errorf("httpapi: info: %w", err)
	}
	set := a.d.DB.Settings().Get()
	current, minimum := a.d.Info.Protocol()
	info := api.Info{
		Server: api.InfoServer{
			Name:      serverName(set.ServerName, a.d.Site),
			Version:   a.d.Info.ServerVersion(),
			PublicURL: a.d.Site.Origin,
		},
		Protocol:         api.InfoProtocol{Current: current, Min: minimum},
		MinClientVersion: set.MinClientVersion,
		Registration:     api.RegistrationMode(set.RegistrationMode),
		SetupRequired:    !hasAdmin,
		AccountRules:     accountRules,
	}
	if a.d.Push != nil {
		if key := a.d.Push.VAPIDPublicKey(); key != "" {
			info.Push = &api.InfoPush{VAPIDPublicKey: key}
			info.Features = append(info.Features, api.InfoFeaturePush)
		}
	}
	info.Features = append(info.Features, api.InfoFeaturePasswordReset)
	return info, nil
}

// serverName is the serverName setting, or else the host name of the site's public origin (without port or
// brackets).
func serverName(setting string, site Site) string {
	if setting != "" {
		return setting
	}
	if site.Hostname != "" {
		return site.Hostname
	}
	if u, err := url.Parse(site.Origin); err == nil {
		return u.Hostname()
	}
	return ""
}
