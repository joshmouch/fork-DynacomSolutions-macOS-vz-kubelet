package buildutil

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"

	"github.com/agoda-com/macOS-vz-kubelet/internal/disk"
	"github.com/agoda-com/macOS-vz-kubelet/pkg/oci"

	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/virtual-kubelet/virtual-kubelet/log"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
)

// ArtifactType is the artifact type of macOS images (same as the images the provider pulls).
const ArtifactType = "application/vnd.agoda.macosvz.artifact"

// PushOptions describe one image push.
type PushOptions struct {
	Ref         string // destination name:tag
	Creds       Credentials
	Disk        string // path of disk.img
	Aux         string // path of aux.img; empty with Config for a data disk (disk layer only)
	Config      string // path of config.json (copied byte for byte from the base image); empty with Aux
	Annotations map[string]string
}

// PushResult reports what was pushed.
type PushResult struct {
	ManifestDigest digest.Digest
	Manifest       ocispec.Manifest
}

// Push uploads the image with no temp file: each gzip layer is first compressed into a hash
// (reading the file once, writing nothing) to learn the digest and size the registry upload
// needs up front, then compressed again straight into the upload. The disk must not change
// between the two reads; the registry rejects the layer if it did.
func Push(ctx context.Context, o PushOptions) (PushResult, error) {
	return publish(ctx, o, "")
}

// CacheLocal consumes the private input files into the provider cache without contacting
// the destination registry. It builds exactly the same descriptors as Push.
func CacheLocal(ctx context.Context, o PushOptions, cacheDir string) (PushResult, error) {
	if cacheDir == "" {
		return PushResult{}, errors.New("local cache directory is required")
	}
	return publish(ctx, o, cacheDir)
}

// CacheNativeLocal publishes stopped-VM files through the node-local immutable
// descriptor contract, avoiding full disk compression and byte verification.
// Push and CacheLocal retain the portable byte-digest contract for imports.
func CacheNativeLocal(ctx context.Context, o PushOptions, cacheDir string) (PushResult, error) {
	if cacheDir == "" || (o.Aux == "") != (o.Config == "") {
		return PushResult{}, errors.New("native cache requires a cache root and paired aux/config")
	}
	paths := map[string]string{oci.MediaTypeDiskImage.Title(): o.Disk}
	types := []oci.MediaType{oci.MediaTypeDiskImage}
	if o.Aux != "" {
		paths[oci.MediaTypeAuxImage.Title()], paths[oci.MediaTypeConfigV1.Title()] = o.Aux, o.Config
		types = append(types, oci.MediaTypeAuxImage, oci.MediaTypeConfigV1)
		b, err := os.ReadFile(o.Config)
		if err != nil {
			return PushResult{}, err
		}
		var cfg oci.Config
		if json.Unmarshal(b, &cfg) != nil || cfg.HardwareModelData == "" || cfg.MachineIdData == "" {
			return PushResult{}, errors.New("invalid native platform config")
		}
	}
	m := ocispec.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: ocispec.MediaTypeImageManifest,
		ArtifactType: ArtifactType, Config: ocispec.DescriptorEmptyJSON, Annotations: o.Annotations}
	for _, mt := range types {
		m.Layers = append(m.Layers, ocispec.Descriptor{MediaType: string(mt), Annotations: map[string]string{ocispec.AnnotationTitle: mt.Title()}})
	}
	m, err := oci.InstallNativeLocal(ctx, cacheDir, o.Ref, m, paths)
	if err != nil {
		return PushResult{}, err
	}
	b, err := json.Marshal(m)
	return PushResult{Manifest: m, ManifestDigest: digest.FromBytes(b)}, err
}

