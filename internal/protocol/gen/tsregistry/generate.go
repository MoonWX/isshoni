package main

import (
	"bytes"
	"errors"
	"fmt"
	"go/token"
	"reflect"
	"regexp"
	"slices"
	"strings"

	"github.com/MoonWX/isshoni/internal/protocol"
)

// protocolPkg is the import path of package protocol. Every payload type must be a named struct type of that
// package: tygo generates exactly that package into types.gen.ts, where the TypeScript interface has the Go name.
var protocolPkg = reflect.TypeFor[protocol.Empty]().PkgPath()

// dottedName matches message types and features: dotted lowercase words (01 §5, §6.2). Such a name can go into a
// single-quoted TypeScript string or property name without escaping.
var dottedName = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)*$`)

// bareName matches the message types that need no quotes as a TypeScript property name.
var bareName = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

type request struct {
	typ, data, result, reply, doc string
}

type message struct {
	typ, payload, doc string
}

type feature struct {
	typ, feature string
}

// tsRegistry is protocol.Registry as the TypeScript file needs it. Every list is in Registry order.
type tsRegistry struct {
	requests      []request // client->server requests
	notifications []message // client->server notifications
	server        []message // server->client replies and notifications
	types         []string  // every message type once
	features      []feature // message types behind a feature
}

// Generate renders registry.gen.ts for specs (protocol.Registry). It fails when a Spec cannot be expressed in
// TypeScript: a payload that is not a named struct type of package protocol, a duplicate or two-way entry, a kind
// the generator does not know for its direction (server->client requests do not exist in v1), a request whose reply
// type the server never sends, or one message type with different features per direction.
func Generate(specs []protocol.Spec) ([]byte, error) {
	r, err := buildRegistry(specs)
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	r.render(&b)
	return b.Bytes(), nil
}

func buildRegistry(specs []protocol.Spec) (*tsRegistry, error) {
	var (
		r        tsRegistry
		errs     []error
		dirs     = map[protocol.MessageType]protocol.Direction{}
		features = map[protocol.MessageType]protocol.Feature{}
	)
	fail := func(i int, s protocol.Spec, format string, args ...any) {
		errs = append(errs, fmt.Errorf("protocol.Registry[%d] %q: %s", i, s.Type, fmt.Sprintf(format, args...)))
	}
	for i, s := range specs {
		if !dottedName.MatchString(string(s.Type)) {
			fail(i, s, "a message type is dotted lowercase words (01 §5)")
			continue
		}
		if s.Dir != protocol.DirClientToServer && s.Dir != protocol.DirServerToClient {
			fail(i, s, "Dir %d is not exactly one direction", s.Dir)
			continue
		}
		if dirs[s.Type]&s.Dir != 0 {
			fail(i, s, "a second entry for direction %d", s.Dir)
			continue
		}
		if _, ok := dirs[s.Type]; !ok {
			r.types = append(r.types, string(s.Type))
		}
		dirs[s.Type] |= s.Dir

		if s.Feature != "" && !dottedName.MatchString(string(s.Feature)) {
			fail(i, s, "feature %q is not dotted lowercase words (01 §6.2)", s.Feature)
			continue
		}
		if f, ok := features[s.Type]; !ok {
			features[s.Type] = s.Feature
			if s.Feature != "" {
				r.features = append(r.features, feature{string(s.Type), string(s.Feature)})
			}
		} else if f != s.Feature {
			fail(i, s, "feature %q differs from the other direction's %q", s.Feature, f)
			continue
		}

		doc, err := docOf(s)
		if err != nil {
			fail(i, s, "%v", err)
			continue
		}
		switch {
		case s.Dir == protocol.DirClientToServer && s.Kind == protocol.KindRequest:
			data, errData := tsType(s.Payload)
			result, errResult := tsType(s.Result)
			if err := errors.Join(errData, errResult); err != nil {
				fail(i, s, "%v", err)
				continue
			}
			if !dottedName.MatchString(string(s.Reply)) {
				fail(i, s, "a request needs a Reply type")
				continue
			}
			r.requests = append(r.requests, request{string(s.Type), data, result, string(s.Reply), doc})
		case s.Dir == protocol.DirClientToServer && s.Kind == protocol.KindNotification:
			payload, err := tsType(s.Payload)
			if err != nil {
				fail(i, s, "%v", err)
				continue
			}
			r.notifications = append(r.notifications, message{string(s.Type), payload, doc})
		case s.Dir == protocol.DirServerToClient && (s.Kind == protocol.KindReply || s.Kind == protocol.KindNotification):
			payload := "" // nil (ok): the results of the requests it answers, filled in below
			if s.Payload != nil || s.Kind != protocol.KindReply {
				var err error
				if payload, err = tsType(s.Payload); err != nil {
					fail(i, s, "%v", err)
					continue
				}
			}
			r.server = append(r.server, message{string(s.Type), payload, doc})
		default:
			fail(i, s, "kind %d is not supported for direction %d (only clients send requests in v1)", s.Kind, s.Dir)
		}
	}

	serverTypes := map[string]bool{}
	for i, m := range r.server {
		serverTypes[m.typ] = true
		if m.payload != "" {
			continue
		}
		var union []string
		for _, q := range r.requests {
			if q.reply == m.typ && !slices.Contains(union, q.result) {
				union = append(union, q.result)
			}
		}
		if len(union) == 0 {
			errs = append(errs, fmt.Errorf("%q: a reply without a payload answers no request, so it has no type", m.typ))
			continue
		}
		r.server[i].payload = strings.Join(union, " | ")
	}
	for _, q := range r.requests {
		if !serverTypes[q.reply] {
			errs = append(errs, fmt.Errorf("%q: its reply %q is not a server->client message", q.typ, q.reply))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return &r, nil
}

// tsType returns the TypeScript name of a payload type: P.<Go name>, where P is types.gen.ts.
func tsType(v any) (string, error) {
	if v == nil {
		return "", errors.New("no payload type (use protocol.Empty{} for a message without fields)")
	}
	t := reflect.TypeOf(v)
	if t.Kind() != reflect.Struct || t.PkgPath() != protocolPkg || !token.IsIdentifier(t.Name()) ||
		!token.IsExported(t.Name()) {
		return "", fmt.Errorf("payload %v is not a named struct type of package protocol (types.gen.ts has no TypeScript type for it)", t)
	}
	return "P." + t.Name(), nil
}

// docOf returns the one-line comment for a message's entry: its milestone when not M1, its feature, its roles.
func docOf(s protocol.Spec) (string, error) {
	var parts []string
	if s.Milestone != "" && s.Milestone != "M1" {
		parts = append(parts, "Milestone "+s.Milestone+".")
	}
	if s.Since > 1 {
		parts = append(parts, fmt.Sprintf("Since protocol version %d.", s.Since))
	}
	if s.Feature != "" {
		parts = append(parts, "Feature "+string(s.Feature)+".")
	}
	if s.Roles != nil {
		roles := make([]string, len(s.Roles))
		for i, r := range s.Roles {
			roles[i] = string(r)
		}
		parts = append(parts, "Roles: "+strings.Join(roles, ", ")+".")
	}
	doc := strings.Join(parts, " ")
	if strings.Contains(doc, "*/") || strings.ContainsAny(doc, "\r\n") {
		return "", fmt.Errorf("milestone or roles %q do not fit in a comment", doc)
	}
	return doc, nil
}

// prop is a message type as a TypeScript property name.
func prop(typ string) string {
	if bareName.MatchString(typ) {
		return typ
	}
	return "'" + typ + "'"
}

func (r *tsRegistry) render(b *bytes.Buffer) {
	p := func(format string, args ...any) { fmt.Fprintf(b, format, args...) }
	doc := func(d string) {
		if d != "" {
			p("  /** %s */\n", d)
		}
	}

	p(`// Code generated by tsregistry from protocol.Registry (internal/protocol/registry.go). DO NOT EDIT.
