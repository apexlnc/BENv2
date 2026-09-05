package harness

import (
	"errors"
	"testing"

	"github.com/srhg-ai-7cef3f93/ben/internal/agent/localdomain"
	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

func TestLegacyEvidenceConfirmsOnlyAnotherBoot(t *testing.T) {
	here := bootID()
	if here == "" {
		t.Skip("this platform reports no boot identity")
	}
	for _, tc := range []struct {
		name     string
		evidence core.RunEvidence
		wantGone bool
		wantErr  bool
	}{
		{
			name: "another boot is quiet",
			evidence: core.RunEvidence{
				Scheme: core.RunEvidenceLocal, ID: "4242", Boot: here + "-old",
			},
			wantGone: true,
		},
		{
			name: "same boot remains possibly live",
			evidence: core.RunEvidence{
				Scheme: core.RunEvidenceLocal, ID: "4242", Boot: here,
			},
		},
		{
			name:     "missing boot",
			evidence: core.RunEvidence{Scheme: core.RunEvidenceLocal, ID: "4242"},
			wantErr:  true,
		},
		{
			name: "malformed id",
			evidence: core.RunEvidence{
				Scheme: core.RunEvidenceLocal, ID: "not-a-pgid", Boot: here,
			},
			wantErr: true,
		},
		{
			name: "zero id",
			evidence: core.RunEvidence{
				Scheme: core.RunEvidenceLocal, ID: "0", Boot: here,
			},
			wantErr: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gone, err := EvidenceGone(tc.evidence)
			if gone != tc.wantGone {
				t.Errorf("gone = %v, want %v", gone, tc.wantGone)
			}
			if (err != nil) != tc.wantErr {
				t.Errorf("err = %v, want error %v", err, tc.wantErr)
			}
		})
	}
}

func TestLinuxEvidenceUsesTheLocalDomainCodec(t *testing.T) {
	gone, err := EvidenceGone(core.RunEvidence{
		Scheme: localdomain.EvidenceScheme,
		Boot:   "not-a-boot-id",
		ID:     "not-a-domain-id",
	})
	if gone {
		t.Fatal("malformed Linux evidence reported quiet")
	}
	if !errors.Is(err, localdomain.ErrInvalidEvidence) && !errors.Is(err, localdomain.ErrUnavailable) {
		t.Fatalf("error = %v, want %v or %v", err, localdomain.ErrInvalidEvidence, localdomain.ErrUnavailable)
	}
}

func TestEvidenceGoneRefusesAnUnknownScheme(t *testing.T) {
	gone, err := EvidenceGone(core.RunEvidence{Scheme: "remote-shaped", ID: "run"})
	if gone || err == nil {
		t.Fatalf("EvidenceGone = (%v, %v), want unconfirmed error", gone, err)
	}
}
