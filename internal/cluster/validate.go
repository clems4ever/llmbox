package cluster

import (
	"fmt"

	"github.com/clems4ever/llmbox/internal/docker"
)

// ValidationPolicy is the spoke-side admission policy applied to box-creation
// requests arriving over the wire — defense-in-depth on top of the verb
// allowlist, so a spoke validates inputs itself rather than trusting the hub.
// The box-id format is always enforced, and so is the presence of an image: a
// spoke holds no default image of its own, so the hub must name one on every
// create. AllowedImages, when non-empty, further restricts which images the
// spoke will launch.
type ValidationPolicy struct {
	// AllowedImages is the set of images the spoke will launch. Empty means no
	// image restriction (any image the hub sends is accepted).
	AllowedImages []string
}

// validateCreate rejects a creation request whose box id is malformed, whose
// image is absent, or whose image is not allowed by the policy. The image is
// required because spokes have no default of their own: the hub resolves the box
// image (its built-in default included) and sends it explicitly.
//
// @arg opts The creation options received from the hub.
// @error error if the box id is not a valid hostname label, no image is named, or the requested image is not allowed.
//
// @testcase TestValidateCreateRejectsBadBoxID rejects a malformed box id.
// @testcase TestValidateCreateImageAllowlist allows listed images and rejects others.
// @testcase TestValidateCreateRejectsEmptyImage rejects a create that names no image.
func (p ValidationPolicy) validateCreate(opts docker.CreateOptions) error {
	if !docker.ValidBoxID(opts.BoxID) {
		return fmt.Errorf("invalid box id %q: must be 1-63 chars of lowercase letters, digits, or hyphens (not starting or ending with a hyphen)", opts.BoxID)
	}
	if opts.Image == "" {
		return fmt.Errorf("create request names no image: the hub must send an explicit image (spokes have no default)")
	}
	if len(p.AllowedImages) > 0 {
		for _, allowed := range p.AllowedImages {
			if opts.Image == allowed {
				return nil
			}
		}
		return fmt.Errorf("image %q is not allowed on this spoke", opts.Image)
	}
	return nil
}
