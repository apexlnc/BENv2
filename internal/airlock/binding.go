package airlock

import (
	"fmt"

	"github.com/srhg-ai-7cef3f93/ben/internal/remote"
)

// clientSubstrateBinding reduces the constructed client to the non-secret
// scope Airlock applies to durable addresses. The credential source contract
// makes BindingKey complete for the source definition. A static $VAR can still
// change runtime principal beneath that definition; keyed mutations add the
// credential snapshot's PrincipalBinding before their reservation is written.
func clientSubstrateBinding(c *client) (SubstrateBinding, error) {
	descriptor := c.auth.Descriptor()
	binding := SubstrateBinding{
		BaseURL:              c.base.String(),
		CredentialKind:       descriptor.Kind,
		CredentialBindingKey: descriptor.BindingKey,
	}
	if !binding.complete() {
		return SubstrateBinding{}, fmt.Errorf("%w: the auth source has no complete non-secret binding", ErrConfig)
	}
	return binding, nil
}

// requireSubstrateBinding is the restart fence. An older record with no
// binding is unknowable, not adoptable: replaying it could cross an endpoint or
// tenant boundary and turn the same textual key into a second side effect.
func requireSubstrateBinding(recorded, current SubstrateBinding, record string) error {
	switch {
	case !recorded.complete():
		return fmt.Errorf("%w: %s has no endpoint and credential binding", ErrSubstrateBinding, record)
	case recorded != current:
		return fmt.Errorf("%w: %s was written for another endpoint or credential identity",
			ErrSubstrateBinding, record)
	default:
		return nil
	}
}

// requirePrincipalBinding is the dynamic half of the restart fence. A source
// definition can stay byte-for-byte equal while an opaque $VAR changes to a
// token for another Airlock tenant or subject, so the static substrate binding
// alone cannot authorize replay of an unanswered keyed mutation.
func requirePrincipalBinding(recorded, current, record string) error {
	switch {
	case recorded == "":
		return fmt.Errorf("%w: %s has no runtime principal binding", ErrSubstrateBinding, record)
	case current == "":
		return fmt.Errorf("%w: %s has no current runtime principal binding", ErrSubstrateBinding, record)
	case recorded != current:
		return fmt.Errorf("%w: %s was written for another runtime principal", ErrSubstrateBinding, record)
	default:
		return nil
	}
}

func loadBoundSandbox(store Store, current SubstrateBinding, claim remote.Claim) (SandboxRecord, error) {
	rec, err := store.LoadSandbox(claim)
	if err != nil {
		return SandboxRecord{}, err
	}
	if err := requireSubstrateBinding(rec.Substrate, current, claim.String()); err != nil {
		return SandboxRecord{}, err
	}
	return rec, nil
}
