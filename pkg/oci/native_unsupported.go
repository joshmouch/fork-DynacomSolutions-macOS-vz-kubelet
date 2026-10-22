//go:build !darwin

package oci

import (
	"context"
	"errors"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

const AnnotationNativeSeal = "com.agoda.macosvz.local-native-seal.v1"

func nativeCached(ocispec.Descriptor, string) bool { return false }
func ReleaseNativeFiles(string) error              { return nil }
func InstallNativeLocal(context.Context, string, string, ocispec.Manifest, map[string]string) (ocispec.Manifest, error) {
	return ocispec.Manifest{}, errors.New("native local snapshots require macOS APFS")
}
