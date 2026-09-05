package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/airlock"
	"github.com/srhg-ai-7cef3f93/ben/internal/config"
	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/remote"
)

// The v2 substrate leg of assembly (#194, #46).
//
// `ben config effective` renders the whole declaration with no credentials and
// no network, exactly as it does for every other section. What happens here is
// the other half of assembly decision 13's wiring — construct the backend from
// the declaration, and prove it can serve this workflow *here and now* before a
// claim depends on it.

// ErrSubstrateCredential refuses a remote substrate whose backend credential
// could not be constructed. Distinct from ErrNotReady: nothing was asked of
// the network, and the fault is in the workflow.
var ErrSubstrateCredential = errors.New("the substrate credential could not be constructed")

// readySubstrate constructs the declared backend and proves it can serve this
// workflow here and now — or returns nil for a workflow that runs on this host.
//
// nil is the local substrate and the whole of what an omitted `substrate:`
// section means. Everything below is reached only by a workflow that opted in,
// which is what keeps every v1 configuration on exactly the path it was on
// before this file existed.
//
// The readiness probe stays ahead of the local adapters for the reason it always
// did: an operator staging a v2 configuration should meet the *actionable*
// refusal first — a wrong endpoint, a rejected token, a profile their tenant is
// not approved for, a profile an operator has withdrawn — rather than a
// downstream failure about something they did not write.
func (b *builder) readySubstrate(
	ctx context.Context, def *config.WorkflowDefinition, cred core.CredentialSource,
) (*airlock.Substrate, error) {
	if !def.Config.Substrate.Remote() {
		return nil, nil
	}
	substrate, err := newAirlockSubstrate(def, b.substrateDir, cred, b.substrateTransport)
	if err != nil {
		return nil, err
	}
	if err := substrate.Ready(ctx); err != nil {
		return nil, fmt.Errorf("%w: substrate.kind %q: %w", ErrNotReady, def.Config.Substrate.Kind, err)
	}
	b.log.Info("v2 execution substrate is reachable and its profile is provisionable",
		"kind", def.Config.Substrate.Kind,
		"base_url", def.Config.Substrate.Airlock.BaseURL,
		"profile", def.Config.Substrate.Airlock.Profile)
	return substrate, nil
}

// reconcile completes the startup survey of every retained remote claim before
// ordinary dispatch can resume (docs/AIRLOCK.md).
//
// It is a **read**, and what it produces is a log line per claim plus a refusal
// for the ones nothing can act on. Deciding what to *do* with a claim is the
// orchestrator's — the decision needs the tracker's view and this survey has
// none — and the mechanism that keeps a replacement from being dispatched into a
// live execution domain is not this pass at all: it is the §9.10 run marker,
// which the remote strategy reads off the run journal and answers through
// RunGone, and which reports possibly-live until the backend positively observes
// domain quiet.
//
// What the pass adds is the part §9.10 cannot reconstruct from the tracker: a
// claim whose recorded sandbox is gone, owned by somebody else, or pinned to a
// profile revision that has moved. Those are refusals an operator has to see at
// startup rather than one dispatched claim at a time — and, because they are
// reported rather than repaired, the daemon refuses to start rather than
// dispatching around them.
//
// Once per process. A reload cannot move the substrate (config refuses it), so
// re-surveying on every reload would spend a request per retained claim to be
// told what the last survey established.
func (b *builder) reconcile(ctx context.Context, substrate *airlock.Substrate) error {
	b.mu.Lock()
	done := b.reconciled
	b.reconciled = true
	b.mu.Unlock()
	if done {
		return nil
	}

	states, err := substrate.Reconcile(ctx, remote.NewDirStore(b.journalDir))
	if err != nil {
		return fmt.Errorf("%w: surveying retained remote claims: %w", ErrNotReady, err)
	}
	var unresolved []string
	for _, st := range states {
		b.log.Info("retained remote claim surveyed",
			"claim", st.Claim.String(), "record", st.Record, "sandbox", st.Identity.SandboxID,
			"sandbox_state", string(st.Sandbox), "active_run", st.ActiveRunID,
			"dispatched", st.Dispatched, "cursor", st.Cursor, "terminal", st.Terminal,
			"start_unresolved", st.StartUnresolved,
			"quiet", remote.MayReuse(st.Status), "lease", st.Lease.String(), "error", st.Err)
		if st.Err != nil {
			unresolved = append(unresolved, fmt.Sprintf("%s: %v", st.Record, st.Err))
		}
	}
	if len(unresolved) > 0 {
		return fmt.Errorf("%w: %d retained remote claim(s) need reconciling before this daemon may dispatch: %s",
			ErrNotReady, len(unresolved), strings.Join(unresolved, "; "))
	}
	b.log.Info("startup reconciliation complete",
		"claims", len(states), "active", airlock.Active(states))
	return nil
}

