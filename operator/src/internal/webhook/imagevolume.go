// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package webhook

import (
	"context"
	"fmt"
	"sync/atomic"

	"github.com/documentdb/documentdb-operator/internal/imagemode"
)

// imageVolumeConfirmed caches a positive detection of ImageVolume support.
//
// Only positive results are cached: once the cluster is known to support
// ImageVolume, subsequent creates skip the probe entirely. A negative or
// inconclusive result is never cached, so if an administrator enables the
// feature gate later the next create re-probes and succeeds without restarting
// the operator.
var imageVolumeConfirmed atomic.Bool

// ensureImageVolumeSupported blocks DocumentDB creation when the operator is
// forced into ImageVolume mode (DOCUMENTDB_IMAGE_MODE=imageVolume) and the API
// server does not support the Kubernetes ImageVolume feature.
//
// In the default "auto" mode, and in "combined" mode, creation is never blocked:
// the controller falls back to the combined PostgreSQL + DocumentDB image when
// ImageVolume is unavailable (see docs/designs/combined-image-fallback.md).
//
// The check is fail-open: it blocks only when it can positively prove the
// feature is unavailable.
func (v *DocumentDBValidator) ensureImageVolumeSupported(ctx context.Context, namespace string) error {
	if imagemode.ConfiguredMode() != imagemode.ImageVolume {
		return nil
	}
	if imageVolumeConfirmed.Load() {
		return nil
	}

	switch imagemode.ProbeAPI(ctx, v.Client, namespace) {
	case imagemode.ProbeSupported:
		imageVolumeConfirmed.Store(true)
		return nil
	case imagemode.ProbeInconclusive:
		return nil
	}

	return fmt.Errorf(
		"the Kubernetes ImageVolume feature is not enabled on this cluster, but the DocumentDB operator is " +
			"configured with imageMode=imageVolume, which mounts the DocumentDB extension into PostgreSQL pods " +
			"through image volumes. ImageVolume is GA (on by default) in Kubernetes 1.35+. On Kubernetes 1.33/1.34 " +
			"you may enable the ImageVolume feature gate on both the kube-apiserver and every kubelet; the container " +
			"runtime must also support it (containerd >= 2.1 or CRI-O >= 1.33). Alternatively set the Helm value " +
			"imageMode to \"auto\" or \"combined\" to use the combined PostgreSQL + DocumentDB image. " +
			"See https://kubernetes.io/docs/concepts/storage/volumes/#image",
	)
}
