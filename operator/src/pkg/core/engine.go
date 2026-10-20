// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package core

import (
	"context"
	"sync/atomic"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/documentdb/documentdb-operator/internal/engine"
	"github.com/documentdb/documentdb-operator/internal/product"
	util "github.com/documentdb/documentdb-operator/internal/utils"
	"github.com/documentdb/documentdb-operator/internal/validation"
)

// Adapter contract. A product implements ClusterAdapter for its cluster custom
// resource, and BackupAdapter/ScheduledBackupAdapter when it exposes backups.
type (
	NamingProfile                           = product.NamingProfile
	ClusterSpec                             = product.ClusterSpec
	ClusterStatus                           = product.ClusterStatus
	ClusterAdapter[T client.Object]         = product.ClusterAdapter[T]
	BackupAdapter[B client.Object]          = engine.BackupAdapter[B]
	ScheduledBackupAdapter[S client.Object] = engine.ScheduledBackupAdapter[S]
)

// Generic reconcilers. Each is configured with the product adapters and
// registered with SetupWithManager.
type (
	ClusterReconciler[T client.Object]               = engine.ClusterReconciler[T]
	CertificateReconciler[T client.Object]           = engine.CertificateReconciler[T]
	PersistentVolumeReconciler[T client.Object]      = engine.PersistentVolumeReconciler[T]
	BackupReconciler[T, B client.Object]             = engine.BackupReconciler[T, B]
	ScheduledBackupReconciler[T, S, B client.Object] = engine.ScheduledBackupReconciler[T, S, B]

	// ReplicationHooks is the optional multi-region extension point of the
	// cluster reconciler; nil runs every cluster as a single-region primary.
	ReplicationHooks[T client.Object] = engine.ReplicationHooks[T]
	ReplicationContext                = util.ReplicationContext
	SQLExecutor                       = engine.SQLExecutor
)

const (
	// RequeueAfterShort and RequeueAfterLong are the reconcilers' retry delays.
	RequeueAfterShort = engine.RequeueAfterShort
	RequeueAfterLong  = engine.RequeueAfterLong
	// BackupClusterField is the Backup field index the scheduled-backup
	// reconciler registers.
	BackupClusterField = engine.BackupClusterField
)

// NewPodSQLExecutor returns an SQLExecutor that runs psql in the CNPG primary pod.
func NewPodSQLExecutor(c client.Client, clientset kubernetes.Interface, config *rest.Config) SQLExecutor {
	return engine.NewPodSQLExecutor(c, clientset, config)
}

// NewSingleRegionContext returns the replication context of a single-region
// primary cluster.
func NewSingleRegionContext(name, environment, storageClass string) *ReplicationContext {
	return util.NewSingleRegionContext(name, environment, storageClass)
}

// GatewayPort is the gateway port of a profile, or the GATEWAY_PORT env default.
func GatewayPort(p ProductProfile) int32 { return engine.GatewayPort(p) }

// ClusterInstanceStatusChangedPredicate triggers on CNPG instance status changes.
func ClusterInstanceStatusChangedPredicate() predicate.Predicate {
	return engine.ClusterInstanceStatusChangedPredicate()
}

// ServicePrefixPredicate matches Services whose name starts with prefix.
func ServicePrefixPredicate(prefix string) predicate.Predicate {
	return engine.ServicePrefixPredicate(prefix)
}

// Admission validation shared by product webhooks.
type (
	ValidationTerms  = validation.Terms
	RestoreSource    = validation.RestoreSource
	ImageVolumeProbe = validation.ImageVolumeProbe
)

// NewImageVolumeProbe returns the ImageVolume preflight for product. confirmed
// caches a positive result and should be shared across calls.
func NewImageVolumeProbe(c client.Client, confirmed *atomic.Bool, productName string, log logr.Logger) *ImageVolumeProbe {
	return &validation.ImageVolumeProbe{Client: c, Confirmed: confirmed, Product: productName, Log: log}
}

// ResolveBinaryVersion returns the extension version pinned by an image tag or
// version field, or "".
func ResolveBinaryVersion(imageRef, version string) string {
	return validation.ResolveBinaryVersion(imageRef, version)
}

// ImageTagSemver returns the semver of an image reference's tag, or "".
func ImageTagSemver(ref string) string { return validation.ImageTagSemver(ref) }

// ValidateSchemaVersion rejects an explicit schemaVersion above the binary version.
func ValidateSchemaVersion(schemaVersion, binaryVersion string, t ValidationTerms) field.ErrorList {
	return validation.SchemaVersionNotExceedsBinary(schemaVersion, binaryVersion, t)
}

// ValidateImageRollback rejects a binary version below the installed schema.
func ValidateImageRollback(installedSchemaVersion, newBinaryVersion string) field.ErrorList {
	return validation.ImageRollback(installedSchemaVersion, newBinaryVersion)
}

// ValidateStorageResize rejects PVC shrinks.
func ValidateStorageResize(path *field.Path, oldSize, newSize string) field.ErrorList {
	return validation.StorageResize(path, oldSize, newSize)
}

// BackupRestoreSource and PVRestoreSource identify the source of a recovery
// stanza at base.
func BackupRestoreSource(base *field.Path, name string) RestoreSource {
	return validation.BackupSource(base, name)
}

func PVRestoreSource(base *field.Path, name string) RestoreSource {
	return validation.PVSource(base, name)
}

// RestoreSourceSchemaVersion reads the schema version recorded on a restore
// source; readBackup reads it from the product's Backup.
func RestoreSourceSchemaVersion(ctx context.Context, c client.Reader, src RestoreSource, naming NamingProfile, readBackup func() (string, error)) (string, admission.Warnings) {
	return validation.SourceSchemaVersion(ctx, c, src, naming.SchemaVersionAnnotation(), readBackup)
}

// ValidateRestoreCompatibility rejects restoring a schema newer than the binary.
func ValidateRestoreCompatibility(binaryVersion, schemaVersion string, src RestoreSource, t ValidationTerms) (admission.Warnings, field.ErrorList) {
	return validation.CompareBinaryToSchema(binaryVersion, schemaVersion, src, t)
}
