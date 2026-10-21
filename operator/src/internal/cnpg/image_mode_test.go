// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cnpg

import (
	"context"

	cnpgv1 "github.com/cloudnative-pg/cloudnative-pg/api/v1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/documentdb/documentdb-operator/internal/imagemode"
	util "github.com/documentdb/documentdb-operator/internal/utils"
)

var _ = Describe("Combined image mode", func() {
	const combinedImage = "ghcr.io/documentdb/documentdb-kubernetes-operator/documentdb-combined:18-1.0.0"

	It("detects combined mode only from the image-mode annotation", func() {
		cluster := baseCluster("c", "ns")
		Expect(IsCombinedImageMode(cluster)).To(BeFalse())

		// Clusters from earlier operator versions may lack the extension entry
		// (e.g. 0.1.x documentdb-local images); they must not be treated as combined.
		cluster.Spec.PostgresConfiguration.Extensions = nil
		Expect(IsCombinedImageMode(cluster)).To(BeFalse())

		cluster.Annotations = map[string]string{imagemode.AnnotationKey: string(imagemode.ImageVolume)}
		Expect(IsCombinedImageMode(cluster)).To(BeFalse())
		cluster.Annotations[imagemode.AnnotationKey] = string(imagemode.Combined)
		Expect(IsCombinedImageMode(cluster)).To(BeTrue())
	})

	It("runs PostgreSQL from the combined image without extension volumes", func() {
		GinkgoT().Setenv(util.DOCUMENTDB_IMAGE_PULL_POLICY_ENV, "Never")
		cluster := baseCluster("c", "ns")
		cluster.Spec.PostgresConfiguration.AdditionalLibraries = []string{"pg_cron", "pg_documentdb_core", "pg_documentdb"}

		ApplyCombinedImageMode(cluster, combinedImage)

		Expect(cluster.Spec.ImageName).To(Equal(combinedImage))
		Expect(cluster.Spec.PostgresConfiguration.Extensions).To(BeEmpty())
		Expect(cluster.Spec.ImagePullPolicy).To(Equal(corev1.PullNever))
		Expect(cluster.Spec.PostgresConfiguration.AdditionalLibraries).To(ConsistOf("pg_cron", "pg_documentdb_core", "pg_documentdb"))
	})

	It("syncs a combined image upgrade through spec.imageName without a restart annotation", func() {
		current := baseCluster("c", "ns")
		ApplyCombinedImageMode(current, "repo/documentdb-combined:18-0.117.0")
		desired := baseCluster("c", "ns")
		ApplyCombinedImageMode(desired, combinedImage)

		c := buildFakeClient(current).Build()
		Expect(SyncCnpgCluster(context.Background(), c, current, desired, nil)).To(Succeed())

		updated := &cnpgv1.Cluster{}
		Expect(c.Get(context.Background(), types.NamespacedName{Name: "c", Namespace: "ns"}, updated)).To(Succeed())
		Expect(updated.Spec.ImageName).To(Equal(combinedImage))
		Expect(updated.Spec.PostgresConfiguration.Extensions).To(BeEmpty())
		Expect(updated.Annotations).NotTo(HaveKey("kubectl.kubernetes.io/restartedAt"))
	})
})