// Regenerate with "task gen" (docs/m1/01-protocol.md §14.4). The payload types are tygo's, in types.gen.ts.

import type * as P from './types.gen';

/**
 * Client→server requests (01 §7): each carries an id and gets exactly one reply, an error or a message of type reply
 * (ok, or welcome for hello) with the payload result. SignalClient.request sends data and resolves with result; the
 * SignalClient sends hello itself.
 */
export interface ClientRequests {
`)
	for _, q := range r.requests {
		doc(q.doc)
		p("  %s: { data: %s; result: %s; reply: '%s' };\n", prop(q.typ), q.data, q.result, q.reply)
	}
	p(`}

/** Client→server notifications (no id, no reply): the payload per type, as SignalClient.notify sends it. */
export interface ClientNotifications {
`)
	for _, m := range r.notifications {
		doc(m.doc)
		p("  %s: %s;\n", prop(m.typ), m.payload)
	}
	p(`}

/**
 * Server→client messages, replies and notifications: the payload per type. ok carries the result of the request it
 * answers (ClientRequests[type]['result']); error is a reply when it has a re, else a notification (01 §8.13).
 * Clients ignore types they do not know.
 */
export interface ServerMessages {
`)
	for _, m := range r.server {
		doc(m.doc)
		p("  %s: %s;\n", prop(m.typ), m.payload)
	}
	p(`}

