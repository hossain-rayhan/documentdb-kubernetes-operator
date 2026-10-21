// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cnpg

import (
	"os"

	cnpgv1 "github.com/cloudnative-pg/cloudnative-pg/api/v1"

	"github.com/documentdb/documentdb-operator/internal/imagemode"
	util "github.com/documentdb/documentdb-operator/internal/utils"
)

// IsCombinedImageMode reports whether an existing CNPG Cluster was created in
// combined image mode. The mode is fixed at creation and recorded in the
// documentdb.io/image-mode annotation; clusters without it (created by earlier
// operator versions) are never treated as combined, so their image is not
// rewritten.
func IsCombinedImageMode(cluster *cnpgv1.Cluster) bool {
	return cluster.Annotations[imagemode.AnnotationKey] == string(imagemode.Combined)
}

// ApplyCombinedImageMode converts a rendered ImageVolume-mode Cluster into
// combined image mode: PostgreSQL runs from combinedImage, which already ships
// the DocumentDB extension, and no extension image volume is declared. The
// shared_preload_libraries list is unchanged because the combined image
// installs the same libraries into the default PostgreSQL paths.
func ApplyCombinedImageMode(cluster *cnpgv1.Cluster, combinedImage string) {
	cluster.Spec.ImageName = combinedImage
	cluster.Spec.PostgresConfiguration.Extensions = nil
	if pullPolicy := parsePullPolicy(os.Getenv(util.DOCUMENTDB_IMAGE_PULL_POLICY_ENV)); pullPolicy != "" {
		cluster.Spec.ImagePullPolicy = pullPolicy
	}
}
