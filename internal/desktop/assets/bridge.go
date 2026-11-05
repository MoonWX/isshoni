package assets

import (
	"bytes"
	"encoding/json"
)

// navigationGuardJS is bridge.js's second statement (§5.2; W5's fallback, §5.5 layer 2): before any of the SPA's
// scripts, a Navigation API navigate listener cancels every cancelable navigation whose destination is not on the
// page's own origin (scheme://host, compared as text, as links.ts compares them: a custom scheme's URL.origin is
// "null"). That is a script's location.href = …, which no click listener sees and the pinned Wails gives Go no way to
// cancel; a blob: or data: URL counts as foreign, the SPA navigates to neither. A traversal to another document isn't
// cancelable (Go's navigation guard sends the window back), and the SPA's own navigations (the router's history
// entries, a reload) stay on the origin. It never opens the browser: a link the user clicked is links.ts's, and a
// script that navigates the window itself is a bug or an injection. A webview without the API (WKWebView may lack
// it) gets nothing. It lives here, not in the SPA's entry, because it must run first and DesktopPlatform's share of
// the entry has a budget (§7.2).
const navigationGuardJS = `(function(n){if(!n)return;function o(u){u=new URL(u);return u.protocol+"//"+u.host}` +
	`n.addEventListener("navigate",function(e){if(e.cancelable&&o(e.destination.url)!==o(location.href))` +
	`e.preventDefault()})})(window.navigation);`

// bridgeBody is the body of /desktop/bridge.js for c: BridgeJS's statement, then navigationGuardJS.
func bridgeBody(c BridgeConfig) ([]byte, error) {
	b, err := BridgeJS(c)
	if err != nil {
		return nil, err
	}
	return append(append(b, '\n'), navigationGuardJS...), nil
}

// BridgeJS returns the config statement of /desktop/bridge.js for c (§5.2):
//
//	window.__ISSHONI_DESKTOP__=Object.freeze(<c as JSON>);
//
// encoding/json escapes '<', '>', '&', U+2028 and U+2029, so no server name can end the script or break the
// statement. Object.freeze is shallow; the SPA reads the config once at boot and never writes it.
func BridgeJS(c BridgeConfig) ([]byte, error) {
	if c.ServerIDs == nil {
		c.ServerIDs = []string{} // an array, never null
	}
	data, err := json.Marshal(c)
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	b.Grow(len(data) + 48)
	b.WriteString("window.__ISSHONI_DESKTOP__=Object.freeze(")
	b.Write(data)
	b.WriteString(");")
	return b.Bytes(), nil
}
