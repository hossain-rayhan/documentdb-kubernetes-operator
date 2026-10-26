// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package engine

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/documentdb/documentdb-operator/internal/product"
)

// gatewayServiceName returns the gateway Service name for obj. A cluster whose
// long name an earlier release plainly truncated keeps that existing Service,
// and with it its LoadBalancer endpoint and certificate names, but only when
// the Service is controlled by obj; any other cluster uses the hashed name.
func gatewayServiceName(ctx context.Context, c client.Reader, naming product.NamingProfile, obj metav1.Object) (string, error) {
	name := naming.ServiceName(obj.GetName())
	legacy, ok := naming.LegacyServiceName(obj.GetName())
	if !ok {
		return name, nil
	}
	svc := &corev1.Service{}
	if err := c.Get(ctx, types.NamespacedName{Name: legacy, Namespace: obj.GetNamespace()}, svc); err != nil {
		if apierrors.IsNotFound(err) {
			return name, nil
		}
		return "", err
	}
	if owner := metav1.GetControllerOf(svc); owner != nil && owner.UID == obj.GetUID() {
		return legacy, nil
	}
	return name, nil
}
