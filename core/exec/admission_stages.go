package exec

import (
	"fmt"

	"github.com/yongjohnlee80/autodb/core/admission"
)

// The legacy guards, adapted to the admission seam. The ADAPTERS are
// constructed here, in exec, closing over the engine's own unexported
// functions: the identity mapping (errors.Is against the exported
// sentinels) happens in the closure, and the leaf package names nothing
// from this package. The logic is not rewritten — each adapter holds the
// guard itself, maps its error identity onto a Reason per the minimal deny
// table, and stops. The mapping table is the one place identity could
// silently change, so every row carries the sentinel it maps and the cell
// that pins it.
//
// A stage is absent by construction where it is inapplicable (the seam's
// Needs and TargetCaps declarations), and it declares every Code it can
// deny with — mandatory disclosure, rejected at evaluation time if a
// denial arrives undeclared.

// sizeCapStage is the intake bound: reject oversized text BEFORE
// classification, so the audit record always equals what ran — never
// execute an unaudited tail. It is the first stage in every chain and the
// same rule at every one of the engine's former check sites.
type sizeCapStage struct{}

func (sizeCapStage) Name() string { return "sizecap" }

func (sizeCapStage) ContextNeeds() admission.Needs { return admission.Needs{} }

func (sizeCapStage) DenyCodes() []admission.Code {
	return []admission.Code{admission.CodeScriptTooLarge}
}

// Apply enforces the bound the drive supplies through Context. The Reason
// preserves the sentinel's meaning word-for-word: an oversized script is
// refused before understanding, and the identity is the compatibility
// surface.
func (sizeCapStage) Apply(facts admission.Facts, ctx admission.Context) (admission.Contribution, error) {
	if ctx.MaxStatementBytes > 0 && facts.TextLen() > ctx.MaxStatementBytes {
		return admission.Deny(admission.Reason{
			Code:     admission.CodeScriptTooLarge,
			Subject:  fmt.Sprintf("%d bytes", facts.TextLen()),
			Detail:   ErrScriptTooLarge.Error(),
			Continue: true,
		}), nil
	}
	return admission.NoContribution(), nil
}
