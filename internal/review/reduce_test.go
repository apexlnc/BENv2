package review

import (
	"strings"
	"testing"
)

func TestReviewerProfileSelection(t *testing.T) {
	for _, tc := range []struct {
		name    string
		labels  []string
		want    StepKind
		profile string
		why     string
	}{
		{name: "default", want: StepReview, profile: "deep"},
		{name: "explicit", labels: []string{"review-profile:fast"}, want: StepReview, profile: "fast"},
		{name: "case-insensitive label", labels: []string{"Review-Profile:DEEP"}, want: StepReview, profile: "deep"},
		{name: "unknown", labels: []string{"review-profile:expensive"}, want: StepNothing, why: "no configured profile"},
		{name: "multiple", labels: []string{"review-profile:deep", "review-profile:fast"}, want: StepNothing, why: "2 reviewer profile labels"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := fxConfig()
			cfg.ReviewerProfiles = []string{"deep", "fast"}
			cfg.DefaultReviewerProfile = "deep"
			s := fxSnapshot()
			s.Issue.Labels = append(s.Issue.Labels, tc.labels...)
			got := Reduce(cfg, s)
			if got.Kind != tc.want || got.ReviewerProfile != tc.profile {
				t.Fatalf("Reduce = %+v, want kind %s profile %q", got, tc.want, tc.profile)
			}
			if tc.why != "" && !strings.Contains(got.Why, tc.why) {
				t.Errorf("reason = %q, want %q", got.Why, tc.why)
			}
		})
	}
}

func TestLegacyReviewerCommandRefusesProfileSelection(t *testing.T) {
	cfg := fxConfig()
	s := fxSnapshot()
	s.Issue.Labels = append(s.Issue.Labels, "review-profile:deep")
	got := Reduce(cfg, s)
	if got.Kind != StepNothing || !strings.Contains(got.Why, "not selectable") {
		t.Fatalf("Reduce = %+v, want a legacy-mode profile-selection refusal", got)
	}
}

func TestInvalidProfileSelectionParksBeforeRecoveryMutations(t *testing.T) {
	setups := []struct {
		name  string
		setup func(*Snapshot)
		want  StepKind
	}{
		{
			name: "terminal intent revocation",
			setup: func(s *Snapshot) {
				s.Comments = append(s.Comments, intentComment(3,
					routeIntentMarker(occ1, epoch1, head1, OutcomeNoProgress), at(20)))
			},
			want: StepRevoke,
		},
		{
			name: "completed route marker",
			setup: func(s *Snapshot) {
				s.reviewed(1, occ1, epoch1, head1, VerdictChangesRequested, at(20))
				s.unassign(6100, at(21))
			},
			want: StepRecordRoute,
		},
	}
	labels := []struct {
		name   string
		labels []string
		why    string
	}{
		{name: "unknown", labels: []string{"review-profile:unknown"}, why: "no configured profile"},
		{name: "ambiguous", labels: []string{"review-profile:deep", "review-profile:fast"}, why: "2 reviewer profile labels"},
	}

	for _, recovery := range setups {
		cfg := fxConfig()
		cfg.ReviewerProfiles = []string{"deep", "fast"}
		cfg.DefaultReviewerProfile = "deep"
		baseline := fxSnapshot()
		recovery.setup(&baseline)
		if got := Reduce(cfg, baseline); got.Kind != recovery.want {
			t.Fatalf("%s baseline Reduce = %+v, want %s", recovery.name, got, recovery.want)
		}

		for _, selection := range labels {
			t.Run(recovery.name+"/"+selection.name, func(t *testing.T) {
				s := fxSnapshot()
				recovery.setup(&s)
				s.Issue.Labels = append(s.Issue.Labels, selection.labels...)

				got := Reduce(cfg, s)
				if got.Kind != StepNothing || !strings.Contains(got.Why, selection.why) {
					t.Fatalf("Reduce = %+v, want profile refusal containing %q", got, selection.why)
				}
			})
		}
	}
}

func TestPublishedReviewCannotRouteUnderDifferentProfilePolicy(t *testing.T) {
	cfg := fxConfig()
	cfg.ReviewerProfiles = []string{"deep", "fast"}
	cfg.DefaultReviewerProfile = "deep"

	profiled := func(profile string) Snapshot {
		s := fxSnapshot()
		s.Reviews = append(s.Reviews, controllerReview(1, ReviewMarker{
			Occurrence: occ1, Claim: epoch1, Approval: approval1,
			Head: head1, Base: base1, Verdict: VerdictChangesRequested,
			ReviewerProfile: profile,
		}, at(20)))
		return s
	}

	t.Run("unknown selection parks before routing", func(t *testing.T) {
		s := profiled("fast")
		s.Issue.Labels = append(s.Issue.Labels, "review-profile:unknown")
		got := Reduce(cfg, s)
		if got.Kind != StepNothing || !strings.Contains(got.Why, "no configured profile") {
			t.Fatalf("Reduce = %+v, want unknown-profile refusal", got)
		}
	})

	t.Run("changed selection cannot reinterpret a durable verdict", func(t *testing.T) {
		s := profiled("fast")
		// No explicit label means the issue now selects the deep default.
		got := Reduce(cfg, s)
		if got.Kind != StepNothing || !strings.Contains(got.Why, `reviewed with profile "fast"`) {
			t.Fatalf("Reduce = %+v, want profile-drift refusal", got)
		}
	})

	t.Run("legacy marker remains recoverable", func(t *testing.T) {
		s := profiled("")
		got := Reduce(cfg, s)
		if got.Kind != StepUnassign {
			t.Fatalf("Reduce = %+v, want legacy review recovery", got)
		}
	})
}

func TestReviewerProfileConfigurationIsClosed(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*Config)
	}{
		{name: "default without profiles", edit: func(c *Config) { c.DefaultReviewerProfile = "deep" }},
		{name: "missing default", edit: func(c *Config) { c.ReviewerProfiles = []string{"deep"} }},
		{name: "unknown default", edit: func(c *Config) {
			c.ReviewerProfiles, c.DefaultReviewerProfile = []string{"fast"}, "deep"
		}},
		{name: "invalid name", edit: func(c *Config) {
			c.ReviewerProfiles, c.DefaultReviewerProfile = []string{"xhigh!"}, "xhigh!"
		}},
		{name: "duplicate", edit: func(c *Config) {
			c.ReviewerProfiles, c.DefaultReviewerProfile = []string{"deep", "deep"}, "deep"
		}},
		{name: "too many", edit: func(c *Config) {
			c.ReviewerProfiles = []string{"p1", "p2", "p3", "p4", "p5", "p6", "p7", "p8", "p9"}
			c.DefaultReviewerProfile = "p1"
		}},
		{name: "approval label overlap", edit: func(c *Config) {
			c.ReviewerProfiles, c.DefaultReviewerProfile = []string{"deep"}, "deep"
			c.RequiredLabels = append(c.RequiredLabels, "review-profile:deep")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := fxConfig()
			tc.edit(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Fatal("Validate accepted an open or ambiguous reviewer profile policy")
			}
		})
	}
}

