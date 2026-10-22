package audio

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
)

// SettingsStore keeps the device-wide audio settings: 03's settings store, under the settings.json key "audio" (03
// §14.2, 05 §14.1). AudioSettings returns what json.Unmarshal of the section into a Settings gives, and
// DefaultSettings() when there is no section; SaveAudioSettings writes json.Marshal of s back.
type SettingsStore interface {
	AudioSettings() Settings
	SaveAudioSettings(s Settings) error
}

// Settings are the audio settings (05 §14.1). There is no sound mode: the built-in voice apps can't be turned off,
// and the per-share Share sound switch is the only "off".
//
// Its JSON is 05 §14.1's object. Unmarshaling gives the defaults for absent keys (so "{}" and null are
// DefaultSettings()), keeps the order of appRules (oldest decision first: the order that the 256-entry limit drops
// by) and keeps unknown keys, which marshaling writes back unchanged. The Service normalizes what it reads: it drops
// appRules entries whose id is invalid or locked (built-in voice apps, isshoni) or whose decision is not keep-out or
// include, drops unknown tip codes, and keeps the newest 256 decisions.
type Settings struct {
	// KeepOutMicUsers is "Keep out apps that use my microphone" (default true).
	KeepOutMicUsers bool `json:"keepOutMicUsers"`
	// AppRules maps an app id ("exe:kook.exe") to keep-out or include. At most 256 entries; per-run viewer overrides
	// (05 §5.9 step 4) are never stored here.
	AppRules map[string]Decision `json:"appRules"`
	// FastPath is the kill switch audio.fastPath (05 §6.4, default true); no UI in M2.
	FastPath bool `json:"fastPath"`
	// DismissedTips are the dismissed tip codes (05 §14.7).
	DismissedTips []string `json:"dismissedTips"`

	order []string                   // AppRules' ids, oldest decision first (ids not in it count as older)
	extra map[string]json.RawMessage // unknown keys, written back unchanged
}

// maxAppRules is the limit of Settings.AppRules: beyond it the oldest decision is dropped (05 §14.1).
const maxAppRules = 256

// The keys of 05 §14.1.
const (
	keyKeepOutMicUsers = "keepOutMicUsers"
	keyAppRules        = "appRules"
	keyFastPath        = "fastPath"
	keyDismissedTips   = "dismissedTips"
)

// DefaultSettings returns the defaults of 05 §14.1: mic users kept out, no decisions, the fast path on, no tip
// dismissed.
func DefaultSettings() Settings {
	return Settings{KeepOutMicUsers: true, AppRules: map[string]Decision{}, FastPath: true, DismissedTips: []string{}}
}

// UnmarshalJSON reads 05 §14.1's object: absent keys (and null) get their defaults, a known key whose value has the
// wrong type gets its default too, appRules keeps its order and unknown keys are kept.
func (s *Settings) UnmarshalJSON(b []byte) error {
	*s = DefaultSettings()
	if bytes.Equal(bytes.TrimSpace(b), []byte("null")) {
		return nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(b, &obj); err != nil {
		return fmt.Errorf("audio: settings: %w", err)
	}
	for k, v := range obj {
		switch k {
		case keyKeepOutMicUsers:
			_ = json.Unmarshal(v, &s.KeepOutMicUsers) // a wrong type keeps the default
		case keyFastPath:
			_ = json.Unmarshal(v, &s.FastPath)
		case keyDismissedTips:
			var tips []string
			if json.Unmarshal(v, &tips) == nil && tips != nil {
				s.DismissedTips = tips
			}
		case keyAppRules:
			s.AppRules, s.order = decodeAppRules(v)
		default:
			if s.extra == nil {
				s.extra = map[string]json.RawMessage{}
			}
			s.extra[k] = slices.Clone(v)
		}
	}
	return nil
}

// decodeAppRules reads the appRules object in order. Entries whose value is not a string are dropped; a duplicate id
// keeps its last value and position. Anything but an object gives no rules.
func decodeAppRules(b json.RawMessage) (map[string]Decision, []string) {
	rules := map[string]Decision{}
	var order []string
	dec := json.NewDecoder(bytes.NewReader(b))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return rules, nil
	}
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			break
		}
		id, _ := t.(string)
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			break
		}
		var d string
		if json.Unmarshal(raw, &d) != nil {
			continue
		}
		order = slices.DeleteFunc(order, func(o string) bool { return o == id })
		order = append(order, id)
		rules[id] = Decision(d)
	}
	return rules, order
}

