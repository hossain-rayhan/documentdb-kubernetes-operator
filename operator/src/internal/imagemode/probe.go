// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package imagemode

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// ProbeResult is the outcome of the API-server ImageVolume probe.
type ProbeResult int

const (
	// ProbeInconclusive means the environment rejected even the baseline Pod
	// (quota, another admission webhook), so nothing can be concluded.
	ProbeInconclusive ProbeResult = iota
	// ProbeSupported means the API server accepts and retains image volumes.
	ProbeSupported
	// ProbeUnsupported means the API server rejects or prunes image volumes.
	ProbeUnsupported
)

// probeImage is a syntactically valid image reference used only for the
// dry-run probe. It is never pulled or scheduled because every probe Pod is
// submitted with client.DryRunAll.
const probeImage = "registry.k8s.io/pause:3.10"

// probeVolumeName is the volume/mount name shared by both probe Pods.
const probeVolumeName = "probe"

// ProbeAPI performs a differential server-side dry-run to detect whether the
// API server supports ImageVolume, without relying on error-string matching.
//
// It submits two dry-run Pods that are identical except for a single volume:
//   - baseline: an emptyDir volume (always a valid volume source), and
//   - image:    an ImageVolumeSource volume.
//
// Interpretation:
//   - baseline rejected             -> inconclusive (the environment blocks
//     pod creation entirely).
//   - baseline ok, image rejected   -> unsupported.
//   - baseline ok, image accepted but the ImageVolumeSource was pruned from the
//     returned Pod -> unsupported. On Kubernetes 1.33/1.34 with the beta gate
//     OFF the API server admits the Pod and silently strips the disabled field.
//   - baseline ok, image accepted and retained -> supported.
//
// The probe only covers the API server. Whether the node container runtime can
// actually mount image volumes is checked separately (see CheckNodes).
func ProbeAPI(ctx context.Context, c client.Client, namespace string) ProbeResult {
	logger := log.FromContext(ctx)

	if _, err := dryRunProbePod(ctx, c, namespace, baselineProbeVolume()); err != nil {
		logger.Info("ImageVolume API probe inconclusive: baseline probe pod rejected", "error", err)
		return ProbeInconclusive
	}

	returned, err := dryRunProbePod(ctx, c, namespace, imageProbeVolume())
	if err != nil {
		logger.Info("ImageVolume API probe: image-volume probe pod rejected while baseline passed; feature unavailable", "error", err)
		return ProbeUnsupported
	}

	if !hasImageVolume(returned) {
		logger.Info("ImageVolume API probe: image-volume probe pod admitted but the ImageVolumeSource was pruned; feature gate is off")
		return ProbeUnsupported
	}

	return ProbeSupported
}

// hasImageVolume reports whether the server-returned Pod still carries the
// probe volume as an ImageVolumeSource.
func hasImageVolume(pod *corev1.Pod) bool {
	if pod == nil {
		return false
	}
	for _, vol := range pod.Spec.Volumes {
		if vol.Name == probeVolumeName {
			return vol.Image != nil
		}
	}
	return false
}

// dryRunProbePod submits a minimal Pod carrying the given probe volume as a
// server-side dry-run (never persisted) and returns the Pod as processed by the
// API server. The Pod satisfies the Pod Security "restricted" profile so that
// namespace policies do not make the probe inconclusive.
func dryRunProbePod(ctx context.Context, c client.Client, namespace string, volume corev1.Volume) (*corev1.Pod, error) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "documentdb-imagevolume-preflight-",
			Namespace:    namespace,
		},
		Spec: corev1.PodSpec{
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot:   ptr.To(true),
				RunAsUser:      ptr.To[int64](65535),
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			Containers: []corev1.Container{{
				Name:  "probe",
				Image: probeImage,
				SecurityContext: &corev1.SecurityContext{
					AllowPrivilegeEscalation: ptr.To(false),
					Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
				},
				VolumeMounts: []corev1.VolumeMount{{
					Name:      probeVolumeName,
					MountPath: "/probe",
				}},
			}},
			Volumes: []corev1.Volume{volume},
		},
	}
	err := c.Create(ctx, pod, client.DryRunAll)
	return pod, err
}

// baselineProbeVolume returns an emptyDir volume, which every Kubernetes
// version accepts.
func baselineProbeVolume() corev1.Volume {
	return corev1.Volume{
		Name:         probeVolumeName,
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
	}
}

// imageProbeVolume returns an ImageVolumeSource volume, the feature under test.
func imageProbeVolume() corev1.Volume {
	return corev1.Volume{
		Name: probeVolumeName,
		VolumeSource: corev1.VolumeSource{
			Image: &corev1.ImageVolumeSource{
				Reference:  probeImage,
				PullPolicy: corev1.PullIfNotPresent,
			},
		},
	}
}