// newAirlockSubstrate builds the backend from one workflow declaration.
//
// Every value comes from the loader's already-validated configuration; nothing
// here reads the process environment, which is what keeps a workflow the single
// statement of what a daemon does (SPEC §3.7). The credential is narrowed to the
// cached surface for the tracker's reason: the client makes several calls per
// tick per claim, and an exchange per request would multiply the issuer's
// traffic by the daemon's.
func newAirlockSubstrate(
	def *config.WorkflowDefinition, stateDir string, cred core.CredentialSource, transport http.RoundTripper,
) (*airlock.Substrate, error) {
	if cred == nil {
		return nil, fmt.Errorf("%w: substrate.airlock.auth_source resolved to no credential source",
			ErrSubstrateCredential)
	}
	a := def.Config.Substrate.Airlock
	tlsCfg, err := substrateTLS(a.TLSCAFile)
	if err != nil {
		return nil, fmt.Errorf("%w: substrate.airlock.tls_ca_file: %w", ErrConstruct, err)
	}
	substrate, err := airlock.New(airlock.Options{
		BaseURL:   a.BaseURL,
		Auth:      cred,
		Profile:   a.Profile,
		TLS:       tlsCfg,
		Store:     airlock.NewDirStore(stateDir),
		Transport: transport,
		Timeouts: airlock.Timeouts{
			Request:  ms(a.RequestTimeoutMS),
			Poll:     ms(a.PollTimeoutMS),
			PollWait: ms(a.PollWaitMS),
			Settle:   ms(a.SettleTimeoutMS),
			Retries:  a.MaxRetries,
		},
		Retention: airlock.Retention{
			IdleSuspend:     ms(a.IdleSuspendMS),
			DeleteAfterIdle: ms(a.DeleteAfterIdleMS),
			OnSuccess:       disposalOf(a.OnSuccess),
			OnFailure:       disposalOf(a.OnFailure),
			OnRevoked:       disposalOf(a.OnRevoked),
			OnShutdown:      disposalOf(a.OnShutdown),
		},
	})
	if err != nil {
		return nil, fmt.Errorf("%w: substrate.kind %q: %w", ErrConstruct, def.Config.Substrate.Kind, err)
	}
	return substrate, nil
}

// substrateTLS builds the client's verification material. Nil — the host's own
// roots — is the ordinary case; a private CA is what the key exists for.
//
// The bundle is read here rather than by the client so a missing or malformed
// file is a construction refusal an operator sees at startup, not a handshake
// failure discovered at the first claim.
func substrateTLS(caFile string) (*tls.Config, error) {
	if caFile == "" {
		return nil, nil
	}
	pem, err := os.ReadFile(caFile)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("%s holds no PEM certificates", caFile)
	}
	// MinVersion stated rather than inherited: this connection carries a bearer
	// token and a run's whole output, and the default floor is a thing that has
	// moved before.
	return &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}, nil
}

// disposalOf maps the workflow's keyword onto the backend's closed set.
//
// An unrecognized keyword retains. It is unreachable — validateSubstrate refuses
// one — and stated rather than assumed because retain is the only safe default:
// a disposal nobody could name must not destroy a tree somebody may still need.
func disposalOf(keyword string) airlock.Disposal {
	switch keyword {
	case config.DisposalSuspend:
		return airlock.DisposalSuspend
	case config.DisposalDelete:
		return airlock.DisposalDelete
	}
	return airlock.DisposalRetain
}

func ms(v int) time.Duration { return time.Duration(v) * time.Millisecond }