// MarshalJSON writes 05 §14.1's object: the four keys (appRules oldest decision first), then the unknown keys it was
// read with.
func (s Settings) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	field := func(k string, v any) error {
		if buf.Len() > 1 {
			buf.WriteByte(',')
		}
		kb, _ := json.Marshal(k)
		buf.Write(kb)
		buf.WriteByte(':')
		vb, err := json.Marshal(v)
		if err != nil {
			return err
		}
		buf.Write(vb)
		return nil
	}
	tips := s.DismissedTips
	if tips == nil {
		tips = []string{}
	}
	if err := errors.Join(
		field(keyKeepOutMicUsers, s.KeepOutMicUsers),
		field(keyAppRules, orderedRules{s.AppRules, s.ruleIDs()}),
		field(keyFastPath, s.FastPath),
		field(keyDismissedTips, tips),
	); err != nil {
		return nil, err
	}
	for _, k := range slices.Sorted(maps.Keys(s.extra)) {
		if err := field(k, s.extra[k]); err != nil {
			return nil, err
		}
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// orderedRules marshals AppRules in the given order.
type orderedRules struct {
	rules map[string]Decision
	ids   []string
}

func (o orderedRules) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, id := range o.ids {
		if i > 0 {
			buf.WriteByte(',')
		}
		kb, _ := json.Marshal(id)
		vb, _ := json.Marshal(string(o.rules[id]))
		buf.Write(kb)
		buf.WriteByte(':')
		buf.Write(vb)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// ruleIDs returns AppRules' ids, oldest decision first: the ids of order that are still set, then the others (set
// without SetAppDecision) sorted, as older.
func (s Settings) ruleIDs() []string {
	ids := make([]string, 0, len(s.AppRules))
	seen := make(map[string]bool, len(s.AppRules))
	for _, id := range s.order {
		if _, ok := s.AppRules[id]; ok && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	var rest []string
	for id := range s.AppRules {
		if !seen[id] {
			rest = append(rest, id)
		}
	}
	slices.Sort(rest)
	return append(rest, ids...)
}

// clone returns a deep copy of s.
func (s Settings) clone() Settings {
	c := s
	c.AppRules = maps.Clone(s.AppRules)
	if c.AppRules == nil {
		c.AppRules = map[string]Decision{}
	}
	c.DismissedTips = slices.Clone(s.DismissedTips)
	if c.DismissedTips == nil {
		c.DismissedTips = []string{}
	}
	c.order = slices.Clone(s.order)
	c.extra = maps.Clone(s.extra)
	return c
}

// normalized returns a copy of s that the Service can use as it is (see Settings): valid, unlocked decisions only,
// at most 256 of them (the newest), known tips only, each once.
func (s Settings) normalized() Settings {
	c := s.clone()
	ids := c.ruleIDs()
	kept := ids[:0]
	for _, id := range ids {
		if !validAppID(id) || lockedApp(id) || !validDecision(c.AppRules[id]) {
			delete(c.AppRules, id)
			continue
		}
		kept = append(kept, id)
	}
	for len(kept) > maxAppRules {
		delete(c.AppRules, kept[0])
		kept = kept[1:]
	}
	c.order = slices.Clone(kept)
	var tips []string
	for _, t := range c.DismissedTips {
		if knownTip(t) && !slices.Contains(tips, t) {
			tips = append(tips, t)
		}
	}
	c.DismissedTips = tips
	if c.DismissedTips == nil {
		c.DismissedTips = []string{}
	}
	return c
}

// withDecision returns a copy of s with id's decision set to d (the newest decision), or forgotten for auto; the
// oldest decision goes when that makes more than 256.
func (s Settings) withDecision(id string, d Decision) Settings {
	c := s.clone()
	ids := slices.DeleteFunc(c.ruleIDs(), func(o string) bool { return o == id })
	delete(c.AppRules, id)
	if d != DecisionAuto {
		c.AppRules[id] = d
		ids = append(ids, id)
	}
	for len(ids) > maxAppRules {
		delete(c.AppRules, ids[0])
		ids = ids[1:]
	}
	c.order = ids
	return c
}

// decision returns the saved decision for id, or auto.
func (s Settings) decision(id string) Decision {
	if d, ok := s.AppRules[id]; ok {
		return d
	}
	return DecisionAuto
}

// knownTip reports whether code is a tip code of 05 §14.7.
func knownTip(code string) bool { return code == TipDucking }
