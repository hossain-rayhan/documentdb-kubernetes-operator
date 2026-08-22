// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package product

import (
	cnpgv1 "github.com/cloudnative-pg/cloudnative-pg/api/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
)

// Images holds the fully-resolved container images for a cluster.
type Images struct {
	// PostgresExtension is the extension image mounted into PostgreSQL via ImageVolume.
	PostgresExtension string
	// Gateway is the MongoDB wire-protocol gateway sidecar image.
	Gateway string
	// Postgres is the base PostgreSQL image.
	Postgres string
	// PullSecrets are the image pull secrets shared by all cluster containers.
	PullSecrets []corev1.LocalObjectReference
}

// Topology describes the cluster shape and scheduling.
type Topology struct {
	// Instances is the number of PostgreSQL instances in the cluster.
	Instances int
	// Affinity is the CNPG affinity/anti-affinity passthrough.
	Affinity cnpgv1.AffinityConfiguration
}

// Storage describes the persistent volume request. StorageClass is resolved by
// the controller from replication context and passed to the builder separately,
// so it is not part of this adapter-derived model yet.
type Storage struct {
	// PvcSize is the persistent volume claim size (for example "10Gi").
	PvcSize string
}

// Identity carries the owning custom resource's identity for owner references
// and resource labels.
type Identity struct {
	Name       string
	UID        types.UID
	APIVersion string
	Kind       string
}

// Postgres carries the operator-managed PostgreSQL process and init tuning taken
// from the custom resource. Parameter/GUC assembly stays product-specific in the
// builder and is not represented here yet.
type Postgres struct {
	// UID and GID are the process identity overrides; nil leaves the CNPG default.
	UID *int64
	GID *int64
	// PostInitSQL is appended to the mandatory bootstrap SQL.
	PostInitSQL []string
}

// FeatureGates carries the resolved feature-gate flags the builder acts on.
type FeatureGates struct {
	IOUring bool
}

// Recovery describes a bootstrap-from-source request. A nil Recovery on Bootstrap
// means default initialization.
type Recovery struct {
	BackupName           string
	PersistentVolumeName string
}

// Bootstrap describes how the cluster is initialized.
type Bootstrap struct {
	Recovery *Recovery
}

// ClusterIntent is the product-neutral desired state the reconciler renders into
// a CNPG Cluster. Product adapters populate it from their custom resource; the
// reconciler consumes it without product-branding logic. Fields are added to
// this struct as the builder is progressively rewired onto the seam.
type ClusterIntent struct {
	// Images are the resolved extension, gateway, and postgres images.
	Images Images

	// Topology is the cluster shape and scheduling.
	Topology Topology

	// Storage is the persistent volume request.
	Storage Storage

	// Identity is the owning custom resource's identity.
	Identity Identity

	// Postgres is the operator-managed PostgreSQL process and init tuning.
	Postgres Postgres

	// FeatureGates are the resolved feature-gate flags.
	FeatureGates FeatureGates

	// Bootstrap describes how the cluster is initialized.
	Bootstrap Bootstrap

	// CredentialSecret is the resolved credential secret name.
	CredentialSecret string

	// SidecarInjectorPlugin and WALReplicaPlugin are the resolved CNPG plugin
	// names to wire onto the Cluster.
	SidecarInjectorPlugin string
	WALReplicaPlugin      string

	// Product is the profile this intent was produced from.
	Product ProductProfile
}
