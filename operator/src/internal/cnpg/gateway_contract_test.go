// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cnpg

import (
	"testing"

	"github.com/go-logr/logr"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/documentdb/documentdb-operator/internal/product"
	util "github.com/documentdb/documentdb-operator/internal/utils"
)

func TestGatewayContractPluginParams(t *testing.T) {
	gatewayKeys := []string{
		util.PLUGIN_PARAM_GATEWAY_CONTAINER_NAME,
		util.PLUGIN_PARAM_GATEWAY_PORT,
		util.PLUGIN_PARAM_GATEWAY_ARGS,
		util.PLUGIN_PARAM_GATEWAY_USERNAME_ENV,
		util.PLUGIN_PARAM_GATEWAY_PASSWORD_ENV,
	}
	req := ctrl.Request{}
	req.Name, req.Namespace = "golden", "golden-ns"

	intent := product.DocumentDBAdapter{}.ToClusterIntent(goldenBaseDocumentDB())
	params := GetCnpgClusterSpecFromIntent(req, intent, "sa", "", true, logr.Discard()).Spec.Plugins[0].Parameters
	for _, key := range gatewayKeys {
		if _, ok := params[key]; ok {
			t.Errorf("DocumentDB profile emitted %q", key)
		}
	}

	intent.Product.Gateway = product.GatewayProfile{
		ContainerName: "pgcosmos-gateway",
		Port:          8081,
		Args:          []string{"--port", "8081"},
		UsernameEnv:   "COSMOS_USER",
		PasswordEnv:   "COSMOS_KEY",
	}
	params = GetCnpgClusterSpecFromIntent(req, intent, "sa", "", true, logr.Discard()).Spec.Plugins[0].Parameters
	want := map[string]string{
		util.PLUGIN_PARAM_GATEWAY_CONTAINER_NAME: "pgcosmos-gateway",
		util.PLUGIN_PARAM_GATEWAY_PORT:           "8081",
		util.PLUGIN_PARAM_GATEWAY_ARGS:           `["--port","8081"]`,
		util.PLUGIN_PARAM_GATEWAY_USERNAME_ENV:   "COSMOS_USER",
		util.PLUGIN_PARAM_GATEWAY_PASSWORD_ENV:   "COSMOS_KEY",
	}
	for k, v := range want {
		if params[k] != v {
			t.Errorf("%s = %q, want %q", k, params[k], v)
		}
	}
}