// TestReduce is #11's acceptance table. Every row is one observation of the
// forge and the one move it authorizes; the name of each row is the row of the
// ticket it stands for.
func TestReduce(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*Config, *Snapshot)

		want       StepKind
		outcome    Outcome
		head       string
		occurrence int64
		claim      int64
		approval   int64
		why        string // substring of Step.Why
	}{
		{
			name:       "first delivery starts round one",
			want:       StepReview,
			head:       head1,
			occurrence: occ1,
			claim:      epoch1,
			approval:   approval1,
			why:        "round 1 of 3",
		},
		{
			name: "multiple required labels use the complete-set workspace anchor",
			setup: func(c *Config, s *Snapshot) {
				c.RequiredLabels = []string{fxQueue, "security-approved"}
				s.Issue.Labels = append(s.Issue.Labels, "security-approved")
				s.Events = append(s.Events, Event{
					ID: 6500, Type: EventLabeled, Actor: "a-human",
					Label: "security-approved", CreatedAt: at(0),
				})
			},
			want:       StepReview,
			head:       head1,
			occurrence: occ1,
			claim:      epoch1,
			approval:   6500,
		},
		{
			name: "reapproval after publication makes the old occurrence ineligible for review",
			setup: func(_ *Config, s *Snapshot) {
				s.unlabelQueue(6100, "a-human", at(20))
				s.labelQueue(6101, "a-human", at(21))
			},
			want: StepNothing,
			why:  "replaced by epoch 6101",
		},
		{
			name: "a redelivered occurrence reviews nothing twice",
			setup: func(_ *Config, s *Snapshot) {
				s.reviewed(1, occ1, epoch1, head1, VerdictChangesRequested, at(20))
			},
			want:       StepUnassign,
			head:       head1,
			occurrence: occ1,
			claim:      epoch1,
			why:        "changes are requested",
		},
		{
			name: "a legacy durable review resumes from its event-history approval",
			setup: func(_ *Config, s *Snapshot) {
				m := ReviewMarker{Occurrence: occ1, Claim: epoch1, Approval: approval1, Head: head1, Base: base1, Verdict: VerdictChangesRequested}
				r := controllerReview(1, m, at(20))
				r.Body = strings.Replace(r.Body, " approval=6001", "", 1)
				s.Reviews = append(s.Reviews, r)
			},
			want:       StepUnassign,
			head:       head1,
			occurrence: occ1,
			claim:      epoch1,
			why:        "changes are requested",
		},
		{
			name: "a legacy review cannot route against a replacement approval",
			setup: func(_ *Config, s *Snapshot) {
				m := ReviewMarker{Occurrence: occ1, Claim: epoch1, Approval: approval1, Head: head1, Base: base1, Verdict: VerdictChangesRequested}
				r := controllerReview(1, m, at(20))
				r.Body = strings.Replace(r.Body, " approval=6001", "", 1)
				s.Reviews = append(s.Reviews, r)
				s.unlabelQueue(6100, "a-human", at(21))
				s.labelQueue(6101, "a-human", at(22))
			},
			want:       StepRecordRoute,
			outcome:    OutcomeRevise,
			occurrence: occ1,
			claim:      epoch1,
			head:       head1,
			why:        "preserving newer human approval epoch 6101",
		},
		{
			name: "reapplying another required label cannot route the old workspace cycle",
			setup: func(c *Config, s *Snapshot) {
				c.RequiredLabels = []string{fxQueue, "security-approved"}
				s.Issue.Labels = append(s.Issue.Labels, "security-approved")
				s.Events = append(s.Events, Event{
					ID: 6500, Type: EventLabeled, Actor: "a-human",
					Label: "security-approved", CreatedAt: at(0),
				})
				s.Reviews = append(s.Reviews, controllerReview(1, ReviewMarker{
					Occurrence: occ1, Claim: epoch1, Approval: 6500,
					Head: head1, Base: base1, Verdict: VerdictChangesRequested,
				}, at(20)))
				s.Events = append(s.Events,
					Event{ID: 9100, Type: EventUnlabeled, Actor: "a-human", Label: "security-approved", CreatedAt: at(21)},
					Event{ID: 9101, Type: EventLabeled, Actor: "a-human", Label: "security-approved", CreatedAt: at(22)},
				)
			},
			want:       StepRecordRoute,
			outcome:    OutcomeRevise,
			occurrence: occ1,
			claim:      epoch1,
			head:       head1,
			why:        "preserving newer human approval epoch 9101",
		},
		{
			name: "changes requested hands the claim back and leaves the label alone",
			setup: func(_ *Config, s *Snapshot) {
				s.reviewed(1, occ1, epoch1, head1, VerdictChangesRequested, at(20))
			},
			want:       StepUnassign,
			occurrence: occ1,
			claim:      epoch1,
			head:       head1,
		},
		{
			name: "a published revision verdict never unassigns under a replacement approval",
			setup: func(_ *Config, s *Snapshot) {
				s.reviewed(1, occ1, epoch1, head1, VerdictChangesRequested, at(20))
				s.unlabelQueue(6100, "a-human", at(21))
				s.labelQueue(6101, "a-human", at(22))
			},
			want:       StepRecordRoute,
			outcome:    OutcomeRevise,
			occurrence: occ1,
			claim:      epoch1,
			head:       head1,
			why:        "preserving newer human approval epoch 6101",
		},
		{
			name: "a same-second crash after the unassignment repairs the marker only",
			setup: func(_ *Config, s *Snapshot) {
				s.reviewed(1, occ1, epoch1, head1, VerdictChangesRequested, at(20))
				s.unassign(6100, at(20))
			},
			want:       StepRecordRoute,
			outcome:    OutcomeRevise,
			occurrence: occ1,
			claim:      epoch1,
			head:       head1,
			why:        "handed back",
		},
		{
			name: "a newer claim epoch is never unassigned",
			setup: func(_ *Config, s *Snapshot) {
				s.reviewed(1, occ1, epoch1, head1, VerdictChangesRequested, at(20))
				s.unassign(6100, at(21))
				s.assign(epoch2, at(22))
			},
			want:       StepRecordRoute,
			outcome:    OutcomeRevise,
			occurrence: occ1,
			claim:      epoch1,
		},
		{
			name: "a newer claim that moved the head before marker recovery is never reviewed or unassigned",
			setup: func(_ *Config, s *Snapshot) {
				s.reviewed(1, occ1, epoch1, head1, VerdictChangesRequested, at(20))
				s.unassign(6100, at(21))
				s.assign(epoch2, at(22))
				s.PR = fxPR(head2)
			},
			want:       StepRecordRoute,
			outcome:    OutcomeRevise,
			occurrence: occ1,
			claim:      epoch1,
			head:       head1,
			why:        "source claim has already been handed back",
		},
		{
			name: "an overtaken completed occurrence is recorded before the newer publication advances",
			setup: func(_ *Config, s *Snapshot) {
				s.reviewed(1, occ1, epoch1, head1, VerdictChangesRequested, at(20))
				s.unassign(6100, at(21))
				s.assign(epoch2, at(22))
				s.delivered(4, occ2, head2, at(30))
			},
			want:       StepRecordRoute,
			outcome:    OutcomeRevise,
			occurrence: occ1,
			claim:      epoch1,
			head:       head1,
			why:        "handed back",
		},
		{
			name: "a newer stop mutation does not complete every older occurrence",
			setup: func(_ *Config, s *Snapshot) {
				s.reviewed(1, occ1, epoch1, head1, VerdictChangesRequested, at(20))
				s.delivered(4, occ2, head2, at(30))
				s.reviewed(2, occ2, epoch1, head2, VerdictClean, at(31))
				s.unlabelQueue(6100, fxController, at(32))
			},
			want:       StepRecordRoute,
			outcome:    OutcomeHumanReview,
			occurrence: occ2,
			claim:      epoch1,
			head:       head2,
		},
		{
			name: "a recorded route makes redelivery and a stale intent a no-op",
			setup: func(_ *Config, s *Snapshot) {
				s.reviewed(1, occ1, epoch1, head1, VerdictChangesRequested, at(20))
				s.unassign(6100, at(21))
				s.Comments = append(s.Comments, routeComment(3, RouteMarker{occ1, epoch1, head1, OutcomeRevise}, at(22)))
				s.Comments = append(s.Comments, intentComment(4, routeIntentMarker(occ1, epoch1, head1, OutcomeHumanReview), at(23)))
			},
			want: StepNothing,
			why:  "already routed as revise",
		},
		{
			name: "a clean review ends at human review without an approval",
			setup: func(_ *Config, s *Snapshot) {
				s.reviewed(1, occ1, epoch1, head1, VerdictClean, at(20))
			},
			want:       StepRevoke,
			outcome:    OutcomeHumanReview,
			occurrence: occ1,
			claim:      epoch1,
			head:       head1,
			why:        "is clean",
		},
		{
			name: "a clean route preserves reapproval after its occurrence",
			setup: func(_ *Config, s *Snapshot) {
				s.reviewed(1, occ1, epoch1, head1, VerdictClean, at(20))
				s.unlabelQueue(6100, "a-human", at(21))
				s.labelQueue(6101, "a-human", at(22))
			},
			want:       StepRecordRoute,
			outcome:    OutcomeHumanReview,
			occurrence: occ1,
			claim:      epoch1,
			head:       head1,
			why:        "preserving newer human approval epoch 6101",
		},
		{
			name: "a crash after the revocation repairs the marker only",
			setup: func(_ *Config, s *Snapshot) {
				s.reviewed(1, occ1, epoch1, head1, VerdictClean, at(20))
				s.unlabelQueue(6100, fxController, at(21))
			},
			want:       StepRecordRoute,
			outcome:    OutcomeHumanReview,
			occurrence: occ1,
			claim:      epoch1,
			head:       head1,
			why:        "no longer standing",
		},
		{
			name: "a same-second human reapplication does not undo a completed stop",
			setup: func(_ *Config, s *Snapshot) {
				s.reviewed(1, occ1, epoch1, head1, VerdictClean, at(20))
				s.unlabelQueue(6100, fxController, at(20))
				s.labelQueue(6101, "a-human", at(20))
			},
			want:    StepRecordRoute,
			outcome: OutcomeHumanReview,
		},
		{
			name: "a completed clean stop is not reversed after reapproval and subject movement",
			setup: func(_ *Config, s *Snapshot) {
				s.reviewed(1, occ1, epoch1, head1, VerdictClean, at(20))
				s.unlabelQueue(6100, fxController, at(21))
				s.labelQueue(6101, "a-human", at(22))
				s.PR = fxPR(head2)
				s.PR.BaseSHA = base2
			},
			want:       StepRecordRoute,
			outcome:    OutcomeHumanReview,
			occurrence: occ1,
			claim:      epoch1,
			head:       head1,
			why:        "original route",
		},
		{
			name: "a later review cannot rewrite a completed clean stop",
			setup: func(_ *Config, s *Snapshot) {
				s.reviewed(1, occ1, epoch1, head1, VerdictClean, at(20))
				s.unlabelQueue(6100, fxController, at(21))
				s.labelQueue(6101, "a-human", at(22))
				s.PR = fxPR(head2)
				s.PR.BaseSHA = base2
				s.reviewed(2, occ1, epoch1, head2, VerdictChangesRequested, at(30))
			},
			want:       StepRecordRoute,
			outcome:    OutcomeHumanReview,
			occurrence: occ1,
			claim:      epoch1,
			head:       head1,
			why:        "original route",
		},
		{
			name: "reapproval after a landed round-cap revocation repairs the old route",
			setup: func(_ *Config, s *Snapshot) {
				s.reviewed(1, occ1, epoch1, head1, VerdictChangesRequested, at(20))
				s.delivered(4, occ2, head2, at(30))
				s.reviewed(2, occ2, epoch1, head2, VerdictChangesRequested, at(31))
				s.delivered(5, occ3, head3, at(40))
				s.reviewed(3, occ3, epoch1, head3, VerdictChangesRequested, at(41))
				s.unlabelQueue(6100, fxController, at(45))
				s.labelQueue(6101, "a-human", at(60))
			},
			want:       StepRecordRoute,
			outcome:    OutcomeRoundCap,
			occurrence: occ3,
			claim:      epoch1,
			head:       head3,
		},
		{
			name: "reapproval after a landed no-progress revocation repairs the old route",
			setup: func(_ *Config, s *Snapshot) {
				s.reviewed(1, occ1, epoch1, head1, VerdictChangesRequested, at(20))
				s.unassign(6100, at(21))
				s.Comments = append(s.Comments, routeComment(3, RouteMarker{occ1, epoch1, head1, OutcomeRevise}, at(22)))
				s.assign(epoch2, at(30))
				s.delivered(4, occ2, head1, at(40))
				s.Comments = append(s.Comments, intentComment(5, routeIntentMarker(occ2, epoch2, head1, OutcomeNoProgress), at(44)))
				s.unlabelQueue(6101, fxController, at(45))
				s.labelQueue(6102, "a-human", at(60))
			},
			want:       StepRecordRoute,
			outcome:    OutcomeNoProgress,
			occurrence: occ2,
			claim:      epoch2,
			head:       head1,
		},
		{
			name: "a real revision is a new head and a new round",
			setup: func(_ *Config, s *Snapshot) {
				s.reviewed(1, occ1, epoch1, head1, VerdictChangesRequested, at(20))
				s.unassign(6100, at(21))
				s.Comments = append(s.Comments, routeComment(3, RouteMarker{occ1, epoch1, head1, OutcomeRevise}, at(22)))
				s.assign(epoch2, at(30))
				s.delivered(4, occ2, head2, at(40))
			},
			want:       StepReview,
			head:       head2,
			occurrence: occ2,
			claim:      epoch2,
			why:        "round 2 of 3",
		},
		{
			name: "a new occurrence on a reviewed head stops as no progress",
			setup: func(_ *Config, s *Snapshot) {
				s.reviewed(1, occ1, epoch1, head1, VerdictChangesRequested, at(20))
				s.unassign(6100, at(21))
				s.Comments = append(s.Comments, routeComment(3, RouteMarker{occ1, epoch1, head1, OutcomeRevise}, at(22)))
				s.assign(epoch2, at(30))
				s.delivered(4, occ2, head1, at(40))
			},
			want:       StepRecordIntent,
			outcome:    OutcomeNoProgress,
			occurrence: occ2,
			claim:      epoch2,
			approval:   approval1,
			head:       head1,
			why:        "has not moved",
		},
		{
			name: "a no-progress intent keeps the occurrence approval after pre-intent reapproval",
			setup: func(_ *Config, s *Snapshot) {
				s.reviewed(1, occ1, epoch1, head1, VerdictChangesRequested, at(20))
				s.unassign(6100, at(21))
				s.Comments = append(s.Comments, routeComment(3, RouteMarker{occ1, epoch1, head1, OutcomeRevise}, at(22)))
				s.assign(epoch2, at(30))
				s.delivered(4, occ2, head1, at(40))
				s.unlabelQueue(6101, "a-human", at(42))
				s.labelQueue(6102, "a-human", at(43))
			},
			want:       StepRecordIntent,
			outcome:    OutcomeNoProgress,
			occurrence: occ2,
			claim:      epoch2,
			approval:   approval1,
			head:       head1,
		},
		{
			name: "a human cannot forge a terminal route intent",
			setup: func(_ *Config, s *Snapshot) {
				s.reviewed(1, occ1, epoch1, head1, VerdictChangesRequested, at(20))
				s.unassign(6100, at(21))
				s.Comments = append(s.Comments, routeComment(3, RouteMarker{occ1, epoch1, head1, OutcomeRevise}, at(22)))
				s.assign(epoch2, at(30))
				s.delivered(4, occ2, head1, at(40))
				forged := intentComment(5, routeIntentMarker(occ2, epoch2, head2, OutcomeRoundCap), at(41))
				forged.Author = "mallory"
				s.Comments = append(s.Comments, forged)
			},
			want:       StepRecordIntent,
			outcome:    OutcomeNoProgress,
			occurrence: occ2,
			claim:      epoch2,
			head:       head1,
		},
		{
			name: "a terminal intent must name a tracker occurrence",
			setup: func(_ *Config, s *Snapshot) {
				s.Comments = append(s.Comments, intentComment(3, routeIntentMarker(9999, epoch1, head1, OutcomeHumanReview), at(20)))
			},
			want: StepNothing,
			why:  "not a tracker-authored published transition",
		},
		{
			name: "a terminal intent cannot cross a claim epoch",
			setup: func(_ *Config, s *Snapshot) {
				s.Comments = append(s.Comments, intentComment(3, routeIntentMarker(occ1, epoch2, head1, OutcomeHumanReview), at(20)))
			},
			want: StepNothing,
			why:  "but its source claim",
		},
		{
			name: "a terminal intent cannot cross an approval epoch",
			setup: func(_ *Config, s *Snapshot) {
				s.unlabelQueue(6101, "a-human", at(11))
				s.labelQueue(6102, "a-human", at(12))
				m := routeIntentMarker(occ1, epoch1, head1, OutcomeHumanReview)
				m.Approval = 6102
				s.Comments = append(s.Comments, intentComment(3, m, at(20)))
			},
			want: StepNothing,
			why:  "but its source approval",
		},
		{
			name: "a legacy queue-label intent resumes under a multi-label workspace cycle",
			setup: func(c *Config, s *Snapshot) {
				c.RequiredLabels = []string{fxQueue, "security-approved"}
				s.Issue.Labels = append(s.Issue.Labels, "security-approved")
				s.Events = append(s.Events, Event{
					ID: 6500, Type: EventLabeled, Actor: "a-human",
					Label: "security-approved", CreatedAt: at(0),
				})
				// The pre-upgrade shape recorded QueueLabel's 6001 event even
				// though the complete-set workspace anchor is 6500.
				s.Comments = append(s.Comments, intentComment(3,
					routeIntentMarker(occ1, epoch1, head1, OutcomeHumanReview), at(20)))
			},
			want:       StepRevoke,
			outcome:    OutcomeHumanReview,
			occurrence: occ1,
			claim:      epoch1,
			head:       head1,
			why:        "resuming durable",
		},
		{
			name: "a legacy multi-label intent preserves a replacement cycle",
			setup: func(c *Config, s *Snapshot) {
				c.RequiredLabels = []string{fxQueue, "security-approved"}
				s.Issue.Labels = append(s.Issue.Labels, "security-approved")
				s.Events = append(s.Events,
					Event{ID: 6500, Type: EventLabeled, Actor: "a-human", Label: "security-approved", CreatedAt: at(0)},
					Event{ID: 9100, Type: EventUnlabeled, Actor: "a-human", Label: "security-approved", CreatedAt: at(21)},
					Event{ID: 9101, Type: EventLabeled, Actor: "a-human", Label: "security-approved", CreatedAt: at(22)},
				)
				s.Comments = append(s.Comments, intentComment(3,
					routeIntentMarker(occ1, epoch1, head1, OutcomeHumanReview), at(20)))
			},
			want:       StepRecordRoute,
			outcome:    OutcomeHumanReview,
			occurrence: occ1,
			claim:      epoch1,
			head:       head1,
			why:        "preserving newer human approval epoch 9101",
		},
		{
			name: "duplicate terminal intents resume once",
			setup: func(cfg *Config, s *Snapshot) {
				cfg.AddHumanReviewLabel = false
				m := routeIntentMarker(occ1, epoch1, head1, OutcomeHumanReview)
				s.Comments = append(s.Comments, intentComment(3, m, at(20)), intentComment(4, m, at(21)))
			},
			want:       StepRevoke,
			outcome:    OutcomeHumanReview,
			occurrence: occ1,
			claim:      epoch1,
			head:       head1,
		},
		{
			name: "conflicting terminal intents fail closed",
			setup: func(_ *Config, s *Snapshot) {
				s.Comments = append(s.Comments,
					intentComment(3, routeIntentMarker(occ1, epoch1, head1, OutcomeHumanReview), at(20)),
					intentComment(4, routeIntentMarker(occ1, epoch1, head1, OutcomeRoundCap), at(21)),
				)
			},
			want: StepNothing,
			why:  "conflicting terminal route intents",
		},
		{
			name: "an absent required label completes a terminal intent",
			setup: func(_ *Config, s *Snapshot) {
				s.Comments = append(s.Comments, intentComment(3, routeIntentMarker(occ1, epoch1, head1, OutcomeHumanReview), at(20)))
				s.Issue.Labels = nil
			},
			want:       StepRecordRoute,
			outcome:    OutcomeHumanReview,
			occurrence: occ1,
			claim:      epoch1,
			head:       head1,
		},
		{
			name: "three reviewed heads with changes still requested stop at the cap",
			setup: func(_ *Config, s *Snapshot) {
				s.reviewed(1, occ1, epoch1, head1, VerdictChangesRequested, at(20))
				s.delivered(4, occ2, head2, at(30))
				s.reviewed(2, occ2, epoch1, head2, VerdictChangesRequested, at(31))
				s.delivered(5, occ3, head3, at(40))
				s.reviewed(3, occ3, epoch1, head3, VerdictChangesRequested, at(41))
			},
			want:       StepRevoke,
			outcome:    OutcomeRoundCap,
			occurrence: occ3,
			claim:      epoch1,
			head:       head3,
			why:        "which is the cap",
		},
		{
			name: "a fourth distinct head is not reviewed at all",
			setup: func(_ *Config, s *Snapshot) {
				s.reviewed(1, occ1, epoch1, head1, VerdictChangesRequested, at(20))
				s.delivered(4, occ2, head2, at(30))
				s.reviewed(2, occ2, epoch1, head2, VerdictChangesRequested, at(31))
				s.delivered(5, occ3, head3, at(40))
				s.reviewed(3, occ3, epoch1, head3, VerdictChangesRequested, at(41))
				s.delivered(6, occ4, head4, at(50))
			},
			want:       StepRecordIntent,
			outcome:    OutcomeRoundCap,
			occurrence: occ4,
			claim:      epoch1,
			head:       head4,
		},
		{
			name: "a human reapplying the label begins a newly approved cycle",
			setup: func(_ *Config, s *Snapshot) {
				s.reviewed(1, occ1, epoch1, head1, VerdictChangesRequested, at(20))
				s.delivered(3, occ2, head2, at(25))
				s.reviewed(2, occ2, epoch1, head2, VerdictChangesRequested, at(30))
				s.delivered(4, occ3, head3, at(35))
				s.reviewed(3, occ3, epoch1, head3, VerdictChangesRequested, at(40))
				s.unlabelQueue(6100, fxController, at(45))
				s.unassign(6101, at(46))
				s.Comments = append(s.Comments, routeComment(6, RouteMarker{occ3, epoch1, head3, OutcomeRoundCap}, at(47)))
				s.labelQueue(6102, "a-human", at(60))
				s.assign(epoch2, at(61))
				s.delivered(7, occ4, head4, at(62))
			},
			want:       StepReview,
			head:       head4,
			occurrence: occ4,
			claim:      epoch2,
			why:        "round 1 of 3",
		},
		{
			name: "a head moved after review publication is reviewed again",
			setup: func(_ *Config, s *Snapshot) {
				s.reviewed(1, occ1, epoch1, head1, VerdictChangesRequested, at(20))
				s.PR = fxPR(head2)
			},
			want:       StepReview,
			head:       head2,
			occurrence: occ1,
			claim:      epoch1,
			why:        "round 2 of 3",
		},
		{
			name: "a base moved after review publication is reviewed again",
			setup: func(_ *Config, s *Snapshot) {
				s.reviewed(1, occ1, epoch1, head1, VerdictChangesRequested, at(20))
				s.PR.Base = "release"
				s.PR.BaseSHA = base2
			},
			want:       StepReview,
			head:       head1,
			occurrence: occ1,
			claim:      epoch1,
			why:        "base moved",
		},
		{
			name: "a human co-assignee is never removed",
			setup: func(_ *Config, s *Snapshot) {
				s.reviewed(1, occ1, epoch1, head1, VerdictChangesRequested, at(20))
				s.Issue.Assignees = []string{fxPrincipal, "a-human"}
			},
			want:       StepRevoke,
			outcome:    OutcomeHumanReview,
			occurrence: occ1,
			why:        "no assignee is removed",
		},
		{
			name: "a contested claim epoch escalates rather than guessing",
			setup: func(_ *Config, s *Snapshot) {
				s.reviewed(1, occ1, 6999, head1, VerdictChangesRequested, at(20))
			},
			want:    StepRecordIntent,
			outcome: OutcomeHumanReview,
			why:     "contested",
		},
		{
			name: "a contested review approval escalates rather than guessing",
			setup: func(_ *Config, s *Snapshot) {
				s.Reviews = append(s.Reviews, controllerReview(1, ReviewMarker{
					Occurrence: occ1, Claim: epoch1, Approval: 6999,
					Head: head1, Base: base1, Verdict: VerdictChangesRequested,
				}, at(20)))
			},
			want:    StepRecordIntent,
			outcome: OutcomeHumanReview,
			why:     "contested",
		},
		{
			name: "a change log and assignee list that disagree are retried, not escalated",
			setup: func(_ *Config, s *Snapshot) {
				s.reviewed(1, occ1, epoch1, head1, VerdictChangesRequested, at(20))
				s.Issue.Assignees = nil
			},
			want: StepNothing,
			why:  "retrying",
		},
		{
			name: "a revocation between the review and its route is obeyed",
			setup: func(_ *Config, s *Snapshot) {
				s.reviewed(1, occ1, epoch1, head1, VerdictChangesRequested, at(20))
				s.unlabelQueue(6100, "a-human", at(21))
			},
			want:       StepRecordRoute,
			outcome:    OutcomeHumanReview,
			occurrence: occ1,
			claim:      epoch1,
			why:        "withdrawn before the revision could be routed",
		},
		{
			name: "a revocation after the unassignment still records the revise",
			setup: func(_ *Config, s *Snapshot) {
				s.reviewed(1, occ1, epoch1, head1, VerdictChangesRequested, at(20))
				s.unassign(6100, at(21))
				s.unlabelQueue(6101, "a-human", at(22))
			},
			want:       StepRecordRoute,
			outcome:    OutcomeRevise,
			occurrence: occ1,
			claim:      epoch1,
			why:        "handed back",
		},
		{
			name: "a revoked label with nothing owed stops automation",
			setup: func(_ *Config, s *Snapshot) {
				s.unlabelQueue(6100, "a-human", at(15))
			},
			want: StepNothing,
			why:  "automation is revoked",
		},
		{
			name: "a missing non-queue required label parks a review while events lag",
			setup: func(c *Config, s *Snapshot) {
				c.RequiredLabels = []string{fxQueue, "security-approved"}
				// Event history still reports the complete set at publication,
				// while the current issue snapshot already observes revocation.
				s.Events = append(s.Events, Event{
					ID: 6500, Type: EventLabeled, Actor: "a-human",
					Label: "security-approved", CreatedAt: at(0),
				})
			},
			want: StepNothing,
			why:  `"security-approved" is not standing`,
		},
		{
			name: "a missing non-queue required label never permits revision handoff",
			setup: func(c *Config, s *Snapshot) {
				c.RequiredLabels = []string{fxQueue, "security-approved"}
				s.Events = append(s.Events, Event{
					ID: 6500, Type: EventLabeled, Actor: "a-human",
					Label: "security-approved", CreatedAt: at(0),
				})
				s.Reviews = append(s.Reviews, controllerReview(1, ReviewMarker{
					Occurrence: occ1, Claim: epoch1, Approval: 6500,
					Head: head1, Base: base1, Verdict: VerdictChangesRequested,
				}, at(20)))
			},
			want:       StepRevoke,
			outcome:    OutcomeHumanReview,
			occurrence: occ1,
			claim:      epoch1,
			head:       head1,
			why:        `required label "security-approved" was withdrawn`,
		},
		{
			name: "a review by anybody else authorizes no route",
			setup: func(_ *Config, s *Snapshot) {
				r := controllerReview(1, ReviewMarker{Occurrence: occ1, Claim: epoch1, Approval: approval1, Head: head1, Base: base1, Verdict: VerdictClean}, at(20))
				r.Author = "an-impostor"
				s.Reviews = append(s.Reviews, r)
			},
			want: StepReview,
			head: head1,
		},
		{
			name: "a review whose marker does not match GitHub's commit_id is not evidence",
			setup: func(_ *Config, s *Snapshot) {
				r := controllerReview(1, ReviewMarker{Occurrence: occ1, Claim: epoch1, Approval: approval1, Head: head1, Base: base1, Verdict: VerdictClean}, at(20))
				r.CommitID = head2
				s.Reviews = append(s.Reviews, r)
			},
			want: StepReview,
			head: head1,
		},
		{
			name: "a review carrying two markers is not evidence",
			setup: func(_ *Config, s *Snapshot) {
				r := controllerReview(1, ReviewMarker{Occurrence: occ1, Claim: epoch1, Approval: approval1, Head: head1, Base: base1, Verdict: VerdictClean}, at(20))
				r.Body = ReviewMarker{Occurrence: occ1, Claim: epoch1, Approval: approval1, Head: head1, Base: base1, Verdict: VerdictChangesRequested}.String() + "\n" + r.Body
				s.Reviews = append(s.Reviews, r)
			},
			want: StepReview,
			head: head1,
		},
		{
			name: "an APPROVE by the controller identity is not one of its reviews",
			setup: func(_ *Config, s *Snapshot) {
				r := controllerReview(1, ReviewMarker{Occurrence: occ1, Claim: epoch1, Approval: approval1, Head: head1, Base: base1, Verdict: VerdictClean}, at(20))
				r.State = "APPROVED"
				s.Reviews = append(s.Reviews, r)
			},
			want: StepReview,
			head: head1,
		},
		{
			name: "a milestone by anybody else is not a delivery",
			setup: func(_ *Config, s *Snapshot) {
				s.Comments[1].Author = "an-impostor"
			},
			want: StepNothing,
			why:  "no published milestone",
		},
		{
			name: "an invented milestone occurrence is not a delivery",
			setup: func(_ *Config, s *Snapshot) {
				var kept []Event
				for _, ev := range s.Events {
					if ev.ID != occ1 {
						kept = append(kept, ev)
					}
				}
				s.Events = kept
			},
			want: StepNothing,
			why:  "not a tracker-authored published transition",
		},
		{
			name: "a pull request closing another issue is not this issue's work",
			setup: func(_ *Config, s *Snapshot) {
				s.PR.Body = "Fixes #12\n"
			},
			want: StepNothing,
			why:  "closes issue #12",
		},
		{
			name: "a pull request closing this issue and a foreign one is refused",
			setup: func(_ *Config, s *Snapshot) {
				s.PR.Body = "Fixes #11\nFixes other/repo#12\n"
			},
			want: StepNothing,
			why:  "also closes an issue in another repository",
		},
		{
			name: "a pull request closing no issue cannot be confirmed",
			setup: func(_ *Config, s *Snapshot) {
				s.PR.Body = "No keyword here.\n"
			},
			want: StepNothing,
			why:  "names no issue to close",
		},
		{
			name: "a pull request on a non-canonical branch is refused",
			setup: func(_ *Config, s *Snapshot) {
				s.PR.Branch = "feature/other"
			},
			want: StepNothing,
			why:  "not the canonical",
		},
		{
			name: "a closed pull request is refused",
			setup: func(_ *Config, s *Snapshot) {
				s.PR.Closed = true
			},
			want: StepNothing,
			why:  "is not open",
		},
		{
			name: "a merged pull request is refused",
			setup: func(_ *Config, s *Snapshot) {
				s.PR.Merged = true
			},
			want: StepNothing,
			why:  "is not open",
		},
		{
			name: "a milestone linking another repository is refused",
			setup: func(_ *Config, s *Snapshot) {
				s.Comments[1] = milestone(2, occ1, "https://github.com/evil/other/pull/42", at(10))
			},
			want: StepNothing,
			why:  "which is not acme/ben",
		},
		{
			name: "a milestone linking a different pull request number is refused",
			setup: func(_ *Config, s *Snapshot) {
				s.Comments[1] = milestone(2, occ1, "https://github.com/acme/ben/pull/43", at(10))
			},
			want: StepNothing,
			why:  "but #42 was read",
		},
		{
			name: "a milestone with no link is not a delivery",
			setup: func(_ *Config, s *Snapshot) {
				s.Comments[1].Body = "**BEN published a pull request.**\n\n<!-- ben:milestone kind=published occurrence=9001 -->\n"
			},
			want: StepNothing,
			why:  "no published milestone",
		},
		{
			name: "a pull request that could not be read routes nothing",
			setup: func(_ *Config, s *Snapshot) {
				s.PR = nil
			},
			want: StepNothing,
			why:  "could not be read",
		},
		{
			name: "a closed issue routes nothing",
			setup: func(_ *Config, s *Snapshot) {
				s.Issue.Closed = true
			},
			want: StepNothing,
			why:  "is closed",
		},
		{
			name: "a terminal intent binds the route before its mutation",
			setup: func(_ *Config, s *Snapshot) {
				s.reviewed(1, occ1, epoch1, head1, VerdictChangesRequested, at(20))
				s.unassign(6100, at(21))
				s.Comments = append(s.Comments, routeComment(3, RouteMarker{occ1, epoch1, head1, OutcomeRevise}, at(22)))
				s.assign(epoch2, at(30))
				s.delivered(4, occ2, head1, at(40))
				s.Comments = append(s.Comments, intentComment(5, routeIntentMarker(occ2, epoch2, head1, OutcomeNoProgress), at(41)))
				s.PR = fxPR(head2)
				s.PR.BaseSHA = base2
			},
			want:       StepRevoke,
			outcome:    OutcomeNoProgress,
			occurrence: occ2,
			claim:      epoch2,
			head:       head1,
			why:        "durable no-progress intent",
		},
		{
			name: "a pending terminal intent preserves a newer human approval",
			setup: func(_ *Config, s *Snapshot) {
				s.reviewed(1, occ1, epoch1, head1, VerdictChangesRequested, at(20))
				s.unassign(6100, at(21))
				s.Comments = append(s.Comments, routeComment(3, RouteMarker{occ1, epoch1, head1, OutcomeRevise}, at(22)))
				s.assign(epoch2, at(30))
				s.delivered(4, occ2, head1, at(40))
				s.Comments = append(s.Comments, intentComment(5, routeIntentMarker(occ2, epoch2, head1, OutcomeNoProgress), at(41)))
				s.unlabelQueue(6101, "a-human", at(42))
				s.labelQueue(6102, "a-human", at(43))
				s.PR = fxPR(head2)
				s.PR.BaseSHA = base2
			},
			want:       StepRecordRoute,
			outcome:    OutcomeNoProgress,
			occurrence: occ2,
			claim:      epoch2,
			head:       head1,
			why:        "preserving newer human approval epoch 6102",
		},
		{
			name: "a no-review terminal intent survives reapproval and subject movement",
			setup: func(_ *Config, s *Snapshot) {
				s.reviewed(1, occ1, epoch1, head1, VerdictChangesRequested, at(20))
				s.unassign(6100, at(21))
				s.Comments = append(s.Comments, routeComment(3, RouteMarker{occ1, epoch1, head1, OutcomeRevise}, at(22)))
				s.assign(epoch2, at(30))
				s.delivered(4, occ2, head1, at(40))
				s.Comments = append(s.Comments, intentComment(5, routeIntentMarker(occ2, epoch2, head1, OutcomeNoProgress), at(41)))
				s.unlabelQueue(6101, fxController, at(42))
				s.labelQueue(6102, "a-human", at(43))
				s.PR = fxPR(head2)
				s.PR.BaseSHA = base2
			},
			want:       StepRecordRoute,
			outcome:    OutcomeNoProgress,
			occurrence: occ2,
			claim:      epoch2,
			head:       head1,
			why:        "durable no-progress intent",
		},
		{
			name: "a merged pull request cannot prevent reviewed route repair",
			setup: func(_ *Config, s *Snapshot) {
				s.reviewed(1, occ1, epoch1, head1, VerdictClean, at(20))
				s.unlabelQueue(6100, fxController, at(21))
				s.Issue.Closed = true
				s.PR.Closed = true
				s.PR.Merged = true
			},
			want:       StepRecordRoute,
			outcome:    OutcomeHumanReview,
			occurrence: occ1,
			claim:      epoch1,
			head:       head1,
		},
		{
			name: "a merged pull request cannot prevent intent route repair",
			setup: func(_ *Config, s *Snapshot) {
				s.reviewed(1, occ1, epoch1, head1, VerdictChangesRequested, at(20))
				s.unassign(6100, at(21))
				s.Comments = append(s.Comments, routeComment(3, RouteMarker{occ1, epoch1, head1, OutcomeRevise}, at(22)))
				s.assign(epoch2, at(30))
				s.delivered(4, occ2, head1, at(40))
				s.Comments = append(s.Comments, intentComment(5, routeIntentMarker(occ2, epoch2, head1, OutcomeNoProgress), at(41)))
				s.unlabelQueue(6101, fxController, at(42))
				s.Issue.Closed = true
				s.PR.Closed = true
				s.PR.Merged = true
			},
			want:       StepRecordRoute,
			outcome:    OutcomeNoProgress,
			occurrence: occ2,
			claim:      epoch2,
			head:       head1,
		},
		{
			name: "with no standing claim there is nothing to review under",
			setup: func(_ *Config, s *Snapshot) {
				s.unassign(6100, at(15))
			},
			want: StepNothing,
			why:  "no standing claim",
		},
		{
			name: "an invalid configuration decides nothing",
			setup: func(c *Config, _ *Snapshot) {
				c.Controller = fxPrincipal
			},
			want: StepNothing,
			why:  "review and unassign itself",
		},
		{
			name: "a claimed milestone is not a published one",
			setup: func(_ *Config, s *Snapshot) {
				s.Comments[1].Body = strings.Replace(s.Comments[1].Body, "kind=published", "kind=claimed", 1)
			},
			want: StepNothing,
			why:  "no published milestone",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, snap := fxConfig(), fxSnapshot()
			if tc.setup != nil {
				tc.setup(&cfg, &snap)
			}

			got := Reduce(cfg, snap)

			if got.Kind != tc.want {
				t.Fatalf("Reduce kind = %q (%s), want %q", got.Kind, got.Why, tc.want)
			}
			if tc.outcome != "" && got.Outcome != tc.outcome {
				t.Errorf("outcome = %q, want %q (%s)", got.Outcome, tc.outcome, got.Why)
			}
			if tc.head != "" && got.Head != tc.head {
				t.Errorf("head = %s, want %s", short(got.Head), short(tc.head))
			}
			if tc.occurrence != 0 && got.Occurrence != tc.occurrence {
				t.Errorf("occurrence = %d, want %d", got.Occurrence, tc.occurrence)
			}
			if tc.claim != 0 && got.Claim != tc.claim {
				t.Errorf("claim = %d, want %d", got.Claim, tc.claim)
			}
			if tc.approval != 0 && got.Approval != tc.approval {
				t.Errorf("approval = %d, want %d", got.Approval, tc.approval)
			}
			if tc.why != "" && !strings.Contains(got.Why, tc.why) {
				t.Errorf("why = %q, want it to contain %q", got.Why, tc.why)
			}
			assertPermissionModel(t, cfg, got)
		})
	}
}

