// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package product

import (
	"testing"

	dbpreview "github.com/documentdb/documentdb-operator/api/preview"
	util "github.com/documentdb/documentdb-operator/internal/utils"
)

// TestDocumentDBAdapterExtensionImage covers the extension-image resolution
// priority: explicit image, then spec version, then the change-stream override,
// then the product default.
func TestDocumentDBAdapterExtensionImage(t *testing.T) {
	a := DocumentDBAdapter{}
	tests := []struct {
		name string
		spec dbpreview.DocumentDBSpec
		want string
	}{
		{"explicit image overrides feature gate", dbpreview.DocumentDBSpec{
			Image:        &dbpreview.ImageSpec{DocumentDB: "custom-registry/custom-image:v1"},
			FeatureGates: map[string]bool{dbpreview.FeatureGateChangeStreams: true},
		}, "custom-registry/custom-image:v1"},
		{"documentDBVersion resolves image", dbpreview.DocumentDBSpec{DocumentDBVersion: "1.2.3"}, util.DOCUMENTDB_EXTENSION_IMAGE_REPO + ":1.2.3"},
		{"explicit overrides documentDBVersion", dbpreview.DocumentDBSpec{
			Image:             &dbpreview.ImageSpec{DocumentDB: "custom-registry/custom-image:v1"},
			DocumentDBVersion: "1.2.3",
		}, "custom-registry/custom-image:v1"},
		{"changestream enabled", dbpreview.DocumentDBSpec{FeatureGates: map[string]bool{dbpreview.FeatureGateChangeStreams: true}}, util.CHANGESTREAM_DOCUMENTDB_IMAGE},
		{"changestream disabled falls through to default", dbpreview.DocumentDBSpec{FeatureGates: map[string]bool{dbpreview.FeatureGateChangeStreams: false}}, util.DEFAULT_DOCUMENTDB_IMAGE},
		{"default when no overrides", dbpreview.DocumentDBSpec{}, util.DEFAULT_DOCUMENTDB_IMAGE},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := a.ExtensionImage(&dbpreview.DocumentDB{Spec: tt.spec}); got != tt.want {
				t.Errorf("ExtensionImage() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestDocumentDBAdapterGatewayImage covers the gateway-image resolution priority.
func TestDocumentDBAdapterGatewayImage(t *testing.T) {
	a := DocumentDBAdapter{}
	tests := []struct {
		name string
		spec dbpreview.DocumentDBSpec
		want string
	}{
		{"default when no overrides", dbpreview.DocumentDBSpec{}, util.DEFAULT_GATEWAY_IMAGE},
		{"explicit image takes precedence", dbpreview.DocumentDBSpec{
			Image:        &dbpreview.ImageSpec{Gateway: "custom-registry/custom-gateway:v1"},
			FeatureGates: map[string]bool{dbpreview.FeatureGateChangeStreams: true},
		}, "custom-registry/custom-gateway:v1"},
		{"documentDBVersion resolves image", dbpreview.DocumentDBSpec{DocumentDBVersion: "1.2.3"}, util.GATEWAY_IMAGE_REPO + ":1.2.3"},
		{"explicit overrides documentDBVersion", dbpreview.DocumentDBSpec{
			Image:             &dbpreview.ImageSpec{Gateway: "custom-registry/custom-gateway:v1"},
			DocumentDBVersion: "1.2.3",
		}, "custom-registry/custom-gateway:v1"},
		{"changestream enabled", dbpreview.DocumentDBSpec{FeatureGates: map[string]bool{dbpreview.FeatureGateChangeStreams: true}}, util.CHANGESTREAM_GATEWAY_IMAGE},
		{"changestream disabled", dbpreview.DocumentDBSpec{FeatureGates: map[string]bool{dbpreview.FeatureGateChangeStreams: false}}, util.DEFAULT_GATEWAY_IMAGE},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := a.GatewayImage(&dbpreview.DocumentDB{Spec: tt.spec}); got != tt.want {
				t.Errorf("GatewayImage() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestDocumentDBAdapterImageResolutionEnvVar confirms the DOCUMENTDB_VERSION env
// fallback flows through the profile-driven resolver.
func TestDocumentDBAdapterImageResolutionEnvVar(t *testing.T) {
	t.Setenv(util.DOCUMENTDB_VERSION_ENV, "0.200.0")
	a := DocumentDBAdapter{}
	db := &dbpreview.DocumentDB{Spec: dbpreview.DocumentDBSpec{}}

	if got, want := a.ExtensionImage(db), util.DOCUMENTDB_EXTENSION_IMAGE_REPO+":0.200.0"; got != want {
		t.Errorf("ExtensionImage() = %q, want %q", got, want)
	}
	if got, want := a.GatewayImage(db), util.GATEWAY_IMAGE_REPO+":0.200.0"; got != want {
		t.Errorf("GatewayImage() = %q, want %q", got, want)
	}
}