func publish(ctx context.Context, o PushOptions, cacheDir string) (PushResult, error) {
	if (o.Aux == "") != (o.Config == "") {
		return PushResult{}, errors.New("aux and config must be set together: both for a bootable image, neither for a data disk")
	}
	dataDisk := o.Aux == ""
	logger := log.G(ctx)

	// Pass 1: digests and sizes, nothing stored.
	type gzLayer struct {
		desc ocispec.Descriptor
		path string
	}
	var layers []gzLayer
	type layerIn struct {
		mt   oci.MediaType
		path string
	}
	ins := []layerIn{{oci.MediaTypeDiskImage, o.Disk}}
	if !dataDisk {
		ins = append(ins, layerIn{oci.MediaTypeAuxImage, o.Aux})
	}
	for _, l := range ins {
		logger.Infof("Hashing %s (pass 1 of 2, nothing is written)", l.mt.Title())
		r, err := disk.CompressToWriter(ctx, l.path, io.Discard)
		if err != nil {
			return PushResult{}, fmt.Errorf("failed to hash %s: %w", l.mt.Title(), err)
		}
		layers = append(layers, gzLayer{path: l.path, desc: ocispec.Descriptor{
			MediaType: string(l.mt),
			Digest:    r.GzDigest,
			Size:      r.CompressedSize,
			Annotations: map[string]string{
				ocispec.AnnotationTitle:          l.mt.Title(),
				oci.AnnotationUncompressedSize:   fmt.Sprintf("%d", r.UncompressedSize),
				oci.AnnotationUncompressedDigest: r.UncompressedDigest.String(),
			},
		}})
	}

	var cfgBytes []byte
	var cfgDesc ocispec.Descriptor
	if !dataDisk {
		var err error
		cfgBytes, err = os.ReadFile(o.Config)
		if err != nil {
			return PushResult{}, err
		}
		var cfg oci.Config
		if err := json.Unmarshal(cfgBytes, &cfg); err != nil || cfg.HardwareModelData == "" || cfg.MachineIdData == "" {
			return PushResult{}, errors.New("config.json has no hardware model or machine identifier")
		}
		cfgDesc = ocispec.Descriptor{
			MediaType:   string(oci.MediaTypeConfigV1),
			Digest:      digest.FromBytes(cfgBytes),
			Size:        int64(len(cfgBytes)),
			Annotations: map[string]string{ocispec.AnnotationTitle: oci.MediaTypeConfigV1.Title()},
		}
	}

	emptyCfg := ocispec.DescriptorEmptyJSON
	emptyCfg.Platform = &ocispec.Platform{Architecture: "arm64", OS: "darwin"}

	annotations := map[string]string{ocispec.AnnotationCreated: time.Now().UTC().Format(time.RFC3339)}
	for k, v := range o.Annotations {
		annotations[k] = v
	}
	manifestLayers := []ocispec.Descriptor{layers[0].desc}
	if !dataDisk {
		manifestLayers = append(manifestLayers, layers[1].desc, cfgDesc)
	}
	manifest := ocispec.Manifest{
		Versioned:    specs.Versioned{SchemaVersion: 2},
		MediaType:    ocispec.MediaTypeImageManifest,
		ArtifactType: ArtifactType,
		Config:       emptyCfg,
		Layers:       manifestLayers,
		Annotations:  annotations,
	}
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		return PushResult{}, err
	}
	manifestDesc := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageManifest,
		Digest:    digest.FromBytes(manifestBytes),
		Size:      int64(len(manifestBytes)),
	}

	if cacheDir != "" {
		paths := map[string]string{
			oci.MediaTypeDiskImage.Title(): o.Disk,
			oci.MediaTypeAuxImage.Title():  o.Aux,
			oci.MediaTypeConfigV1.Title():  o.Config,
		}
		if err := oci.InstallLocal(ctx, cacheDir, o.Ref, manifestBytes, paths); err != nil {
			return PushResult{}, err
		}
		return PushResult{ManifestDigest: manifestDesc.Digest, Manifest: manifest}, nil
	}

	repo, err := remote.NewRepository(o.Ref)
	if err != nil {
		return PushResult{}, err
	}
	repo.PlainHTTP = isLocal(repo.Reference.Registry)
	repo.Client = &auth.Client{
		Credential: auth.StaticCredential(repo.Reference.Registry, auth.Credential{
			Username: o.Creds.Username, Password: o.Creds.Password, RefreshToken: o.Creds.IdentityToken,
		}),
	}
	ctx = auth.AppendRepositoryScope(ctx, repo.Reference, auth.ActionPull, auth.ActionPush)
	blobs := repo.Blobs()
	// Pass 2: stream each gzip layer into the upload.
	for _, l := range layers {
		if ok, err := blobs.Exists(ctx, l.desc); err == nil && ok {
			logger.Infof("Layer %s already in the registry, skipping upload", l.desc.Annotations[ocispec.AnnotationTitle])
			continue
		}
		logger.Infof("Uploading %s (pass 2 of 2): %d bytes compressed", l.desc.Annotations[ocispec.AnnotationTitle], l.desc.Size)
		if err := streamLayer(ctx, blobs, l.desc, l.path); err != nil {
			return PushResult{}, err
		}
	}
	type smallBlob struct {
		d ocispec.Descriptor
		c []byte
	}
	smallBlobs := []smallBlob{{emptyCfg, ocispec.DescriptorEmptyJSON.Data}}
	if !dataDisk {
		smallBlobs = append(smallBlobs, smallBlob{cfgDesc, cfgBytes})
	}
	for _, b := range smallBlobs {
		if ok, err := blobs.Exists(ctx, b.d); err == nil && ok {
			continue
		}
		if err := blobs.Push(ctx, b.d, bytes.NewReader(b.c)); err != nil {
			return PushResult{}, fmt.Errorf("failed to push %s: %w", b.d.MediaType, err)
		}
	}
	if err := repo.Manifests().PushReference(ctx, manifestDesc, bytes.NewReader(manifestBytes), repo.Reference.Reference); err != nil {
		return PushResult{}, fmt.Errorf("failed to push manifest: %w", err)
	}
	return PushResult{ManifestDigest: manifestDesc.Digest, Manifest: manifest}, nil
}

// pushTarget is the part of a blob store streamLayer needs.
type pushTarget interface {
	Push(ctx context.Context, expected ocispec.Descriptor, content io.Reader) error
}

// streamLayer compresses path into a pipe that the upload reads, and checks that the bytes
// produced match the digest and size promised by pass 1.
func streamLayer(ctx context.Context, t pushTarget, want ocispec.Descriptor, path string) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	pr, pw := io.Pipe()
	type result struct {
		r   disk.CompressionResult
		err error
	}
	done := make(chan result, 1)
	go func() {
		r, err := disk.CompressToWriter(ctx, path, pw)
		pw.CloseWithError(err)
		done <- result{r, err}
	}()
	pushErr := t.Push(ctx, want, pr)
	pr.CloseWithError(pushErr) // unblock the compressor if the upload stopped early
	cancel()
	res := <-done
	if pushErr != nil {
		return fmt.Errorf("failed to push %s: %w", want.Annotations[ocispec.AnnotationTitle], pushErr)
	}
	if res.err != nil {
		return res.err
	}
	if res.r.GzDigest != want.Digest || res.r.CompressedSize != want.Size {
		return fmt.Errorf("%s changed between the two reads (digest %s, want %s)", want.Annotations[ocispec.AnnotationTitle], res.r.GzDigest, want.Digest)
	}
	return nil
}

// isLocal mirrors the provider's rule: plain HTTP only for localhost and local IPs.
func isLocal(host string) bool {
	host = strings.Split(host, ":")[0]
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && (ip.IsLoopback() || ip.IsPrivate())
}
