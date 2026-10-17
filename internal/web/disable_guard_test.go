package web

import (
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/macourteau/asiair-commander/internal/config"
	"github.com/macourteau/asiair-commander/internal/hub"
	"github.com/macourteau/asiair-commander/internal/model"
)

// guardServer builds a Server whose only wired dependency is a hub carrying the
// given published rig statuses and a fixed clock — enough to exercise the
// disable guard's real decision function (design device-identity-and-media §8.2).
// Statuses are published through the real hub path (PublishRig), so the guard
// reads them from a genuine Snapshot().
func guardServer(t *testing.T, now time.Time, statuses ...model.RigStatus) *Server {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := hub.New(logger)
	for _, st := range statuses {
		h.PublishRig(st)
	}
	return &Server{deps: Deps{Hub: h}, now: func() time.Time { return now }, log: logger}
}

func rigCfg(id string, sources ...model.Source) *model.Config {
	c := config.Defaults()
	c.Rigs = []model.RigProfile{{ID: id, Name: id, Sources: sources}}
	return c
}

func ptrStatus(s model.RigStatus) *model.RigStatus { return &s }

// TestDisableGuardStructurallyEmptyOnly (D4) pins that newly disabling a share is
// refused unless the rig is online, not paused, its last listing is within one
// poll, and the share is in the structurally-confirmed-empty set — and allowed when
// all hold. An already-disabled source passes untouched even while offline.
func TestDisableGuardStructurallyEmptyOnly(t *testing.T) {
	now := time.Date(2026, 7, 23, 0, 0, 0, 0, time.UTC)
	const rig, share = "rig-1", "Udisk Images"
	enabled := rigCfg(rig, model.Source{Share: share, Roots: []string{"Autorun"}})
	disabledCand := rigCfg(rig, model.Source{Share: share, Roots: []string{"Autorun"}, Disabled: true})

	tests := []struct {
		name       string
		stored     *model.Config
		status     *model.RigStatus // nil → nothing published for the rig
		wantRefuse bool
		wantSub    string
	}{
		{
			name:   "confirmed empty allows",
			stored: enabled,
			status: ptrStatus(model.RigStatus{ID: rig, Online: true, ListingTS: now.Unix(),
				PollIntervalSec: 30, EmptySlots: []string{share}}),
			wantRefuse: false,
		},
		{
			name:   "medium present refuses",
			stored: enabled,
			status: ptrStatus(model.RigStatus{ID: rig, Online: true, ListingTS: now.Unix(),
				PollIntervalSec: 30, EmptySlots: nil}),
			wantRefuse: true, wantSub: "while a medium is inserted",
		},
		{
			name:   "offline refuses",
			stored: enabled,
			status: ptrStatus(model.RigStatus{ID: rig, Online: false, ListingTS: now.Unix(),
				PollIntervalSec: 30, EmptySlots: []string{share}}),
			wantRefuse: true, wantSub: "Bring the rig online",
		},
		{
			name:   "paused refuses",
			stored: enabled,
			status: ptrStatus(model.RigStatus{ID: rig, Online: true, Paused: true, ListingTS: now.Unix(),
				PollIntervalSec: 30, EmptySlots: []string{share}}),
			wantRefuse: true, wantSub: "Resume the rig",
		},
		{
			name:   "stale listing refuses",
			stored: enabled,
			status: ptrStatus(model.RigStatus{ID: rig, Online: true, ListingTS: now.Add(-100 * time.Second).Unix(),
				PollIntervalSec: 30, EmptySlots: []string{share}}),
			wantRefuse: true, wantSub: "Wait for a fresh check",
		},
		{
			name:       "no published status refuses",
			stored:     enabled,
			status:     nil,
			wantRefuse: true, wantSub: "Bring the rig online",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var statuses []model.RigStatus
			if tc.status != nil {
				statuses = append(statuses, *tc.status)
			}
			srv := guardServer(t, now, statuses...)
			probs := srv.disableGuardProblems(disabledCand, tc.stored)
			if tc.wantRefuse {
				if len(probs) != 1 {
					t.Fatalf("problems=%d, want 1 refusal: %+v", len(probs), probs)
				}
				if probs[0].Field != "rigs[rig-1].sources" {
					t.Fatalf("field=%q, want rigs[rig-1].sources", probs[0].Field)
				}
				if probs[0].Severity != config.SeverityError {
					t.Fatalf("severity=%q, want error", probs[0].Severity)
				}
				if !strings.Contains(probs[0].Message, tc.wantSub) {
					t.Fatalf("message=%q, want it to contain %q", probs[0].Message, tc.wantSub)
				}
			} else if len(probs) != 0 {
				t.Fatalf("allowed case returned problems: %+v", probs)
			}
		})
	}

	// An already-disabled source is never re-confirmed, even offline: only a fresh
	// introduction of a disabled state is guarded.
	t.Run("already disabled passes untouched offline", func(t *testing.T) {
		storedDisabled := rigCfg(rig, model.Source{Share: share, Roots: []string{"Autorun"}, Disabled: true})
		srv := guardServer(t, now) // no status published → rig effectively offline
		if probs := srv.disableGuardProblems(disabledCand, storedDisabled); len(probs) != 0 {
			t.Fatalf("re-saving an already-disabled source must not be guarded: %+v", probs)
		}
	})
}

