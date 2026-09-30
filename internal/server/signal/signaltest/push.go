package signaltest

import (
	"context"
	"slices"
	"sync"

	"github.com/MoonWX/isshoni/internal/server/signal"
)

// Push is a fake signal.PushNotifier that records every event.
type Push struct {
	mu     sync.Mutex
	events []signal.PushShareStarted
}

// ShareStarted implements signal.PushNotifier.
func (p *Push) ShareStarted(_ context.Context, ev signal.PushShareStarted) {
	p.mu.Lock()
	defer p.mu.Unlock()
	ev.PresentUserIDs = slices.Clone(ev.PresentUserIDs)
	p.events = append(p.events, ev)
}

// Events returns the recorded events, in order.
func (p *Push) Events() []signal.PushShareStarted {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.events)
}

// Policy holds the admin policy that the hub reads on every use: pass Get as Deps.Policy.
type Policy struct {
	mu sync.Mutex
	p  signal.Policy
}

// Set replaces the policy.
func (p *Policy) Set(v signal.Policy) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.p = v
}

// Get returns the policy.
func (p *Policy) Get() signal.Policy {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.p
}
