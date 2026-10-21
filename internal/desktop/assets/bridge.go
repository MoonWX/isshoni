package assets

import (
	"bytes"
	"encoding/json"
)

// BridgeJS returns the body of /desktop/bridge.js for c (§5.2):
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