// TestDisableGuardCreateAndReAdd (D4) pins the transition rule beyond a plain edit:
// a create with a pre-disabled source, a config-PUT insert, and a remove-and-re-add
// disabled are all fresh introductions with no confirming listing, so all are
// refused. An enabled source never trips the guard.
func TestDisableGuardCreateAndReAdd(t *testing.T) {
	now := time.Date(2026, 7, 23, 0, 0, 0, 0, time.UTC)
	const rig, share = "rig-1", "TF Images"
	disabledCand := rigCfg(rig, model.Source{Share: share, Roots: []string{"Autorun"}, Disabled: true})

	t.Run("create pre-disabled refused", func(t *testing.T) {
		// A brand-new rig: the stored config has no such rig to diff, and no worker has
		// published a listing yet.
		stored := config.Defaults() // no rigs
		srv := guardServer(t, now)
		probs := srv.disableGuardProblems(disabledCand, stored)
		if len(probs) != 1 || probs[0].Field != "rigs[rig-1].sources" {
			t.Fatalf("create pre-disabled must be refused on rigs[rig-1].sources; got %+v", probs)
		}
	})

	t.Run("config-PUT insert disabled refused", func(t *testing.T) {
		// A rig introduced via PUT /api/config: stored has a different rig, so the new
		// rig's disabled source is a fresh introduction.
		stored := rigCfg("other", model.Source{Share: "EMMC Images", Roots: []string{"Autorun"}})
		srv := guardServer(t, now)
		if probs := srv.disableGuardProblems(disabledCand, stored); len(probs) != 1 {
			t.Fatalf("a PUT-inserted disabled source must be refused; got %+v", probs)
		}
	})

	t.Run("remove-and-re-add disabled refused", func(t *testing.T) {
		// The stored rig watches the share ENABLED; the candidate re-adds it disabled.
		// Even with a fresh online status, a non-empty slot refuses.
		stored := rigCfg(rig, model.Source{Share: share, Roots: []string{"Autorun"}})
		srv := guardServer(t, now, model.RigStatus{ID: rig, Online: true, ListingTS: now.Unix(),
			PollIntervalSec: 30, EmptySlots: nil})
		if probs := srv.disableGuardProblems(disabledCand, stored); len(probs) != 1 {
			t.Fatalf("re-adding a share disabled must be refused while its slot is not empty; got %+v", probs)
		}
	})

	t.Run("re-add confirmed empty allowed", func(t *testing.T) {
		// The same re-add, but the live listing confirms the slot empty → allowed.
		stored := rigCfg(rig, model.Source{Share: share, Roots: []string{"Autorun"}})
		srv := guardServer(t, now, model.RigStatus{ID: rig, Online: true, ListingTS: now.Unix(),
			PollIntervalSec: 30, EmptySlots: []string{share}})
		if probs := srv.disableGuardProblems(disabledCand, stored); len(probs) != 0 {
			t.Fatalf("re-adding a confirmed-empty share must be allowed; got %+v", probs)
		}
	})

	t.Run("enabled source never guarded", func(t *testing.T) {
		enabledCand := rigCfg(rig, model.Source{Share: share, Roots: []string{"Autorun"}})
		srv := guardServer(t, now) // offline: irrelevant, the source is not disabled
		if probs := srv.disableGuardProblems(enabledCand, config.Defaults()); len(probs) != 0 {
			t.Fatalf("an enabled source must never be guarded; got %+v", probs)
		}
	})
}
