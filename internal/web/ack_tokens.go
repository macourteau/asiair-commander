package web

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strconv"
	"time"
)

// The acknowledge handshake (design library-reconciliation §10.7): a challenge
// mints a short-lived, per-target token the confirm must carry, so a request
// that never saw a challenge — a bookmarked URL, a replayed form post, a stray
// curl — cannot arm a device-original deletion. The store is in-memory and
// mutex-guarded, so a pending confirmation never survives a restart.
const (
	ackTokenTTL = 60 * time.Second
	ackTokenMax = 1024
)

// ackToken is one minted, unconsumed confirmation. ids is the frame-id set bound
// to a group token (nil for a single frame): the confirm applies exactly this set
// so the acknowledged rows can only shrink, never grow, between dialog and confirm
// (design library-reconciliation §10.7).
type ackToken struct {
	token   string
	expires time.Time
	ids     []int64
}

// ackFrameKey scopes a token to one (rig, frame). The frame id is numeric, so a
// raw '|' join is unambiguous.
func ackFrameKey(rigID string, frameID int64) string {
	return "f|" + rigID + "|" + strconv.FormatInt(frameID, 10)
}

// ackGroupKey scopes a token to one (rig, dir). Both segments are strconv.Quote'd
// because a rig id may contain '|' (rig ids are validated only for non-emptiness):
// a raw join would let (rig "a|x", dir "/p") and (rig "a", dir "x|/p") collide, so
// a confirm on one would consume the other's pending token (design
// library-reconciliation §10.7).
func ackGroupKey(rigID, dir string) string {
	return "d|" + strconv.Quote(rigID) + "|" + strconv.Quote(dir)
}

// mintAckToken issues a fresh token for key, binding ids (nil for a single frame),
// and returns it with its expiry. Each mint sweeps expired entries; if the map is
// still at ackTokenMax the earliest-expiring entry is evicted. Re-minting the same
// key overwrites, so re-challenging a target never accumulates.
func (s *Server) mintAckToken(key string, ids []int64) (token string, expires time.Time, err error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", time.Time{}, fmt.Errorf("mint ack token: %w", err)
	}
	tok := hex.EncodeToString(buf)
	now := s.now()
	exp := now.Add(ackTokenTTL)

	s.ackMu.Lock()
	defer s.ackMu.Unlock()
	for k, v := range s.ackTokens {
		if !v.expires.After(now) {
			delete(s.ackTokens, k)
		}
	}
	if _, exists := s.ackTokens[key]; !exists && len(s.ackTokens) >= ackTokenMax {
		// Still full after the sweep: evict the earliest-expiring entry so a fresh
		// mint always succeeds rather than failing under a flood of live tokens.
		var earliestKey string
		var earliest time.Time
		for k, v := range s.ackTokens {
			if earliestKey == "" || v.expires.Before(earliest) {
				earliestKey, earliest = k, v.expires
			}
		}
		delete(s.ackTokens, earliestKey)
	}
	s.ackTokens[key] = ackToken{token: tok, expires: exp, ids: ids}
	s.log.Info("web: minted acknowledge token", "key", key, "ids", len(ids))
	return tok, exp, nil
}

// lookupAckToken validates token against the entry stored under key — matching key,
// non-empty, unexpired, constant-time equal — WITHOUT consuming it. Consume happens
// only after the acknowledgement succeeds (design library-reconciliation §10.7:
// a timed-out-but-applied acknowledgement must leave the token spendable on
// retry).
func (s *Server) lookupAckToken(key, token string) (ackToken, bool) {
	if token == "" {
		return ackToken{}, false
	}
	s.ackMu.Lock()
	defer s.ackMu.Unlock()
	entry, ok := s.ackTokens[key]
	if !ok || !entry.expires.After(s.now()) || !constantEqual(entry.token, token) {
		s.log.Warn("web: acknowledge token validation failed", "key", key)
		return ackToken{}, false
	}
	return entry, true
}

// consumeAckToken removes the token stored under key, but only if it still matches
// the one being consumed — so tab A's confirm cannot delete the fresh token tab B
// just minted for the same target (compare-before-delete, design
// library-reconciliation §10.7).
func (s *Server) consumeAckToken(key, token string) {
	s.ackMu.Lock()
	defer s.ackMu.Unlock()
	if entry, ok := s.ackTokens[key]; ok && constantEqual(entry.token, token) {
		delete(s.ackTokens, key)
		s.log.Info("web: consumed acknowledge token", "key", key)
	}
}
