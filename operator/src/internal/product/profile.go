// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package product defines the product-neutral seam the operator reconciles
// against. A ProductProfile captures the identity, image defaults, and CNPG
// plugin/secret conventions that vary between operator products, and an Adapter
// translates a product-specific custom resource into that neutral model. The
// reconciler consumes the profile and a ClusterIntent, so it carries no
// product-branding logic; each product supplies its own adapter without changing
// the reconciler.
package product

// ProductProfile captures the product-varying identity and defaults for an
// operator product. Values are sourced from the canonical operator constants so
// existing behavior is preserved exactly.
type ProductProfile struct {
	// Name is the human-readable product name.
	Name string

	// ExtensionImageRepo and GatewayImageRepo are the image repositories used
	// when resolving an image from a bare version string.
	ExtensionImageRepo string
	GatewayImageRepo   string

	// DefaultExtensionImage and DefaultGatewayImage are the fully-qualified
	// images used when neither an explicit image nor a version is supplied.
	DefaultExtensionImage string
	DefaultGatewayImage   string

	// DefaultCredentialSecret is the credential secret name used when the custom
	// resource does not specify one.
	DefaultCredentialSecret string

	// SidecarInjectorPlugin and WALReplicaPlugin are the CNPG plugin names the
	// operator wires onto the rendered Cluster.
	SidecarInjectorPlugin string
	WALReplicaPlugin      string
}

// Adapter is the product-neutral contract shared by every product integration:
// it exposes the product's static Profile. Each concrete adapter (for example
// DocumentDBAdapter) also provides a typed ToClusterIntent for its own custom
// resource; that method is not on this interface because the custom-resource
// type is product-specific.
type Adapter interface {
	// Profile returns the static product profile.
	Profile() ProductProfile
}
