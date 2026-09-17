// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package operations

import (
	"fmt"

	"k8s.io/client-go/kubernetes"

	"github.com/documentdb/documentdb-operator/test/longhaul/config"
	"github.com/documentdb/documentdb-operator/test/longhaul/journal"
	"github.com/documentdb/documentdb-operator/test/longhaul/monitor"
)

// Registry stores operations by their stable Name() values while preserving
// registration order for deterministic snapshots and random-mode summaries.
type Registry struct {
	order      []string
	operations map[string]Operation
}

// NewRegistry builds a validated operation registry.
func NewRegistry(ops ...Operation) (*Registry, error) {
	registry := &Registry{
		order:      make([]string, 0, len(ops)),
		operations: make(map[string]Operation, len(ops)),
	}
	for _, op := range ops {
		if op == nil {
			return nil, fmt.Errorf("operation registry contains a nil operation")
		}
		name := op.Name()
		if name == "" {
			return nil, fmt.Errorf("operation registry contains an operation with an empty name")
		}
		if _, exists := registry.operations[name]; exists {
			return nil, fmt.Errorf("operation registry contains duplicate name %q", name)
		}
		registry.order = append(registry.order, name)
		registry.operations[name] = op
	}
	return registry, nil
}

// NewDefaultRegistry centralizes construction of every supported operation.
func NewDefaultRegistry(
	cfg config.Config,
	clusterClient monitor.ClusterClient,
	clientset kubernetes.Interface,
	health *monitor.HealthMonitor,
	j *journal.Journal,
) (*Registry, error) {
	return NewRegistry(
		NewScaleUp(clusterClient, health, cfg.MaxInstances, cfg.RecoveryTimeout),
		NewScaleDown(clusterClient, health, cfg.MinInstances, cfg.RecoveryTimeout),
		NewUpgradeDocumentDB(clusterClient, clientset, health, j, cfg.Namespace, cfg.RecoveryTimeout),
		NewKillOperatorPod(clientset, cfg.OperatorNamespace, cfg.RecoveryTimeout),
		NewKillPrimaryPod(clusterClient, health, cfg.RecoveryTimeout),
	)
}

// All returns all registered operations in stable registration order.
func (r *Registry) All() []Operation {
	ops := make([]Operation, 0, len(r.order))
	for _, name := range r.order {
		ops = append(ops, r.operations[name])
	}
	return ops
}

// Resolve returns the named operations in exactly the requested order.
func (r *Registry) Resolve(names []string) ([]Operation, error) {
	resolved := make([]Operation, 0, len(names))
	seen := make(map[string]struct{}, len(names))
	for _, name := range names {
		if _, duplicate := seen[name]; duplicate {
			return nil, fmt.Errorf("operation sequence contains duplicate name %q", name)
		}
		op, ok := r.operations[name]
		if !ok {
			return nil, fmt.Errorf("operation sequence contains unknown name %q", name)
		}
		seen[name] = struct{}{}
		resolved = append(resolved, op)
	}
	return resolved, nil
}
