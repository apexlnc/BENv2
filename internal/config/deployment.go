package config

import (
	"fmt"
	"strings"
)

// The `deployment` block of SPEC §5.2.9 — the declaration §10.1 requires before
// a daemon dispatches.
//
// The whole of this file exists because §10.1 says a deployment "MUST NOT arrive
// in [risk-accepted] mode by default or by omission", and a program can enforce
// exactly one half of that: it can refuse the omission. It cannot verify what
// was declared. §10.1 is explicit that its requirement 2 is "a property, not a
// mechanism", that it endorses none, and that "a deployment MUST verify that its
// chosen mechanism achieves it on its own platform rather than assume it" — so an
// attestation BEN checked would be a lie in whichever direction it guessed.
//
// That is why there is no default and no detection. A default is arrival by
// omission with extra steps; detection would have to answer "is a human present",
// which is not observable from inside the process — a TTY check is wrong in both
// directions, since `ben run` under tmux has a TTY and no human, and under
// systemd has neither.

// validateDeployment enforces §5.2.9. It runs on the resolved config, so an
// absent block and an empty mode are the same thing by here — which is the
// intended reading: `deployment:` written with no `mode` is as unstated as no
// block at all, and both are the refusal this exists for.
func validateDeployment(d DeploymentConfig) error {
	if d.Mode == "" {
		return &ValidationError{Field: "deployment.mode", Msg: fmt.Sprintf(
			"required: state how this deployment runs (one of %s). SPEC §10.1 forbids arriving "+
				"in an unattended mode by omission, and BEN cannot verify the property for you — "+
				"declaring it is the whole of what it asks", deploymentModeList())}
	}
	if !knownDeploymentMode(d.Mode) {
		return &ValidationError{Field: "deployment.mode", Msg: fmt.Sprintf(
			"unknown mode %q; the set is closed (one of %s)", d.Mode, deploymentModeList())}
	}
	// Required for risk-accepted and only there: protected needs no justification,
	// and demanding one everywhere trains operators to write filler. Trimmed,
	// because whitespace is not a record.
	if d.Mode == DeploymentRiskAccepted && strings.TrimSpace(d.AcceptedBecause) == "" {
		return &ValidationError{Field: "deployment.accepted_because", Msg: "required and non-blank for " +
			"mode risk-accepted: §10.1 calls it an explicit, recorded choice, and the record is this field"}
	}
	return nil
}

func knownDeploymentMode(m DeploymentMode) bool {
	for _, known := range deploymentModes {
		if m == known {
			return true
		}
	}
	return false
}

func deploymentModeList() string {
	out := make([]string, len(deploymentModes))
	for i, m := range deploymentModes {
		out[i] = string(m)
	}
	return strings.Join(out, ", ")
}

// Unattended reports whether this declaration asserts an unattended mode, which
// is what §10.1's requirements govern. `attended` is the exemption.
func (d DeploymentConfig) Unattended() bool {
	return d.Mode == DeploymentProtected || d.Mode == DeploymentRiskAccepted
}
