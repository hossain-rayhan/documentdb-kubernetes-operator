// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package engine

import (
	"context"
	"slices"
	"strings"
	"testing"

	cmapi "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	cnpgv1 "github.com/cloudnative-pg/cloudnative-pg/api/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	dbpreview "github.com/documentdb/documentdb-operator/api/preview"
	"github.com/documentdb/documentdb-operator/internal/product"
)

// sharedPrefix makes "documentdb-service-"+name exceed 63 characters only in
// the trailing "-east"/"-west", which plain truncation used to discard.
var sharedPrefix = strings.Repeat("a", 44)

func longCluster(suffix string, uid types.UID) *dbpreview.DocumentDB {
	db := reconcilableCluster()
	db.Name = sharedPrefix + suffix
	db.UID = uid
	db.Spec.ExposeViaService.ServiceType = "ClusterIP"
	return db
}

func ownedService(name, ip string, owner *dbpreview.DocumentDB) *corev1.Service {
	svc := gatewayService(ip)
	svc.Name = name
	svc.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: "documentdb.io/preview", Kind: "DocumentDB", Name: owner.Name, UID: owner.UID,
		Controller: &[]bool{true}[0],
	}}
	return svc
}

func TestGatewayServiceNameKeepsOnlyOwnLegacyService(t *testing.T) {
	naming := product.DocumentDBProfile().Naming
	east, west := longCluster("-east", "east-uid"), longCluster("-west", "west-uid")
	legacy, ok := naming.LegacyServiceName(east.Name)
	if !ok || legacy != naming.ServicePrefix+sharedPrefix {
		t.Fatalf("unexpected legacy name %q, %v", legacy, ok)
	}

	s := newTestScheme(t)
	ctx := context.Background()
	c := newFakeClient(t, s, nil, ownedService(legacy, "10.0.0.1", east))
	for _, tt := range []struct {
		name string
		obj  *dbpreview.DocumentDB
		want string
	}{
		{name: "short name needs no lookup", obj: testCluster(), want: naming.ServicePrefix + "db"},
		{name: "owner of the legacy Service keeps it", obj: east, want: legacy},
		{name: "other cluster gets its hashed name", obj: west, want: naming.ServiceName(west.Name)},
		{name: "no legacy Service uses the hashed name", obj: longCluster("-north", "north-uid"), want: naming.ServiceName(sharedPrefix + "-north")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := gatewayServiceName(ctx, c, naming, tt.obj)
			if err != nil || got != tt.want {
				t.Fatalf("gatewayServiceName = %q, %v; want %q", got, err, tt.want)
			}
		})
	}

	failing := newFakeClient(t, s, &interceptor.Funcs{Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
		return errInjected
	}})
	if _, err := gatewayServiceName(ctx, failing, naming, west); err != errInjected {
		t.Fatalf("expected lookup error, got %v", err)
	}
}

// TestClusterReconcileLongNamesDoNotShareService reproduces two clusters whose
// names differ only past the 63-character cut: each must publish its own
// Service endpoint, and the pre-existing cluster keeps its legacy Service.
func TestClusterReconcileLongNamesDoNotShareService(t *testing.T) {
	naming := product.DocumentDBProfile().Naming
	east, west := longCluster("-east", "east-uid"), longCluster("-west", "west-uid")
	legacy, _ := naming.LegacyServiceName(east.Name)
	westService := naming.ServiceName(west.Name)
	if westService == legacy || westService == naming.ServiceName(east.Name) {
		t.Fatalf("expected a distinct Service name for %q, got %q", west.Name, westService)
	}

	s := newTestScheme(t)
	ctx := context.Background()
	c := newFakeClient(t, s, nil, east, west,
		ownedService(legacy, "10.0.0.1", east), ownedService(westService, "10.0.0.2", west))
	r, _ := newClusterReconciler(c, s, &sqlRecorder{roleOut: "(1 row)"})

	for _, tt := range []struct {
		db *dbpreview.DocumentDB
		ip string
	}{{db: west, ip: "10.0.0.2"}, {db: east, ip: "10.0.0.1"}} {
		key := ctrl.Request{NamespacedName: types.NamespacedName{Name: tt.db.Name, Namespace: testNamespace}}
		if _, err := r.Reconcile(ctx, key); err != nil {
			t.Fatalf("%s: create reconcile: %v", tt.db.Name, err)
		}
		cluster := &cnpgv1.Cluster{}
		if err := c.Get(ctx, key.NamespacedName, cluster); err != nil {
			t.Fatal(err)
		}
		cluster.Status = cnpgv1.ClusterStatus{
			Phase:           CNPGClusterHealthyPhase,
			CurrentPrimary:  tt.db.Name + "-1",
			InstancesStatus: map[cnpgv1.PodStatus][]string{cnpgv1.PodHealthy: {tt.db.Name + "-1"}},
		}
		if err := c.Update(ctx, cluster); err != nil {
			t.Fatal(err)
		}
		if _, err := r.Reconcile(ctx, key); err != nil {
			t.Fatalf("%s: converge reconcile: %v", tt.db.Name, err)
		}
		got := &dbpreview.DocumentDB{}
		if err := c.Get(ctx, key.NamespacedName, got); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(got.Status.ConnectionString, tt.ip) {
			t.Fatalf("%s published %q, want its own Service IP %s", tt.db.Name, got.Status.ConnectionString, tt.ip)
		}
	}

	if err := c.Get(ctx, types.NamespacedName{Name: naming.ServiceName(east.Name), Namespace: testNamespace}, &corev1.Service{}); !apierrors.IsNotFound(err) {
		t.Fatalf("existing cluster must keep its legacy Service instead of creating a hashed one, got %v", err)
	}
}

func TestCertificateNamesFollowResolvedService(t *testing.T) {
	naming := product.DocumentDBProfile().Naming
	east, west := longCluster("-east", "east-uid"), longCluster("-west", "west-uid")
	legacy, _ := naming.LegacyServiceName(east.Name)

	for _, tt := range []struct {
		db   *dbpreview.DocumentDB
		want string
	}{{db: east, want: legacy}, {db: west, want: naming.ServiceName(west.Name)}} {
		t.Run(tt.db.Name, func(t *testing.T) {
			s := newTestScheme(t)
			c := newFakeClient(t, s, nil, tt.db, ownedService(legacy, "10.0.0.1", east))
			key := ctrl.Request{NamespacedName: types.NamespacedName{Name: tt.db.Name, Namespace: testNamespace}}
			if _, err := newCertReconciler(c, s).Reconcile(context.Background(), key); err != nil {
				t.Fatal(err)
			}
			cert := &cmapi.Certificate{}
			if err := c.Get(context.Background(), types.NamespacedName{Name: tt.db.Name + "-gateway-cert", Namespace: testNamespace}, cert); err != nil {
				t.Fatal(err)
			}
			want := []string{tt.want, tt.want + "." + testNamespace, tt.want + "." + testNamespace + ".svc"}
			if !slices.Equal(cert.Spec.DNSNames, want) {
				t.Fatalf("DNSNames = %v, want %v", cert.Spec.DNSNames, want)
			}
		})
	}
}