// assertPermissionModel is the invariant every row shares, anchored where no
// individual row can forget it: whatever the reducer decides, it removes only
// the required label, adds only the informational one, and names only BEN's
// principal for removal. This is the half of #11's security posture that lives
// in this package — the other half is the workflow's `permissions:` block, and
// cmd/benreview's own tests hold the client to it.
func assertPermissionModel(t *testing.T, cfg Config, s Step) {
	t.Helper()
	switch s.Kind {
	case StepNothing, StepReview, StepRecordIntent, StepRecordRoute:
		if s.Principal != "" || s.RemoveLabel != "" || s.AddLabel != "" {
			t.Errorf("%s mutates labels or assignees: %+v", s.Kind, s)
		}
	case StepUnassign:
		if s.Principal != cfg.Principal {
			t.Errorf("unassign names %q, want the claim principal %q", s.Principal, cfg.Principal)
		}
		if s.RemoveLabel != "" || s.AddLabel != "" {
			t.Errorf("unassign touches labels: %+v", s)
		}
	case StepRevoke:
		if s.RemoveLabel != cfg.QueueLabel {
			t.Errorf("revoke removes %q, want the required label %q", s.RemoveLabel, cfg.QueueLabel)
		}
		wantAdd := ""
		if cfg.AddHumanReviewLabel {
			wantAdd = HumanReviewLabel
		}
		if s.AddLabel != wantAdd {
			t.Errorf("revoke adds %q, want the fixed informational label %q", s.AddLabel, wantAdd)
		}
		if s.Principal != "" {
			t.Errorf("a stop route unassigned %q; BEN releases its own claim (SPEC §9.8)", s.Principal)
		}
	default:
		t.Fatalf("unknown step kind %q — extend this assertion with the new kind", s.Kind)
	}
	if (s.Kind == StepRecordIntent || s.Kind == StepRecordRoute) && s.Claim == 0 {
		t.Errorf("route artifact would carry claim=0, which cannot be parsed back")
	}
	if s.Kind == StepRecordIntent && s.Approval == 0 {
		t.Errorf("terminal route intent would carry approval=0, which cannot be parsed back")
	}
}

