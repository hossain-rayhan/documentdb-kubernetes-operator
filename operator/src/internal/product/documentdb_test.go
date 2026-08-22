// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package product

import (
	"testing"

	dbpreview "github.com/documentdb/documentdb-operator/api/preview"
	util "github.com/documentdb/documentdb-operator/internal/utils"
)

// TestDocumentDBAdapterImageResolutionMatchesUtil guards profile-driven image
// resolution against drift from the canonical util resolvers. Both paths must
// return identical images for every scenario so wiring the reconciler through
// the adapter never changes behavior.
func TestDocumentDBAdapterImageResolutionMatchesUtil(t *testing.T) {
	a := DocumentDBAdapter{}

	cases := []struct {
		name string
		spec dbpreview.DocumentDBSpec
	}{
		{"defaults", dbpreview.DocumentDBSpec{}},
		{"explicit images", dbpreview.DocumentDBSpec{
			Image: &dbpreview.ImageSpec{
				DocumentDB: "custom-registry/ext:v1",
				Gateway:    "custom-registry/gw:v1",
			},
		}},
		{"spec version", dbpreview.DocumentDBSpec{DocumentDBVersion: "1.2.3"}},
		{"changestream enabled", dbpreview.DocumentDBSpec{
			FeatureGates: map[string]bool{dbpreview.FeatureGateChangeStreams: true},
		}},
		{"changestream disabled", dbpreview.DocumentDBSpec{
			FeatureGates: map[string]bool{dbpreview.FeatureGateChangeStreams: false},
		}},
		{"explicit overrides version and gate", dbpreview.DocumentDBSpec{
			Image:             &dbpreview.ImageSpec{DocumentDB: "r/ext:x", Gateway: "r/gw:x"},
			DocumentDBVersion: "9.9.9",
			FeatureGates:      map[string]bool{dbpreview.FeatureGateChangeStreams: true},
		}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			db := &dbpreview.DocumentDB{Spec: c.spec}

			if got, want := a.ExtensionImage(db), util.GetDocumentDBImageForInstance(db); got != want {
				t.Errorf("ExtensionImage() = %q, util = %q", got, want)
			}
			if got, want := a.GatewayImage(db), util.GetGatewayImageForDocumentDB(db); got != want {
				t.Errorf("GatewayImage() = %q, util = %q", got, want)
			}
		})
	}
}

// TestDocumentDBAdapterImageResolutionEnvVar confirms the DOCUMENTDB_VERSION env
// fallback flows through the profile-driven resolver identically.
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
