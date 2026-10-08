package push

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/MoonWX/isshoni/internal/logx"
)

// Pruning (04 §14.6). Subscriptions go away in four ways: at once when the push service says they are gone (404,
// 410) or not ours (401, 403), in the Service's result handling; with the web session that created them (03's
// ON DELETE CASCADE); all of them at the first start after a VAPID rotation; and, here, daily when they kept
// failing for a month.

// metaVAPIDFingerprint is 03's meta key for the fingerprint of the VAPID key that the stored subscriptions were
// created for (03 §4.6).
const metaVAPIDFingerprint = "vapid_key_fp"

// The daily job deletes the subscriptions with at least pruneMinFailures failures in a row and no success for
// pruneNoSuccessFor.
const (
	pruneInterval     = 24 * time.Hour
	pruneMinFailures  = 20
	pruneNoSuccessFor = 30 * 24 * time.Hour
)

// vapidFingerprint identifies a VAPID key pair without revealing it: the hex of the first 8 bytes of SHA-256 of
// the public key's 65 bytes, the form of 03's session_key_fp and invite_key_fp (03 §4.6).
func vapidFingerprint(publicKey []byte) string {
	sum := sha256.Sum256(publicKey)
	return hex.EncodeToString(sum[:8])
}

// checkVAPIDFingerprint is the startup half of `isshoni admin rotate-secrets` (04 §5.2, §5.3): a browser's
// subscription is bound to the VAPID public key it subscribed with, so after a rotation no stored subscription can
// ever be sent to again. When the stored fingerprint differs from the current key's, every subscription is
// deleted (each browser subscribes again the next time the app opens) and the new fingerprint is stored. An empty
// stored fingerprint is a first start, not a rotation.
//
// New runs it before returning, so the purge is over before any listener serves. The delete comes first: a crash
// between the two writes repeats the purge at the next start, never skips it.
func checkVAPIDFingerprint(ctx context.Context, st Store, fp string) (rotated bool, deleted int, err error) {
	stored, err := st.Meta(ctx, metaVAPIDFingerprint)
	if err != nil {
		return false, 0, fmt.Errorf("push: reading the VAPID key fingerprint: %w", err)
	}
	if stored == fp {
		return false, 0, nil
	}
	if stored != "" {
		rotated = true
		if deleted, err = st.DeleteAll(ctx); err != nil {
			return true, 0, fmt.Errorf("push: deleting the subscriptions of the previous VAPID key: %w", err)
		}
	}
	if err := st.SetMeta(ctx, metaVAPIDFingerprint, fp); err != nil {
		return rotated, deleted, fmt.Errorf("push: storing the VAPID key fingerprint: %w", err)
	}
	return rotated, deleted, nil
}

// pruneLoop runs the daily job: once when Run starts (a server that restarts every night would otherwise never
// prune) and then every 24 hours, until ctx ends.
func (s *Service) pruneLoop(ctx context.Context) {
	t := time.NewTicker(pruneInterval)
	defer t.Stop()
	for {
		s.pruneOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *Service) pruneOnce(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	n, err := s.store.Prune(ctx, pruneMinFailures, s.now().Add(-pruneNoSuccessFor))
	switch {
	case err != nil && ctx.Err() == nil:
		s.log.Warn("pruning push subscriptions failed", logx.Err(err))
	case n > 0:
		s.log.Info("deleted push subscriptions that kept failing", "count", n,
			"min_failures", pruneMinFailures, "no_success_days", int(pruneNoSuccessFor/(24*time.Hour)))
	}
}
