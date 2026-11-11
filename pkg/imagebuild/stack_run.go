package imagebuild

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/agoda-com/macOS-vz-kubelet/internal/buildutil"
	"github.com/agoda-com/macOS-vz-kubelet/pkg/downloader"
	"github.com/agoda-com/macOS-vz-kubelet/pkg/resource"
	"github.com/virtual-kubelet/virtual-kubelet/log"
	"oras.land/oras-go/v2/registry/remote"
)

var errBuildRequired = errors.New("native image identity requires construction")

// Run joins its result with custody cleanup. A failed cleanup is a failure,
// never an otherwise successful pending outcome.
func onlyBuildRequired(err error) bool {
	if err == errBuildRequired {
		return true
	}
	joined, ok := err.(interface{ Unwrap() []error })
	return ok && len(joined.Unwrap()) == 1 && onlyBuildRequired(joined.Unwrap()[0])
}

// StackResult separates a settled prefix from completion of the requested stack.
// Ref and InputIdentity belong to the last ready prefix, not the pending layer.
type StackResult struct {
	State         string `json:"state"`
	Ready         int    `json:"ready"`
	Total         int    `json:"total"`
	Ref           string `json:"ref"`
	InputIdentity string `json:"inputIdentity"`
	TargetRef string `json:"targetRef"`
	TargetInputIdentity string `json:"targetInputIdentity"`
}

var runStackLayer = Run

// RunStack builds only missing exact prefixes through Run's existing APFS clone,
// publication and locking owners. Completed prefixes survive interruption; a
// retry joins the same identity and restarts only an unsettled component.
// To supplies the repository; every output tag is its immutable prefix digest.
func RunStack(ctx context.Context, o Options, path string) error {
	_, err := AdvanceStack(ctx, o, path, 0)
	return err
}

// AdvanceStack reuses every qualified prefix and constructs at most maxNew
// components. Zero preserves the unrestricted stack operation. Admission is
// enforced by Run under its existing identity custody, including follower reuse.
func AdvanceStack(ctx context.Context, o Options, path string, maxNew int) (StackResult, error) {
	if maxNew < 0 {
		return StackResult{}, fmt.Errorf("max-new-layers must not be negative")
	}
	if !o.Local || o.DataDiskGiB != 0 {
		return StackResult{}, fmt.Errorf("ordered stacks require local bootable images")
	}
	b, err := buildutil.ReadFile(path, buildutil.FilePolicy{MaxSize: maxConfigSize, OwnerUID: o.OwnerUID, ForbidBit: 0022})
	if err != nil {
		return StackResult{}, err
	}
	var layers []StackLayer
	if err := json.Unmarshal(b, &layers); err != nil {
		return StackResult{}, err
	}
	for i := range layers {
		b, err := buildutil.ReadFile(layers[i].Script, buildutil.FilePolicy{MaxSize: maxScriptSize, OwnerUID: o.OwnerUID, ForbidBit: 0022})
		if err != nil {
			return StackResult{}, err
		}
		sum := sha256.Sum256(b)
		actual := hex.EncodeToString(sum[:])
		if layers[i].ScriptDigest != "" && layers[i].ScriptDigest != actual {
			return StackResult{}, fmt.Errorf("recipe digest mismatch: %s", layers[i].Name)
		}
		layers[i].ScriptDigest = actual
	}
	base, err := downloader.VerifyCached(ctx, o.CacheDir, o.From, resource.RegistryCredentials{})
	if err != nil {
		return StackResult{}, err
	}
	ids, err := StackIdentities(base.ManifestDigest, layers)
	if err != nil {
		return StackResult{}, err
	}
	repo, err := remote.NewRepository(o.To)
	if err != nil {
		return StackResult{}, err
	}
	prefix := repo.Reference.Registry + "/" + repo.Reference.Repository
	return advanceStackLayers(ctx, o, prefix, layers, ids, maxNew)
}

func advanceStackLayers(ctx context.Context, o Options, prefix string, layers []StackLayer, ids []string, maxNew int) (StackResult, error) {
	result := StackResult{State: "pending", Total: len(layers), Ref: o.From,
		TargetRef: prefix+":"+ids[len(ids)-1], TargetInputIdentity: ids[len(ids)-1]}
	built := 0
	for i, l := range layers {
		o.To, o.InputIdentity, o.ScriptPath = prefix+":"+ids[i], ids[i], l.Script
		o.ExpectedScriptDigest = l.ScriptDigest
		reused := false
		waited := false
		o.ReuseResult, o.ReuseOnly = &reused, maxNew > 0 && built >= maxNew
		o.YieldAfterWait, o.WaitedResult = maxNew > 0, &waited
		if err := runStackLayer(ctx, o); err != nil {
			if onlyBuildRequired(err) && (o.ReuseOnly || waited) {
				return result, nil
			}
			return StackResult{}, fmt.Errorf("layer %s (%s): %w", l.Name, ids[i], err)
		}
		if waited {
			built = maxNew
		} else if !reused {
			built++
		}
		result.Ready, result.Ref, result.InputIdentity = i+1, o.To, o.InputIdentity
		o.From = o.To
	}
	result.State = "complete"
	log.G(ctx).Infof("Immutable ordered stack ready: %s", o.To)
	return result, nil
}
