package volumes

import (
	"fmt"
	"path/filepath"

	corev1 "k8s.io/api/core/v1"
)

// ImageMount is a container's mount of a Kubernetes image volume (VolumeSource.Image). The node
// materializes Ref into a directory (pkg/imagevolume) and shares it read-only, as Kubernetes
// defines image volumes; the guest sees it at its mount path's base name, like every share.
type ImageMount struct {
	Name          string
	Ref           string
	ContainerPath string
}

// ImageMounts returns the container's image volume mounts. A subPath is refused: a share is
// always the whole tree.
func ImageMounts(container corev1.Container, pod *corev1.Pod) ([]ImageMount, error) {
	var mounts []ImageMount
	for _, m := range container.VolumeMounts {
		spec := findPodVolumeSpec(pod, m.Name)
		if spec == nil || spec.Image == nil {
			continue
		}
		if spec.Image.Reference == "" {
			return nil, fmt.Errorf("image volume %s names no image reference", m.Name)
		}
		if m.SubPath != "" || m.SubPathExpr != "" {
			return nil, fmt.Errorf("image volume %s: subPath is not supported on this node", m.Name)
		}
		mounts = append(mounts, ImageMount{Name: m.Name, Ref: spec.Image.Reference, ContainerPath: filepath.Clean(m.MountPath)})
	}
	return mounts, nil
}
