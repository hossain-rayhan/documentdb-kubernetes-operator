// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package product

import (
	"testing"

	util "github.com/documentdb/documentdb-operator/internal/utils"
)

// TestDocumentDBProfilePinsConstants guards the DocumentDB profile against
// accidental drift from the canonical operator constants. If a default image,
// credential secret, or plugin name changes, the mapping must be updated here
// deliberately rather than silently diverging.
func TestDocumentDBProfilePinsConstants(t *testing.T) {
	p := DocumentDBProfile()

	cases := []struct {
		name string
		got  string
		want string
	}{
		{"Name", p.Name, "DocumentDB"},
		{"ExtensionImageRepo", p.ExtensionImageRepo, util.DOCUMENTDB_EXTENSION_IMAGE_REPO},
		{"GatewayImageRepo", p.GatewayImageRepo, util.GATEWAY_IMAGE_REPO},
		{"DefaultExtensionImage", p.DefaultExtensionImage, util.DEFAULT_DOCUMENTDB_IMAGE},
		{"DefaultGatewayImage", p.DefaultGatewayImage, util.DEFAULT_GATEWAY_IMAGE},
		{"DefaultCredentialSecret", p.DefaultCredentialSecret, util.DEFAULT_DOCUMENTDB_CREDENTIALS_SECRET},
		{"SidecarInjectorPlugin", p.SidecarInjectorPlugin, util.DEFAULT_SIDECAR_INJECTOR_PLUGIN},
		{"WALReplicaPlugin", p.WALReplicaPlugin, util.DEFAULT_WAL_REPLICA_PLUGIN},
	}

	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("DocumentDBProfile().%s = %q, want %q", c.name, c.got, c.want)
		}
	}
}

// TestDocumentDBAdapterSatisfiesSeam verifies the DocumentDB adapter is usable
// through the product-neutral Adapter interface.
func TestDocumentDBAdapterSatisfiesSeam(t *testing.T) {
	var a Adapter = DocumentDBAdapter{}
	if got := a.Profile().Name; got != "DocumentDB" {
		t.Errorf("Adapter.Profile().Name = %q, want %q", got, "DocumentDB")
	}
}
