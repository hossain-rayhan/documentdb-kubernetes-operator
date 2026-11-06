---
title: Deploy on Azure Local
description: Install the DocumentDB Kubernetes Operator on AKS enabled by Azure Arc on Azure Local and deploy a DocumentDB cluster.
tags:
  - azure-local
  - aks-arc
  - azure
  - deployment
---

# Deploy on Azure Local

Run DocumentDB on Kubernetes in your own datacenter or edge location with
[AKS enabled by Azure Arc on Azure Local](https://learn.microsoft.com/azure/aks-hybrid-edge/aks-overview).
Your data stays on your infrastructure, and you manage the Kubernetes cluster through Azure Arc.

This guide installs the operator and deploys a small DocumentDB cluster. It takes about 15 minutes on a cluster that's already running.

## When to use DocumentDB on Azure Local

Consider DocumentDB on Azure Local when you need to:

- keep data on-premises for data residency or compliance requirements;
- run MongoDB-compatible applications close to users, devices, or factory locations for low latency;
- use the same Kubernetes-based tooling for databases in Azure, other clouds, and on-premises.

## How it works

1. **Azure Local and an AKS cluster.** You deploy Azure Local and create an AKS cluster on it by following the Microsoft documentation. AKS clusters on Azure Local are connected to Azure Arc by default.
2. **Connect to the cluster** through Azure Arc cluster connect.
3. **Install cert-manager and the DocumentDB operator** with Helm.
4. **Deploy a DocumentDB cluster** with a `DocumentDB` resource.
5. **Connect your applications** with any MongoDB driver or tool.

## Before you begin

### A Kubernetes cluster on Azure Local

This guide assumes you already have an AKS cluster on Azure Local that's connected to Azure Arc. If you don't, create one first:

- [What is AKS enabled by Azure Arc?](https://learn.microsoft.com/azure/aks-hybrid-edge/aks-overview): overview and prerequisites
- [Create Kubernetes clusters using the Azure CLI](https://learn.microsoft.com/azure/aks-hybrid-edge/local/aks-create-clusters-cli)
- [Create Kubernetes clusters using the Azure portal](https://learn.microsoft.com/azure/aks-hybrid-edge/local/aks-create-clusters-portal)

The cluster needs:

| Requirement | Why |
|---|---|
| Kubernetes 1.32 or later | Minimum version supported by the operator |
| At least one Linux worker node with 4 vCPUs and 16 GiB of memory; three nodes for high availability | Runs the database pods |
| A default StorageClass (AKS on Azure Local provides `default`) | Stores database data |
| Outbound HTTPS access from the nodes to `ghcr.io` and `quay.io` | Pulls the operator, DocumentDB, and cert-manager images |
| An account with cluster-admin rights | The operator installs cluster-wide resources such as CRDs and webhooks |

### Tools on your machine

| Tool | Version | Purpose |
|---|---|---|
| [Azure CLI](https://learn.microsoft.com/cli/azure/install-azure-cli) with the `connectedk8s` extension | 2.70 or later | Connects to the cluster through Azure Arc |
| [kubectl](https://kubernetes.io/docs/tasks/tools/) | Within one minor version of your cluster | Kubernetes CLI |
| [Helm](https://helm.sh/docs/intro/install/) | 3.x | Installs cert-manager and the operator |
| [mongosh](https://www.mongodb.com/docs/mongodb-shell/install/) (optional) | Latest | Connects to DocumentDB from your machine |

On Windows, run the commands in this guide in [WSL](https://learn.microsoft.com/windows/wsl/install).

## Step 1: Connect to the cluster

AKS clusters on Azure Local usually have a private API server, so connect through
[Azure Arc cluster connect](https://learn.microsoft.com/azure/azure-arc/kubernetes/cluster-connect).

```bash
az extension add --name connectedk8s --upgrade --yes
az login
az account set --subscription <subscription-id>
```

In a separate terminal, start the proxy and leave it running:

```bash
az connectedk8s proxy --name <cluster-name> --resource-group <resource-group>
```

The proxy adds the cluster to your default kubeconfig (`~/.kube/config`) and makes it the current context. The entry stays after you stop the proxy; switch back to another context with `kubectl config use-context <name>`. In your first terminal, check that you can reach the cluster:

```bash
kubectl get nodes
```

```text
NAME              STATUS   ROLES           AGE   VERSION
moc-l03ih2sr4pp   Ready    <none>          2d    v1.33.7
moc-la9br28uu7w   Ready    <none>          2d    v1.33.7
moc-li06giu86m6   Ready    <none>          2d    v1.33.7
moc-lqokfineo3i   Ready    control-plane   2d    v1.33.7
```

!!! tip "If the proxy stops working"
    Restart the `az connectedk8s proxy` command if `kubectl` reports errors such as `Unknown proxy error occurred`; the connection can drop on long sessions. Run only one proxy at a time. After stopping the proxy with Ctrl+C, wait about 30 seconds before starting it again: its helper process takes a few seconds to exit. If a new proxy fails with `Failed to fetch public key info from client proxy`, a helper process from an earlier proxy is still running: stop it with `kill $(pgrep -f 'clientproxy/arcProxy')` and start the proxy again.

## Step 2: Install cert-manager

The operator uses [cert-manager](https://cert-manager.io/) to manage TLS certificates. Check whether cert-manager is already running:

```bash
kubectl get deployment cert-manager -n cert-manager
```

If it shows `READY 1/1`, skip the installation below. Don't check for the cert-manager CRDs instead: they remain after cert-manager is uninstalled, and the operator installation fails (`context deadline exceeded`) when the CRDs exist but cert-manager isn't running. If the command reports `NotFound`, install cert-manager:

```bash
helm repo add jetstack https://charts.jetstack.io --force-update
helm install cert-manager jetstack/cert-manager \
  --version v1.21.3 \
  --namespace cert-manager \
  --create-namespace \
  --set crds.enabled=true \
  --wait
```

Check that the `cert-manager`, `cert-manager-cainjector`, and `cert-manager-webhook` pods are `Running`:

```bash
kubectl get pods -n cert-manager
```

## Step 3: Install the DocumentDB operator

The operator Helm chart is public, so you don't need to sign in to a registry. It also installs the [CloudNativePG operator](https://cloudnative-pg.io/).

```bash
helm install documentdb-operator oci://ghcr.io/documentdb/documentdb-operator \
  --version <release-version> \
  --namespace documentdb-operator \
  --create-namespace \
  --wait
```

Check that the operator is ready. The DocumentDB operator runs in `documentdb-operator`; the CloudNativePG operator and the sidecar injector run in `cnpg-system`:

```bash
kubectl get deployment -n documentdb-operator
kubectl get deployment -n cnpg-system
```

```text
NAME                  READY   UP-TO-DATE   AVAILABLE   AGE
documentdb-operator   1/1     1            1           60s
NAME                                 READY   UP-TO-DATE   AVAILABLE   AGE
documentdb-operator-cloudnative-pg   1/1     1            1           60s
sidecar-injector                     1/1     1            1           60s
```

The `documentdb-operator` pod usually shows `RESTARTS 1` right after installation. It starts before cert-manager has issued its webhook certificate, exits, and starts cleanly once the certificate exists. This is expected.

## Step 4: Deploy a DocumentDB cluster

Create a namespace and a Secret with the credentials for the DocumentDB user. Replace the example password.

```bash
kubectl create namespace documentdb-ns
kubectl create secret generic documentdb-credentials \
  --namespace documentdb-ns \
  --from-literal=username=docdb_user \
  --from-literal=password='<your-password>'
```

Create the DocumentDB cluster:

```bash
cat <<EOF | kubectl apply -f -
apiVersion: documentdb.io/preview
kind: DocumentDB
metadata:
  name: my-documentdb
  namespace: documentdb-ns
spec:
  nodeCount: 1
  instancesPerNode: 1
  documentDbCredentialSecret: documentdb-credentials
  resource:
    storage:
      pvcSize: 10Gi
  exposeViaService:
    serviceType: ClusterIP
EOF
```

Wait until the cluster is healthy. This usually takes 2–5 minutes; the first deployment on a node takes longer because it downloads the DocumentDB image (about 700 MB).

```bash
until kubectl get documentdb my-documentdb -n documentdb-ns -o jsonpath='{.status.status}' | grep -q 'Cluster in healthy state'; do sleep 10; done
kubectl get pods -n documentdb-ns
```

```text
NAME              READY   STATUS    RESTARTS   AGE
my-documentdb-1   2/2     Running   0          3m
```

Each pod runs PostgreSQL with the DocumentDB extension, and the DocumentDB gateway, which serves the MongoDB wire protocol on port 10260.

!!! note "Combined image mode"
    AKS on Azure Local nodes run containerd 1.7, which doesn't support the Kubernetes ImageVolume feature. The operator detects this and uses a **combined image** that ships PostgreSQL and the DocumentDB extension together. You don't need to configure anything. To confirm the mode:

    ```bash
    kubectl get clusters.postgresql.cnpg.io my-documentdb -n documentdb-ns \
      -o jsonpath='{.metadata.annotations.documentdb\.io/image-mode}{"\n"}'
    ```

    The output is `combined`. For details, including how to mirror the image to a private registry, see [Before you start](before-you-start.md#required-components).

## Step 5: Connect to DocumentDB

Forward the DocumentDB service to your machine. Leave this command running:

```bash
kubectl port-forward service/documentdb-service-my-documentdb 10260:10260 -n documentdb-ns
```

In another terminal, connect with `mongosh`, using the username and password from Step 4:

```bash
mongosh "mongodb://docdb_user:<your-password>@127.0.0.1:10260/?directConnection=true&authMechanism=SCRAM-SHA-256&tls=true&tlsAllowInvalidCertificates=true"
```

Insert and query a document:

```javascript
use testdb
db.users.insertOne({ name: "Alice", role: "admin" })
db.users.find()
```

`tlsAllowInvalidCertificates=true` is needed because the gateway uses a self-signed certificate by default. For production, configure a trusted certificate; see [TLS configuration](../configuration/tls.md). If your password contains special characters such as `@`, `:` or `/`, URL-encode them in the connection string.

Applications that run in the same Kubernetes cluster connect to `documentdb-service-my-documentdb.documentdb-ns.svc.cluster.local:10260` instead. For drivers and more options, see [Connecting to DocumentDB](connecting-to-documentdb.md).

## Azure Local considerations

### High availability

To run a primary and two replicas on different nodes, set `instancesPerNode` to `3`:

```bash
kubectl patch documentdb my-documentdb -n documentdb-ns --type merge -p '{"spec":{"instancesPerNode":3}}'
```

If the primary fails, the operator promotes a replica, usually within seconds. When you drain a node for maintenance, the primary is switched over to another node in a controlled way. See [Local high availability](../high-availability/local-ha.md).

### Access from outside the cluster

AKS on Azure Local doesn't include a load balancer by default, so `LoadBalancer` services get no external IP. To expose DocumentDB outside the cluster, first deploy a load balancer such as [MetalLB for AKS on Azure Local](https://learn.microsoft.com/azure/aks-hybrid-edge/local/hyperconverged/deploy-load-balancer-cli), then set `spec.exposeViaService.serviceType` to `LoadBalancer`. See [Networking](../configuration/networking.md).

### Storage and backups

DocumentDB uses the cluster's default StorageClass unless you set `spec.resource.storage.storageClass`; see [Storage](../configuration/storage.md).

The AKS on Azure Local disk driver doesn't support volume snapshots, so `Backup` and `ScheduledBackup` resources and restoring from snapshots aren't available. Use logical backups instead, for example with `mongodump` and `mongorestore` through the port-forward from Step 5:

```bash
mongodump --uri="mongodb://docdb_user:<your-password>@127.0.0.1:10260/?directConnection=true&authMechanism=SCRAM-SHA-256&tls=true" \
  --tlsInsecure --gzip --archive=documentdb-backup.gz
```

`--tlsInsecure` accepts the default self-signed certificate; `mongodump` and `mongorestore` ignore `tlsAllowInvalidCertificates` in the connection string. Restore with the same options: `mongorestore --uri="…" --tlsInsecure --gzip --archive=documentdb-backup.gz`.

### Images and private registries

The nodes pull images from `ghcr.io` and `quay.io`. If your nodes can't reach the internet, mirror the images to a registry they can reach and set the registry in the operator's Helm values. See [Before you start](before-you-start.md#required-components).

## Clean up

Delete the DocumentDB cluster and its namespace:

```bash
kubectl delete documentdb my-documentdb -n documentdb-ns
kubectl delete namespace documentdb-ns
```

By default, DocumentDB keeps the data volume after the cluster is deleted (`persistentVolumeReclaimPolicy: Retain`). To delete the volume and its disk on Azure Local, switch the released volume to `Delete`:

```bash
kubectl get pv | grep 'documentdb-ns/' | awk '$5=="Released"{print $1}' \
  | xargs -r -I{} kubectl patch pv {} -p '{"spec":{"persistentVolumeReclaimPolicy":"Delete"}}'
```

The disk driver deletes the volumes within a minute or two; `kubectl get pv | grep 'documentdb-ns/'` then returns nothing. Don't use `kubectl delete pv` for this: with `Retain`, it removes only the Kubernetes object and leaves the disk on Azure Local.

To remove the operator and cert-manager, if nothing else on the cluster uses them:

```bash
helm uninstall documentdb-operator -n documentdb-operator
helm uninstall cert-manager -n cert-manager
```

Helm leaves the `documentdb-operator` and `cert-manager` namespaces and the DocumentDB, CloudNativePG, and cert-manager CRDs in place. To remove them as well, first check with the cluster owner that no other workloads use DocumentDB, CloudNativePG, or cert-manager: deleting a CRD deletes every resource of that type in the cluster.

```bash
kubectl delete namespace documentdb-operator cert-manager
kubectl get crd -o name | grep -E 'documentdb\.io|cnpg\.io|cert-manager\.io' | xargs -r kubectl delete
```

Stop the Arc proxy with Ctrl+C when you're done.

## Next steps

- [Connecting to DocumentDB](connecting-to-documentdb.md)
- [Local high availability](../high-availability/local-ha.md)
- [TLS configuration](../configuration/tls.md)
- [Upgrades](../operations/upgrades.md)
- [Azure Local documentation](https://learn.microsoft.com/azure/azure-local/overview)
