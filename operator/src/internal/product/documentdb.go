// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package product

import (
	"os"

	dbpreview "github.com/documentdb/documentdb-operator/api/preview"
	util "github.com/documentdb/documentdb-operator/internal/utils"
)

// DocumentDBProfile returns the ProductProfile for the DocumentDB product. Every
// field is sourced from the canonical operator constants so this profile stays
// the single mapping between product identity and operator defaults.
func DocumentDBProfile() ProductProfile {
	return ProductProfile{
		Name:                    "DocumentDB",
		ExtensionImageRepo:      util.ExtensionImageRepo(),
		GatewayImageRepo:        util.GatewayImageRepo(),
		DefaultTag:              util.DEFAULT_DOCUMENTDB_TAG,
		DefaultCredentialSecret: util.DEFAULT_DOCUMENTDB_CREDENTIALS_SECRET,
		SidecarInjectorPlugin:   util.DEFAULT_SIDECAR_INJECTOR_PLUGIN,
		WALReplicaPlugin:        util.DEFAULT_WAL_REPLICA_PLUGIN,
	}
}

// DocumentDBAdapter is the first product adapter. It maps the DocumentDB custom
// resource onto the product-neutral model consumed by the reconciler.
type DocumentDBAdapter struct{}

// Profile returns the DocumentDB product profile.
func (DocumentDBAdapter) Profile() ProductProfile {
	return DocumentDBProfile()
}

// ExtensionImage resolves the extension image for a DocumentDB instance using the
// product profile's repo and default. The resolution priority is shared across
// products via util.ResolveComponentImage.
func (a DocumentDBAdapter) ExtensionImage(db *dbpreview.DocumentDB) string {
	p := a.Profile()
	var explicit string
	if db.Spec.Image != nil {
		explicit = db.Spec.Image.DocumentDB
	}
	return util.ResolveComponentImage(
		p.ExtensionImageRepo,
		p.DefaultTag,
		explicit,
		db.Spec.DocumentDBVersion,
		os.Getenv(util.DOCUMENTDB_VERSION_ENV),
		util.CHANGESTREAM_DOCUMENTDB_IMAGE,
		dbpreview.IsFeatureGateEnabled(db, dbpreview.FeatureGateChangeStreams),
	)
}

// CombinedImage resolves the combined PostgreSQL + DocumentDB image used when
// the cluster runs in combined image mode. Change-stream images have no
// combined counterpart, so the change-stream feature gate does not affect it.
func (a DocumentDBAdapter) CombinedImage(db *dbpreview.DocumentDB) string {
	var explicit string
	if db.Spec.Image != nil {
		explicit = db.Spec.Image.DocumentDB
	}
	return util.CombinedImage(explicit, db.Spec.DocumentDBVersion, os.Getenv(util.DOCUMENTDB_VERSION_ENV))
}

// GatewayImage resolves the gateway image for a DocumentDB instance using the
// product profile's repo and default.
func (a DocumentDBAdapter) GatewayImage(db *dbpreview.DocumentDB) string {
	p := a.Profile()
	var explicit string
	if db.Spec.Image != nil {
		explicit = db.Spec.Image.Gateway
	}
	return util.ResolveComponentImage(
		p.GatewayImageRepo,
		p.DefaultTag,
		explicit,
		db.Spec.DocumentDBVersion,
		os.Getenv(util.DOCUMENTDB_VERSION_ENV),
		util.CHANGESTREAM_GATEWAY_IMAGE,
		dbpreview.IsFeatureGateEnabled(db, dbpreview.FeatureGateChangeStreams),
	)
}

// ToClusterIntent maps a DocumentDB custom resource onto the product-neutral
// ClusterIntent, resolving product-varying images, credential secret, and plugin
// names via the product profile.
func (a DocumentDBAdapter) ToClusterIntent(db *dbpreview.DocumentDB) ClusterIntent {
	p := a.Profile()

	credentialSecret := db.Spec.DocumentDbCredentialSecret
	if credentialSecret == "" {
		credentialSecret = p.DefaultCredentialSecret
	}

	sidecarPlugin := p.SidecarInjectorPlugin
	if db.Spec.Plugins != nil && db.Spec.Plugins.SidecarInjectorName != "" {
		sidecarPlugin = db.Spec.Plugins.SidecarInjectorName
	}

	var postgresImage string
	if db.Spec.Image != nil && db.Spec.Image.Postgres != "" {
		// A CR-pinned operand image wins.
		postgresImage = db.Spec.Image.Postgres
	} else {
		// Otherwise use the operator-level default (POSTGRES_IMAGE env). When that
		// is empty the field stays empty and CloudNativePG applies its built-in
		// operand default.
		postgresImage = util.PostgresImage()
	}

	var pg Postgres
	if db.Spec.Postgres != nil {
		pg = Postgres{
			UID:         db.Spec.Postgres.UID,
			GID:         db.Spec.Postgres.GID,
			PostInitSQL: db.Spec.Postgres.PostInitSQL,
		}
	}

	var bootstrap Bootstrap
	if db.Spec.Bootstrap != nil && db.Spec.Bootstrap.Recovery != nil {
		recovery := db.Spec.Bootstrap.Recovery
		r := Recovery{BackupName: recovery.Backup.Name}
		if recovery.PersistentVolume != nil {
			r.PersistentVolumeName = recovery.PersistentVolume.Name
		}
		bootstrap.Recovery = &r
	}

	extensionImage := a.ExtensionImage(db)

	return ClusterIntent{
		Images: Images{
			PostgresExtension: extensionImage,
			Combined:          a.CombinedImage(db),
			Gateway:           a.GatewayImage(db),
			Postgres:          postgresImage,
			PullSecrets:       db.Spec.ImagePullSecrets,
		},
		Topology: Topology{
			Instances: db.Spec.InstancesPerNode,
			Affinity:  db.Spec.Affinity,
		},
		Storage: Storage{
			PvcSize: db.Spec.Resource.Storage.PvcSize,
		},
		Identity: Identity{
			Name:       db.Name,
			UID:        db.UID,
			APIVersion: db.APIVersion,
			Kind:       db.Kind,
		},
		Postgres: pg,
		FeatureGates: FeatureGates{
			IOUring: dbpreview.IsFeatureGateEnabled(db, dbpreview.FeatureGateIOUring),
		},
		Bootstrap:             bootstrap,
		CredentialSecret:      credentialSecret,
		SidecarInjectorPlugin: sidecarPlugin,
		WALReplicaPlugin:      p.WALReplicaPlugin,
		Product:               p,
	}
}

// compile-time assertion that DocumentDBAdapter satisfies the Adapter seam.
var _ Adapter = DocumentDBAdapter{}
