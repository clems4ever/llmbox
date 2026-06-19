package cluster

import (
	"fmt"
	"regexp"

	"github.com/clems4ever/llmbox/internal/docker"
)

// boxIDRe is the box-id format a spoke accepts: a single DNS hostname label
// (1-63 chars of lowercase letters, digits, or hyphens, not starting or ending
// with a hyphen). It mirrors what create_llmbox documents and what Docker
// accepts as a hostname, so the spoke can reject a malformed id before touching
// Docker.
var boxIDRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// ValidationPolicy is the spoke-side admission policy applied to box-creation
// requests arriving over the wire — defense-in-depth on top of the verb
// allowlist, so a spoke validates inputs itself rather than trusting the hub.
// The box-id format is always enforced; AllowedImages, when non-empty,
// restricts which explicit images the spoke will launch (an empty image means
// the spoke's own configured default and is always allowed).
type ValidationPolicy struct {
	// AllowedImages is the set of images the spoke will launch when the request
	// names one explicitly. Empty means no image restriction.
	AllowedImages []string
}

// validateCreate rejects a creation request whose box id is malformed or whose
// explicit image is not allowed by the policy.
//
// @arg opts The creation options received from the hub.
// @error error if the box id is not a valid hostname label, or the requested image is not allowed.
//
// @testcase TestValidateCreateRejectsBadBoxID rejects a malformed box id.
// @testcase TestValidateCreateImageAllowlist allows listed images and rejects others.
// @testcase TestValidateCreateAllowsEmptyImage treats an empty image as the spoke default.
func (p ValidationPolicy) validateCreate(opts docker.CreateOptions) error {
	if !boxIDRe.MatchString(opts.BoxID) {
		return fmt.Errorf("invalid box id %q: must be 1-63 chars of lowercase letters, digits, or hyphens (not starting or ending with a hyphen)", opts.BoxID)
	}
	if opts.Image != "" && len(p.AllowedImages) > 0 {
		for _, allowed := range p.AllowedImages {
			if opts.Image == allowed {
				return nil
			}
		}
		return fmt.Errorf("image %q is not allowed on this spoke", opts.Image)
	}
	return nil
}