// TestReduceIsAFunctionOfTheForge pins the property the whole design rests on:
// the controller keeps no state, so two reductions of one observation agree,
// and the second run after a delivery it already handled is a no-op rather
// than a repeat. Event and scheduled paths differ only in what wakes the
// process, so this is also what makes them produce the same result.
func TestReduceIsAFunctionOfTheForge(t *testing.T) {
	cfg, snap := fxConfig(), fxSnapshot()
	snap.reviewed(1, occ1, epoch1, head1, VerdictChangesRequested, at(20))
	snap.unassign(6100, at(21))
	snap.Comments = append(snap.Comments, routeComment(3, RouteMarker{occ1, epoch1, head1, OutcomeRevise}, at(22)))

	first := Reduce(cfg, snap)
	second := Reduce(cfg, snap)
	if first != second {
		t.Fatalf("two reductions of one snapshot disagree:\n %+v\n %+v", first, second)
	}
	if first.Kind != StepNothing {
		t.Fatalf("a fully routed occurrence yields %q (%s), want nothing", first.Kind, first.Why)
	}
}

// TestReduceUsesTheLatestOccurrence: an older delivery cannot reopen a round
// once a newer one exists, whichever order the comment pages arrive in.
func TestReduceUsesTheLatestOccurrence(t *testing.T) {
	cfg, snap := fxConfig(), fxSnapshot()
	snap.delivered(4, occ2, head2, at(40))
	// Deliberately out of order: the reducer sorts on the occurrence, not on
	// the position in the page.
	snap.Comments[1], snap.Comments[2] = snap.Comments[2], snap.Comments[1]

	got := Reduce(cfg, snap)
	if got.Kind != StepReview || got.Occurrence != occ2 || got.Head != head2 {
		t.Fatalf("Reduce = %+v, want a review of occurrence %d at head %s", got, occ2, short(head2))
	}
}