export type ClientRequestType = keyof ClientRequests;
export type ClientNotificationType = keyof ClientNotifications;
export type ClientMessageType = ClientRequestType | ClientNotificationType;
export type ServerMessageType = keyof ServerMessages;

/**
 * A server→client message, discriminated by type. re is set on replies. data is always set: an absent data means {}
 * on the wire (01 §5), and the SignalClient fills it in.
 */
export type ServerEnvelope<K extends ServerMessageType = ServerMessageType> = {
  [T in K]: { type: T; re?: string; data: ServerMessages[T] };
}[K];

/**
 * A client→server message, discriminated by type: requests carry an id (1–32 chars [A-Za-z0-9_-], unique among the
 * client's outstanding requests), notifications none.
 */
export type ClientEnvelope<K extends ClientMessageType = ClientMessageType> = {
  [T in K]: T extends ClientRequestType
    ? { type: T; id: string; data: ClientRequests[T]['data'] }
    : T extends ClientNotificationType
      ? { type: T; data: ClientNotifications[T] }
      : never;
}[K];

`)
	list := func(doc, name string, types []string) {
		p("/** %s */\nexport const %s = [", doc, name)
		if len(types) > 0 {
			p("\n")
			for _, t := range types {
				p("  '%s',\n", t)
			}
		}
		p("] as const satisfies readonly P.MessageType[];\n\n")
	}
	requestType := func(q request) string { return q.typ }
	messageType := func(m message) string { return m.typ }
	list("Client→server request types.", "clientRequestTypes", typesOf(r.requests, requestType))
	list("Client→server notification types.", "clientNotificationTypes", typesOf(r.notifications, messageType))
	list("Server→client message types (replies and notifications).", "serverMessageTypes", typesOf(r.server, messageType))
	list("Every message type, both directions.", "messageTypes", r.types)

	p(`/** The reply type of each request: ok, or welcome for hello. */
export const requestReplies: { readonly [K in ClientRequestType]: ClientRequests[K]['reply'] } = {
`)
	for _, q := range r.requests {
		p("  %s: '%s',\n", prop(q.typ), q.reply)
	}
	p(`};

/**
 * The feature each message type behind one needs (01 §6.2); the others are baseline. A feature is active only when
 * welcome.features lists it.
 */
export const messageFeatures: Readonly<Partial<Record<P.MessageType, P.Feature>>> = {
`)
	for _, f := range r.features {
		p("  %s: '%s',\n", prop(f.typ), f.feature)
	}
	p("};\n")
}

func typesOf[T any](list []T, typ func(T) string) []string {
	out := make([]string, len(list))
	for i, e := range list {
		out[i] = typ(e)
	}
	return out
}
