package buildutil

import (
	"fmt"
	"strings"

	"oras.land/oras-go/v2/registry/remote"
)

// CheckRefs validates the --from and --to references of a build: both parse as OCI
// references: the input may be digest-pinned, the output is tagged, they differ, and both start with one of the allowed
// prefixes. The build runs as root and writes to a registry, so an empty allowlist is refused
// rather than treated as "allow all".
func CheckRefs(from, to string, prefixes []string, allowed func([]string, string) bool) error {
	if len(prefixes) == 0 {
		return fmt.Errorf("VZ_ALLOWED_IMAGE_PREFIXES must be set for build-image")
	}
	for _, r := range []struct{ flag, ref string }{{"--from", from}, {"--to", to}} {
		repo, err := remote.NewRepository(r.ref)
		if err != nil {
			return fmt.Errorf("%s %q: %w", r.flag, r.ref, err)
		}
		if repo.Reference.Reference == "" || (r.flag == "--to" && strings.Contains(r.ref, "@")) {
			return fmt.Errorf("%s %q requires an explicit input tag/digest or output tag", r.flag, r.ref)
		}
		if strings.Contains(r.ref, "@") && repo.Reference.ValidateReferenceAsDigest() != nil {
			return fmt.Errorf("%s %q has an invalid digest", r.flag, r.ref)
		}
		if !allowed(prefixes, r.ref) {
			return fmt.Errorf("%s %q is not allowed: it must start with one of %v (VZ_ALLOWED_IMAGE_PREFIXES)", r.flag, r.ref, prefixes)
		}
	}
	if from == to {
		return fmt.Errorf("--from and --to must differ")
	}
	return nil
}
