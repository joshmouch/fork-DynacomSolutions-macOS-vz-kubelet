package imagebuild

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/agoda-com/macOS-vz-kubelet/internal/buildutil"
	"github.com/agoda-com/macOS-vz-kubelet/pkg/downloader"
	"github.com/agoda-com/macOS-vz-kubelet/pkg/resource"
	"github.com/virtual-kubelet/virtual-kubelet/log"
	"oras.land/oras-go/v2/registry/remote"
)

// RunStack builds only missing exact prefixes through Run's existing APFS clone,
// publication and locking owners. Completed prefixes survive interruption; a
// retry joins the same identity and restarts only an unsettled component.
// To supplies the repository; every output tag is its immutable prefix digest.
func RunStack(ctx context.Context, o Options, path string) error {
	if !o.Local || o.DataDiskGiB != 0 {
		return fmt.Errorf("ordered stacks require local bootable images")
	}
	b, err := buildutil.ReadFile(path, buildutil.FilePolicy{MaxSize: maxConfigSize, OwnerUID: o.OwnerUID, ForbidBit: 0022})
	if err != nil {
		return err
	}
	var layers []StackLayer
	if err := json.Unmarshal(b, &layers); err != nil {
		return err
	}
	for i := range layers {
		b, err := buildutil.ReadFile(layers[i].Script, buildutil.FilePolicy{MaxSize: maxScriptSize, OwnerUID: o.OwnerUID, ForbidBit: 0022})
		if err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		actual := hex.EncodeToString(sum[:])
		if layers[i].ScriptDigest != "" && layers[i].ScriptDigest != actual {
			return fmt.Errorf("recipe digest mismatch: %s", layers[i].Name)
		}
		layers[i].ScriptDigest = actual
	}
	base, err := downloader.VerifyCached(ctx, o.CacheDir, o.From, resource.RegistryCredentials{})
	if err != nil {
		return err
	}
	ids, err := StackIdentities(base.ManifestDigest, layers)
	if err != nil {
		return err
	}
	repo, err := remote.NewRepository(o.To)
	if err != nil {
		return err
	}
	prefix := repo.Reference.Registry + "/" + repo.Reference.Repository
	for i, l := range layers {
		o.To, o.InputIdentity, o.ScriptPath = prefix+":"+ids[i], ids[i], l.Script
		o.ExpectedScriptDigest = l.ScriptDigest
		if err := Run(ctx, o); err != nil {
			return fmt.Errorf("layer %s (%s): %w", l.Name, ids[i], err)
		}
		o.From = o.To
	}
	log.G(ctx).Infof("Immutable ordered stack ready: %s", o.To)
	return nil
}
