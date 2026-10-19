# Product-neutral operator core

## Summary

The operator manages a PostgreSQL extension plus a protocol gateway on top of
[CloudNative-PG](https://cloudnative-pg.io/) (CNPG). DocumentDB is one such
product: the `documentdb` extension and the DocumentDB gateway. Other products
follow the same pattern with a different extension and gateway.

This design separates the CNPG orchestration (rendering the `Cluster`,
syncing it, PostgreSQL parameters, the resource split, and the extension
lifecycle) from the DocumentDB product, so a second operator can reuse the same
code. DocumentDB behavior is unchanged: golden snapshots of the rendered
`Cluster` are byte-identical before and after the refactor.

## Layers

```
 product CR (DocumentDB, ...)
        │  Adapter.ToClusterIntent
        ▼
 ClusterIntent + ProductProfile          internal/product   (public: pkg/core)
        │
        ▼
 renderer · sync · params · split ·      internal/cnpg      (public: pkg/core)
 extension lifecycle SQL
        │
        ▼
 CNPG Cluster ──► sidecar-injector plugin (gateway contract via plugin params)
```

- **`ProductProfile`** (`internal/product/profile.go`) holds what is fixed for
  a product: name, image repositories and default tag, default credential
  secret, CNPG plugin names, the PostgreSQL contract (`PostgresProfile`), and
  the gateway contract (`GatewayProfile`).
- **`PostgresProfile`** holds the extension (`ExtensionSpec`: name and
  ImageVolume library/control/ld paths), the extra
  `shared_preload_libraries`, the bootstrap SQL, `pg_hba`, product-protected
  GUCs, and the role granted to `streaming_replica`.
- **`GatewayProfile`** holds the sidecar container name, port, args, and the
  credential env var names. Empty fields keep the DocumentDB gateway defaults.
- **`ClusterIntent`** (`internal/product/intent.go`) is the per-instance
  desired state: images, topology, storage, resources, monitoring, TLS,
  identity (owner reference), PostgreSQL settings, feature gates, bootstrap,
  and the embedded `ProductProfile`.
- **`DocumentDBAdapter`** (`internal/product/documentdb.go`) maps a `DocumentDB`
  custom resource to a `ClusterIntent`. It is the only place that reads the
  DocumentDB spec for rendering.

The renderer (`cnpg.GetCnpgClusterSpecFromIntent`) only reads the intent. The
old `GetCnpgClusterSpec`, `MergeParameters`, `ProtectedParameters`,
`ComputeResourceSplit`, and `ValidateResources` entry points remain as thin
DocumentDB wrappers.

## What moved where

| Concern | Product-neutral | DocumentDB value |
| --- | --- | --- |
| Extension mount and name | `PostgresProfile.Extension` | `documentdb` |
| `shared_preload_libraries` | `PostgresProfile.AdditionalLibraries` | `pg_cron`, `pg_documentdb_core`, `pg_documentdb` |
| initdb SQL | `PostgresProfile.BootstrapSQL` | `CREATE EXTENSION documentdb CASCADE`, ... |
| Forced GUCs | core: `max_replication_slots`, `max_wal_senders`, `max_prepared_transactions`; product: `PostgresProfile.ProtectedParameters` | `cron.database_name` |
| Replication grant | `PostgresProfile.ReplicationAdminRole` (skipped when empty) | `documentdb_admin_role` |
| Extension upgrade SQL | `cnpg.ExtensionVersionSQL` / `ExtensionUpdateSQL` | name from the profile |
| Image sync | `SyncCnpgCluster` reads the extension by name | first `Extensions` entry |
| Resource validation path | `ValidateResourcesFor(..., base)` | `spec.resource` |
| Gateway sidecar | `GatewayProfile` → plugin params | injector defaults |

## Sidecar-injector gateway contract

The injector accepts five optional plugin parameters: `gatewayContainerName`,
`gatewayPort`, `gatewayArgs` (a JSON array that replaces the DocumentDB args
verbatim), `gatewayUsernameEnv`, and `gatewayPasswordEnv`. They default to the
DocumentDB gateway (`documentdb-gateway`, `10260`, `--create-user ...
--start-pg false --pg-port 5432`, `USERNAME`, `PASSWORD`). The operator sends
them only when `ProductProfile.Gateway` sets them, and the injector serializes
them only when they differ from the defaults, so existing clusters see no
parameter change and no rolling restart. See
[the plugin configuration guide](../developer-guides/sidecar-injector-plugin-configuration.md).

## Wire contracts kept on purpose

These names are visible to running clusters or users. Renaming them would
restart pods or break configuration, so they stay as they are:

- The plugin parameter key `documentDbCredentialSecret` carries the credential
  secret name for every product.
- Operator environment variables keep their `DOCUMENTDB_*` names (for example
  `DOCUMENTDB_GATEWAY_MEMORY_FRACTION` and `DOCUMENTDB_IOURING_SECCOMP_PROFILE`).
- The credential secret keys are `username` and `password`, and the gateway
  TLS secret is mounted at `/tls`.

## Using the core from another operator

Go does not allow importing `internal/` packages from another module, so
`operator/src/pkg/core` re-exports the seam: type aliases for the intent and
profile types plus `RenderCluster`, `SyncCluster`, `MergeParameters`,
`ProtectedParameters`, `ComputeResourceSplit`, `ValidateResources`,
`DefaultSplitConfig`, the extension SQL helpers, and `DocumentDBProfile` as a
reference profile. It adds no behavior of its own.

A second product:

1. Defines its own CRD and an adapter that returns a `core.ProductProfile` and
   builds a `core.ClusterIntent` from its custom resource.
2. Writes its own reconciler. It calls `core.RenderCluster` to build the
   desired `Cluster` and `core.SyncCluster` to apply changes, and runs
   `core.ExtensionUpdateSQL` / `core.ReplicationRoleGrantSQL` for the
   extension lifecycle.
3. Ships the shared sidecar-injector with its gateway image, setting
   `ProductProfile.Gateway` when its gateway contract differs.

`pkg/core/example_test.go` renders a non-DocumentDB cluster through the facade.
`internal/cnpg/second_product_test.go` renders a synthetic `pgcosmos` product
end to end and fails if any DocumentDB string other than the documented
`documentDbCredentialSecret` key appears in the output.

## Testing guarantees

- `internal/cnpg/cnpg_golden_test.go` pins the rendered `Cluster` for seven
  DocumentDB scenarios. Any change to DocumentDB output fails the test.
  Regenerate the snapshots with `UPDATE_GOLDEN=1 go test ./internal/cnpg` only
  for intentional changes.
- Unit tests cover the `...For` functions with a non-DocumentDB field path and
  extension name, sync of a `pgcosmos` extension, the gateway contract on both
  the operator and the injector side, and the facade.

## Not in scope / follow-ups

- `DocumentDBReconciler`, the webhook, backup CRDs, and the kubectl plugin stay
  DocumentDB-specific. A second product writes its own reconciler on top of
  `pkg/core`.
- The client `Service` port comes from the `GATEWAY_PORT` environment variable
  (default `10260`), not from `GatewayProfile.Port`.
- The OTel collector ConfigMap labels metrics with `documentdb.cluster`.
  Making the attribute name a profile field is a follow-up.
- `MonitoringSpec` and `CertificatesConfiguration` are reused from the
  DocumentDB API and CNPG respectively as shared types.
