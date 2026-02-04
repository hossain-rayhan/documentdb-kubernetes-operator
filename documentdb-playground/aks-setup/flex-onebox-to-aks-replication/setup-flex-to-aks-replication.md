# Streaming Replication: Flex OneBox (Primary) → AKS (Secondary)

This guide documents setting up PostgreSQL streaming replication from a Flex OneBox VM (primary) to an AKS DocumentDB cluster (standby/read replica).

## Architecture Overview

```
┌─────────────────────┐         ┌─────────────────────────────┐
│   Flex OneBox VM    │         │        AKS Cluster          │
│     (Primary)       │         │        (Standby)            │
│                     │         │                             │
│  PostgreSQL 17      │ ──WAL──▶│  DocumentDB/CNPG Pod        │
│  + DocumentDB       │ Stream  │  + PostgreSQL 17            │
│                     │         │  + DocumentDB extensions    │
│  Port: 5432         │         │  + Gateway sidecar          │
└─────────────────────┘         └─────────────────────────────┘
```

## Quick Start

Use the automation script:

```bash
# Get AKS egress IP (needed for Flex pg_hba.conf)
NAMESPACE=documentdb-instance-ns \
POD_NAME=sample-documentdb-1 \
./setup-aks-replication.sh egress-ip

# Test connectivity to Flex
NAMESPACE=documentdb-instance-ns \
POD_NAME=sample-documentdb-1 \
FLEX_HOST=fc-xxx.eastus2.cloudapp.azure.com \
FLEX_USER=azuresu \
FLEX_PASSWORD='xxx' \
./setup-aks-replication.sh test-connection

# Setup as standby (WARNING: destroys AKS data)
NAMESPACE=documentdb-instance-ns \
POD_NAME=sample-documentdb-1 \
FLEX_HOST=fc-xxx.eastus2.cloudapp.azure.com \
FLEX_USER=azuresu \
FLEX_PASSWORD='xxx' \
SLOT_NAME=aks_slot \
./setup-aks-replication.sh setup-standby
```

---

## Prerequisites

### AKS Side
- AKS cluster with DocumentDB operator installed
- DocumentDB instance running (2/2 pods)
- `kubectl` configured to access the cluster

### Flex Side
- Flex OneBox VM with PostgreSQL 17 + DocumentDB
- SSH access to the VM
- PostgreSQL superuser access (`azuresu`)

---

## Manual Steps (Detailed)

### Step 1: Get AKS Egress IP

The AKS egress IP is needed for Flex's `pg_hba.conf` to allow replication connections.

```bash
# Using the setup script
./setup-aks-replication.sh egress-ip

# Or manually via Azure CLI
NODE_RG=$(az aks show --resource-group <rg> --name <cluster> --query "nodeResourceGroup" -o tsv)
az network public-ip list --resource-group $NODE_RG --query "[?contains(name, 'kubernetes') == \`false\`].ipAddress" -o tsv
```

Note the egress IP (e.g., `<your-egress-ip>`).

---

### Step 2: Configure Flex (Primary) - CRITICAL

SSH into the Flex VM and make these changes **before** running pg_basebackup:

#### 2.1 Comment out include_dir (CNPG compatibility)

The Flex postgresql.conf has an `include_dir` that doesn't exist in CNPG containers:

```bash
sudo sed -i "s|^include_dir='/datadrive/pg/data/conf.d'|# include_dir='/datadrive/pg/data/conf.d'|" /datadrive/pg/data/postgresql.conf

# Verify
grep -n "include_dir" /datadrive/pg/data/postgresql.conf
```

#### 2.2 Add unix_socket_directories (CNPG compatibility)

CNPG containers use `/controller/run` for sockets, not `/var/run/postgresql`:

```bash
echo "unix_socket_directories = '/controller/run'" | sudo tee -a /datadrive/pg/data/postgresql.conf

# Verify
tail -3 /datadrive/pg/data/postgresql.conf
```

#### 2.3 Configure pg_hba.conf for replication

```bash
# Replace <EGRESS_IP> with the IP from Step 1
echo "host replication azuresu <EGRESS_IP>/32 md5" | sudo tee -a /datadrive/pg/data/pg_hba.conf

# Example:
echo "host replication azuresu <EGRESS_IP>/32 md5" | sudo tee -a /datadrive/pg/data/pg_hba.conf
```

#### 2.4 Create replication slot

```bash
sudo -u postgres psql -c "SELECT pg_create_physical_replication_slot('aks_slot');"

# Verify
sudo -u postgres psql -c "SELECT slot_name, slot_type, active FROM pg_replication_slots;"
```

#### 2.5 Reload PostgreSQL

```bash
sudo -u postgres /usr/lib/postgresql/17/bin/pg_ctl reload -D /datadrive/pg/data
```

---

### Step 3: (Optional) Update Extension Version

**Problem**: Flex may have a different DocumentDB version than AKS. Streaming replication requires matching versions.

**Why control files can't be modified**: In CNPG pods, `/usr/share/postgresql/16/extension/*.control` files are root-owned, and containers run with `no new privileges` flag preventing sudo.

**Solution**: Update `pg_extension` catalog directly (no restart required):

```bash
# Update version
kubectl exec -it documentdb-preview-1 -n documentdb-preview-ns -- \
  psql -U postgres -c "UPDATE pg_extension SET extversion = '0.110-0' WHERE extname IN ('documentdb', 'documentdb_core');"

# Verify
kubectl exec -it documentdb-preview-1 -n documentdb-preview-ns -- \
  psql -U postgres -c "SELECT extname, extversion FROM pg_extension WHERE extname LIKE '%documentdb%';"
```

> ⚠️ This is a catalog-only change for testing. Binary `.so` files remain unchanged.

### Step 4: Set Up AKS as Standby (Standalone Pod Approach)

> **CRITICAL ISSUE**: The CNPG operator auto-promotes standby instances to primary! When CNPG detects a standby with `standby.signal`, it waits for WAL recovery to complete, then **promotes it to primary** because CNPG thinks it should manage the cluster leadership.
>
> **Solution**: Run a **standalone DocumentDB pod** that bypasses CNPG entirely. This gives us full control over the standby without CNPG interference.

#### Why CNPG Doesn't Work for External Replication

When we tried using the normal CNPG-managed pod:
1. CNPG detected standby mode
2. Waited for consistent recovery state
3. Immediately executed: `"Setting myself as primary"` → `"Promoting instance"`
4. This breaks replication because the standby is no longer following the external Flex primary

#### 4.1 First: Deploy a fresh DocumentDB instance and save its config

```bash
# Deploy fresh instance
./create-cluster.sh --deploy-instance

# Wait for pod to be running (2/2)
kubectl wait --for=condition=Ready pod/sample-documentdb-1 -n documentdb-instance-ns --timeout=300s

# Save the AKS postgresql.conf (has correct extension paths)
kubectl exec sample-documentdb-1 -n documentdb-instance-ns -c postgres -- \
  cat /var/lib/postgresql/data/pgdata/postgresql.conf > /tmp/aks-postgresql.conf

echo "[✓] Saved AKS config: $(wc -l < /tmp/aks-postgresql.conf) lines"
```

#### 4.2 Scale down ALL operators (prevent auto-recreation)

```bash
kubectl scale deployment documentdb-operator-cloudnative-pg -n cnpg-system --replicas=0
kubectl scale deployment sidecar-injector -n cnpg-system --replicas=0
kubectl scale deployment documentdb-operator -n documentdb-operator --replicas=0
```

#### 4.3 Delete the CNPG-managed pod and create PVC fixer

```bash
kubectl delete pod sample-documentdb-1 -n documentdb-instance-ns --force --grace-period=0

cat <<'EOF' | kubectl apply -f -
apiVersion: v1
kind: Pod
metadata:
  name: pvc-fixer
  namespace: documentdb-instance-ns
spec:
  containers:
  - name: postgres
    image: postgres:17
    command: ["sleep", "infinity"]
    volumeMounts:
    - name: pgdata
      mountPath: /pgdata
  volumes:
  - name: pgdata
    persistentVolumeClaim:
      claimName: sample-documentdb-1
  nodeSelector:
    kubernetes.io/hostname: <your-node-name>  # Must match the node where PVC is bound
EOF

kubectl wait --for=condition=Ready pod/pvc-fixer -n documentdb-instance-ns --timeout=120s
```

#### 4.4 Run pg_basebackup

```bash
kubectl exec pvc-fixer -n documentdb-instance-ns -- bash -c "
rm -rf /pgdata/pgdata/* /pgdata/pgdata/.[!.]* 2>/dev/null
PGPASSWORD='<FLEX_PASSWORD>' pg_basebackup \
  -h <FLEX_HOST> -p 5432 -U azuresu \
  -D /pgdata/pgdata -Fp -Xs -P -R -S aks_slot -v
"
```

#### 4.5 Restore AKS postgresql.conf and configure for standby

```bash
# Copy saved AKS config (has correct extension paths)
kubectl cp /tmp/aks-postgresql.conf documentdb-instance-ns/pvc-fixer:/pgdata/pgdata/postgresql.conf

# Create custom.conf (CRITICAL: archive_mode must be OFF!)
kubectl exec pvc-fixer -n documentdb-instance-ns -- bash -c "
cat > /pgdata/pgdata/custom.conf << 'CUSTOM'
archive_mode = 'off'
cluster_name = 'documentdb-standby'
cron.database_name = 'postgres'
dynamic_shared_memory_type = 'posix'
full_page_writes = 'on'
hot_standby = 'true'
listen_addresses = '*'
log_destination = 'csvlog'
log_directory = '/controller/log'
log_filename = 'postgres'
logging_collector = 'on'
max_parallel_workers = '32'
max_replication_slots = '10'
max_wal_senders = '10'
max_worker_processes = '32'
port = '5432'
restart_after_crash = 'false'
shared_memory_type = 'mmap'
shared_preload_libraries = 'pg_cron,pg_documentdb_core,pg_documentdb'
ssl = 'on'
ssl_ca_file = '/controller/certificates/client-ca.crt'
ssl_cert_file = '/controller/certificates/server.crt'
ssl_key_file = '/controller/certificates/server.key'
unix_socket_directories = '/controller/run'
wal_level = 'logical'
CUSTOM
"

# Create override.conf with Flex replication settings
kubectl exec pvc-fixer -n documentdb-instance-ns -- bash -c "
cat > /pgdata/pgdata/override.conf << 'OVERRIDE'
primary_conninfo = 'host=<FLEX_HOST> port=5432 user=azuresu password=<FLEX_PASSWORD> application_name=aks_standby sslmode=prefer'
primary_slot_name = 'aks_slot'
recovery_target_timeline = 'latest'
max_connections = 900
max_worker_processes = 25
OVERRIDE
"

# Ensure standby.signal exists and fix ownership
kubectl exec pvc-fixer -n documentdb-instance-ns -- bash -c "
touch /pgdata/pgdata/standby.signal
chown -R 105:108 /pgdata/pgdata
"
```

> **CRITICAL**: `archive_mode = 'off'` is essential! The AKS config has `restore_command` that calls `/controller/manager` which doesn't exist in our standalone pod. Disabling archive mode prevents the `FATAL: could not restore file from archive: command not found` error.

#### 4.6 Delete PVC fixer and create standalone DocumentDB pod

> **CRITICAL - Extension Library Naming Fix**: Flex uses extension names `documentdb_core` and `documentdb`, but AKS uses `pg_documentdb_core` and `pg_documentdb`. Without symlinks, you'll get `ERROR: could not access file "$libdir/documentdb_core": No such file or directory` when querying BSON data.
>
> The solution: Copy all libraries to an emptyDir volume, create symlinks, and mount it at `$libdir`.

```bash
kubectl delete pod pvc-fixer -n documentdb-instance-ns --force --grace-period=0

cat <<'EOF' | kubectl apply -f -
apiVersion: v1
kind: Pod
metadata:
  name: documentdb-standby
  namespace: documentdb-instance-ns
spec:
  securityContext:
    runAsUser: 0
  initContainers:
  - name: setup
    image: ghcr.io/microsoft/documentdb/documentdb-local:16
    securityContext:
      runAsUser: 0
    command: ["/bin/bash", "-c"]
    args:
    - |
      set -e
      # Create TLS certs
      mkdir -p /controller/run /controller/log /controller/certificates
      cd /controller/certificates
      openssl req -new -x509 -days 365 -nodes -out client-ca.crt -keyout client-ca.key -subj "/CN=ca" 2>/dev/null
      openssl req -new -nodes -out server.csr -keyout server.key -subj "/CN=localhost" 2>/dev/null
      openssl x509 -req -in server.csr -CA client-ca.crt -CAkey client-ca.key -CAcreateserial -out server.crt -days 365 2>/dev/null
      chmod 600 server.key client-ca.key
      chown -R 105:108 /controller
      
      # Copy ALL libraries from image to emptyDir to overlay $libdir
      echo "Copying libraries to overlay..."
      cp -a /usr/lib/postgresql/16/lib/* /pglib/
      
      # Create RELATIVE symlinks for Flex extension naming compatibility
      # Flex uses 'documentdb_core' but AKS has 'pg_documentdb_core'
      cd /pglib
      ln -sf pg_documentdb_core.so documentdb_core.so
      ln -sf pg_documentdb.so documentdb.so
      echo "[✓] Created extension symlinks:"
      ls -la /pglib/ | grep 'documentdb.*\.so'
      
      touch /var/lib/postgresql/data/pgdata/standby.signal
      chown -R 105:108 /var/lib/postgresql/data/pgdata
      echo "[✓] Setup complete"
    volumeMounts:
    - name: pgdata
      mountPath: /var/lib/postgresql/data
    - name: controller
      mountPath: /controller
    - name: pglib
      mountPath: /pglib
  containers:
  - name: postgres
    image: ghcr.io/microsoft/documentdb/documentdb-local:16
    securityContext:
      runAsUser: 105
      runAsGroup: 108
    command: ["postgres"]
    args: ["-D", "/var/lib/postgresql/data/pgdata", "-c", "logging_collector=off"]
    volumeMounts:
    - name: pgdata
      mountPath: /var/lib/postgresql/data
    - name: controller
      mountPath: /controller
    - name: pglib
      mountPath: /usr/lib/postgresql/16/lib   # Overlays $libdir with our symlinked copy
    ports:
    - containerPort: 5432
  volumes:
  - name: pgdata
    persistentVolumeClaim:
      claimName: sample-documentdb-1
  - name: controller
    emptyDir: {}
  - name: pglib
    emptyDir: {}   # Holds copied libraries + symlinks
  nodeSelector:
    kubernetes.io/hostname: <your-node-name>  # Must match the node where PVC is bound
EOF
```

> **Note**: We use `ghcr.io/microsoft/documentdb/documentdb-local:16` (the same image as CNPG uses) to ensure the DocumentDB extension is available for MongoDB query compatibility.
>
> The `pglib` emptyDir volume is critical: it copies all `.so` files from the image, adds the compatibility symlinks, then mounts over `/usr/lib/postgresql/16/lib` so PostgreSQL finds both `pg_documentdb_core.so` AND `documentdb_core.so`.

#### 4.7 Verify the pod starts and enters standby mode

```bash
# Wait for pod
sleep 20

# Check status
kubectl get pods -n documentdb-instance-ns

# Check logs for streaming
kubectl logs documentdb-standby -n documentdb-instance-ns 2>&1 | grep -iE "standby|streaming|recovery|ready"
```

Expected output:
```
LOG:  entering standby mode
LOG:  consistent recovery state reached at 0/B000028
LOG:  database system is ready to accept read-only connections
LOG:  started streaming WAL from primary at 0/B000000 on timeline 1
```

---

### Step 5: Verify Replication and Read Data

#### 5.1 Check pod status

```bash
kubectl get pods -n documentdb-instance-ns
# Should show 1/1 Running for documentdb-standby
```

#### 5.2 Check if running as standby

```bash
kubectl exec documentdb-standby -n documentdb-instance-ns -- \
  psql -h /controller/run -U azuresu -d postgres -c "SELECT pg_is_in_recovery() as is_standby;"

# Expected output:
#  is_standby 
# ------------
#  t
```

#### 5.3 Check streaming replication status

On the **Flex primary**:

```bash
sudo -u postgres psql -c "SELECT client_addr, state, sent_lsn, write_lsn, flush_lsn, replay_lsn FROM pg_stat_replication;"
```

On the **AKS standby**:

```bash
kubectl exec documentdb-standby -n documentdb-instance-ns -- \
  psql -h /controller/run -U azuresu -d postgres -c \
  "SELECT pg_is_in_recovery() as standby, pg_last_wal_receive_lsn() as wal_lsn, pg_last_xact_replay_timestamp() as last_replayed;"
```

#### 5.4 Check logs for streaming

```bash
kubectl logs documentdb-standby -n documentdb-instance-ns 2>&1 | grep -E "streaming|recovery|standby"
```

Expected log entries:
```
LOG:  entering standby mode
LOG:  consistent recovery state reached
LOG:  started streaming WAL from primary at ...
```

#### 5.5 Verify replicated MongoDB data is readable

After inserting MongoDB documents on Flex, verify they're replicated and readable on AKS:

```bash
# Check document count
kubectl exec documentdb-standby -n documentdb-instance-ns -- \
  psql -h /controller/run -U azuresu -d postgres -c \
  "SELECT count(*) FROM documentdb_data.documents_2;"

# Read documents as JSON (human-readable format!)
kubectl exec documentdb-standby -n documentdb-instance-ns -- \
  psql -h /controller/run -U azuresu -d postgres -c \
  "SELECT documentdb_core.bson_to_json_string(document) FROM documentdb_data.documents_2;"
```

Example output showing replicated MongoDB documents:
```
                                               bson_to_json_string                                                            
-------------------------------------------------------------------------------------------------------------------------------------------
 { "_id" : { "$oid" : "6982b4f65bc314981baade50" }, "name" : "Alice", "email" : "alice@example.com", "age" : { "$numberInt" : "30" } }
 { "_id" : { "$oid" : "6982b4f65bc314981baade51" }, "name" : "Bob", "email" : "bob@example.com", "age" : { "$numberInt" : "25" } }
 { "_id" : { "$oid" : "6982b4f65bc314981baade52" }, "name" : "Charlie", "email" : "charlie@example.com", "age" : { "$numberInt" : "35" } }
 { "_id" : { "$oid" : "6982b5a7f711f7e076d7e1e2" }, "name" : "Alice", "age" : { "$numberInt" : "30" }, "city" : "Seattle" }
 { "_id" : { "$oid" : "6982b5a7f711f7e076d7e1e3" }, "name" : "Bob", "age" : { "$numberInt" : "25" }, "city" : "Redmond" }
 { "_id" : { "$oid" : "6982b5a7f711f7e076d7e1e4" }, "name" : "Charlie", "age" : { "$numberInt" : "35" }, "city" : "Bellevue" }
(6 rows)
```

> **Note**: The `documentdb_core.bson_to_json_string()` function converts BSON binary data to human-readable JSON. Without the extension symlinks (Step 4.6), this query would fail with `could not access file "$libdir/documentdb_core"`.

---

## Troubleshooting

### Error: `could not access file "$libdir/documentdb_core": No such file or directory`

**Symptom**: When querying BSON data:
```
ERROR:  could not access file "$libdir/documentdb_core": No such file or directory
```

**Cause**: Flex uses extension names `documentdb_core` and `documentdb`, but AKS image has `pg_documentdb_core.so` and `pg_documentdb.so`. The pg_extension catalog references the Flex names, but the AKS libraries have different filenames.

**Fix**: Use the pod spec in Step 4.6 which creates symlinks:
- `documentdb_core.so` → `pg_documentdb_core.so`
- `documentdb.so` → `pg_documentdb.so`

The init container copies all libraries to an emptyDir, creates relative symlinks, then mounts the emptyDir over `/usr/lib/postgresql/16/lib` (the `$libdir`).

**Verification**:
```bash
kubectl exec documentdb-standby -n documentdb-instance-ns -- ls -la /usr/lib/postgresql/16/lib/ | grep documentdb
```
Should show:
```
lrwxrwxrwx  1 root root      21 Feb  4 03:06 documentdb_core.so -> pg_documentdb_core.so
lrwxrwxrwx  1 root root      16 Feb  4 03:06 documentdb.so -> pg_documentdb.so
-rw-r--r--  1 root root 4907272 Apr 24  2025 pg_documentdb_core.so
-rw-r--r--  1 root root 1862352 Apr 24  2025 pg_documentdb.so
```

### Error: CNPG auto-promotes standby to primary

**Symptom**: Logs show:
```
"Setting myself as primary"
"I'm the target primary, applying WALs and promoting my instance"
"selected new timeline ID: 2"
```

**Cause**: CNPG operator manages cluster leadership and auto-promotes standbys when it detects them.

**Fix**: Use the **standalone DocumentDB pod approach** (Step 4) which bypasses CNPG entirely. Keep CNPG operator scaled to 0.

### Error: `could not restore file "00000003.history" from archive: command not found`

**Symptom**: Pod crashes with:
```
FATAL: could not restore file "00000003.history" from archive: command not found
```

**Cause**: The `custom.conf` has `archive_mode = 'on'` and `restore_command` pointing to `/controller/manager` which doesn't exist in standalone pods.

**Fix**: Set `archive_mode = 'off'` in `custom.conf`:
```bash
kubectl exec pvc-fixer -n documentdb-instance-ns -- \
  sed -i "s/^archive_mode = 'on'/archive_mode = 'off'/" /pgdata/pgdata/custom.conf
```

### Error: `could not access file "documentdb": No such file or directory`

**Symptom**: Pod crashes with:
```
FATAL: could not access file "documentdb": No such file or directory
```

**Cause**: Using Flex's postgresql.conf which has `shared_preload_libraries` referencing extension paths that don't exist in the AKS container.

**Fix**: Use the AKS postgresql.conf (saved in Step 4.1) instead of Flex's. The AKS image has extensions in different paths.

### Error: `highest timeline X of the primary is behind recovery timeline Y`

**Symptom**: Standby can't connect to primary:
```
FATAL: highest timeline 1 of the primary is behind recovery timeline 2
```

**Cause**: The standby was previously promoted (created a new timeline), but the primary is still on the old timeline.

**Fix**: Redo pg_basebackup from scratch to get a fresh copy on the correct timeline.

### Error: `could not load server certificate file`

**Symptom**: Pod fails with:
```
FATAL: could not load server certificate file "/controller/certificates/server.crt": No such file or directory
```

**Cause**: The standalone pod doesn't have the TLS certificates that CNPG normally provides.

**Fix**: Use an init container to generate self-signed certificates (included in Step 4.6).

### Error: `include_dir '/datadrive/pg/data/conf.d' does not exist`

**Cause**: Flex postgresql.conf references a directory that doesn't exist in CNPG containers.

**Fix**: Comment out the `include_dir` line on Flex **before** running pg_basebackup:
```bash
sudo sed -i "s|^include_dir='/datadrive/pg/data/conf.d'|# include_dir='/datadrive/pg/data/conf.d'|" /datadrive/pg/data/postgresql.conf
```

### Error: `could not create lock file "/var/run/postgresql/.s.PGSQL.5432.lock"`

**Cause**: postgresql.conf has `unix_socket_directories = '/var/run/postgresql'` but this directory doesn't exist in CNPG containers.

**Fix**: Change to `/controller/run` on Flex **before** pg_basebackup, or fix on PVC after:
```bash
# On Flex (preferred - do before basebackup):
echo "unix_socket_directories = '/controller/run'" | sudo tee -a /datadrive/pg/data/postgresql.conf

# Or fix on PVC after basebackup:
kubectl exec pvc-fixer -n documentdb-instance-ns -- \
  sed -i "s|unix_socket_directories = '/var/run/postgresql'|unix_socket_directories = '/controller/run'|g" /pgdata/pgdata/postgresql.conf
```

### Error: `recovery aborted because of insufficient parameter settings`

**Cause**: Standby has lower values for parameters like `max_connections`, `max_worker_processes` than the primary.

**Fix**: Add overrides in `override.conf` with values >= primary:
```bash
kubectl exec pvc-fixer -n documentdb-instance-ns -- bash -c '
cat >> /pgdata/pgdata/override.conf << CONF
max_connections = 900
max_worker_processes = 25
max_wal_senders = 15
max_prepared_transactions = 5
CONF
'
```

### Error: `role "postgres" does not exist`

**Cause**: CNPG instance manager tries to connect as `postgres` user which doesn't exist on Flex-sourced data (Flex uses `azuresu`).

**Note**: This doesn't prevent replication from working. The error is just CNPG's health check failing. In the standalone pod approach, this error doesn't occur.

### Container keeps crashing (CrashLoopBackOff)

**Cause**: Usually a config issue. Check logs:
```bash
kubectl logs <pod-name> -n <namespace> 2>&1 | grep -E "FATAL|ERROR" | tail -20
```

Common causes:
- Missing TLS certificates → use init container to create them
- `archive_mode = 'on'` → set to `'off'`
- Missing socket directory → fix `unix_socket_directories`
- Parameter mismatch → add overrides for `max_connections`, etc.
- Missing include_dir → comment out on Flex

---

## Environment Variables for setup-aks-replication.sh

| Variable | Description | Default |
|----------|-------------|---------|
| `NAMESPACE` | Kubernetes namespace | `documentdb-preview-ns` |
| `POD_NAME` | DocumentDB pod name | `documentdb-preview-1` |
| `FLEX_HOST` | Flex hostname/IP | (required) |
| `FLEX_PORT` | Flex PostgreSQL port | `5432` |
| `FLEX_USER` | Replication user | `azuresu` |
| `FLEX_PASSWORD` | Replication password | (required) |
| `SLOT_NAME` | Replication slot name | `aks_slot` |

---

## Key Learnings

1. **CNPG auto-promotes standbys**: The biggest issue! CNPG operator detects standby mode and **automatically promotes** the instance to primary. This completely breaks external replication.

2. **Standalone pod is the solution**: Bypass CNPG by using a standalone DocumentDB pod (`ghcr.io/microsoft/documentdb/documentdb-local:16`) that runs postgres directly without the CNPG instance manager.

3. **Archive mode must be OFF**: CNPG's `custom.conf` has `archive_mode = 'on'` with `restore_command` pointing to `/controller/manager`. This doesn't exist in standalone pods and causes `FATAL: command not found` errors.

4. **Use AKS postgresql.conf, not Flex's**: Flex and AKS have different extension paths. pg_basebackup copies Flex's config, but using AKS's original `postgresql.conf` (with correct `shared_preload_libraries` paths) is essential.

5. **TLS certificates required**: The DocumentDB image expects TLS certificates in `/controller/certificates/`. Use an init container to generate self-signed certs.

6. **Config file hierarchy matters**: CNPG uses `postgresql.conf` → `custom.conf` → `override.conf`. Settings in later files override earlier ones. Put replication settings in `override.conf`.

7. **Parameter compatibility**: Standby must have equal or higher values for `max_connections`, `max_worker_processes`, etc.

8. **Timeline mismatches**: If a standby was previously promoted (auto-promoted by CNPG), it creates a new timeline. The only fix is a fresh pg_basebackup.

9. **CNPG container limitations**: Cannot stop PostgreSQL inside a running CNPG pod - the instance manager exits when PG stops, killing the container.

10. **Keep CNPG operator scaled down**: When running standalone standby, keep `documentdb-operator-cloudnative-pg` at 0 replicas to prevent interference.

11. **Extension library naming mismatch**: Flex uses `documentdb_core.so` but AKS has `pg_documentdb_core.so`. Without symlinks, BSON data can't be read. Solution: Copy libraries to emptyDir, create symlinks, mount over `$libdir`.

12. **Use `bson_to_json_string()` for readable output**: The `documentdb_core.bson_to_json_string(document)` function converts BSON binary to human-readable JSON for verification.

---

## Reference

| Item | Value |
|------|-------|
| AKS Cluster | `<your-aks-cluster>` |
| Resource Group | `<your-resource-group>` |
| Namespace | documentdb-instance-ns |
| AKS Egress IP | `<your-aks-egress-ip>` |
| Flex Host | `<flex-hostname>.eastus2.cloudapp.azure.com` |
| Replication User | azuresu |
| Replication Slot | aks_slot |
| DocumentDB Image | ghcr.io/microsoft/documentdb/documentdb-local:16 |
| Standalone Pod Name | documentdb-standby |

---

## Summary of Working Approach

1. **Deploy fresh AKS instance** → save its `postgresql.conf`
2. **Scale down ALL operators** (CNPG, sidecar-injector, documentdb-operator)
3. **Delete CNPG-managed pod** → create PVC fixer
4. **Run pg_basebackup** from Flex
5. **Restore AKS postgresql.conf** + create `custom.conf` (archive_mode=off) + `override.conf` (primary_conninfo)
6. **Create standalone DocumentDB pod** with:
   - Init container for TLS certs
   - Init container copies libraries and creates symlinks for Flex extension naming
   - `pglib` emptyDir mounted over `$libdir`
7. **Verify streaming**: logs show `started streaming WAL from primary`
8. **Verify data readable**: `SELECT documentdb_core.bson_to_json_string(document) FROM documentdb_data.documents_2;`

The key insights are:
- **CNPG cannot be used for external replication** because it auto-promotes standbys
- **Standalone pod approach** gives full control over the PostgreSQL standby behavior
- **Extension symlinks are required** to read BSON data (Flex uses `documentdb_core`, AKS has `pg_documentdb_core`)

---

## Appendix: Root Cause Analysis - Why Manual Configuration Was Required

### What pg_basebackup Copies (from PGDATA)

| Copied | File/Folder | Purpose |
|--------|-------------|---------|
| ✅ | `postgresql.conf` | Main config |
| ✅ | `pg_hba.conf` | Authentication |
| ✅ | `custom.conf`, `override.conf` | CNPG config files |
| ✅ | `base/` | Database data files |
| ✅ | `global/` | Cluster-wide tables (including `pg_extension` catalog) |
| ✅ | `pg_wal/` | WAL files |
| ✅ | All other data directories | |

### What is NOT Copied (Part of Container Image)

| Not Copied | Location | What We Had to Fix |
|------------|----------|-------------------|
| ❌ | `/usr/lib/postgresql/16/lib/*.so` | Extension libraries |
| ❌ | `/usr/share/postgresql/16/extension/*.control` | Extension metadata |
| ❌ | PostgreSQL binaries | |

### The Core Problem: Extension Naming Mismatch

| Component | Flex (Primary) | AKS (Secondary) |
|-----------|----------------|-----------------|
| Library files | `documentdb_core.so` | `pg_documentdb_core.so` |
| Extension name | `documentdb_core` | `pg_documentdb_core` |
| Control file `module_pathname` | `$libdir/documentdb_core` | `$libdir/pg_documentdb_core` |
| `shared_preload_libraries` | `documentdb_core,documentdb` | `pg_documentdb_core,pg_documentdb` |
| Extension version | `0.110-0` | `0.103-0` (or different) |

### What Would Make Replication "Just Work"

For zero manual configuration (beyond slot name and primary host), these files need to be **identical** between Flex and AKS images:

#### 1. Extension Library Names (Critical)
```
/usr/lib/postgresql/16/lib/
├── documentdb_core.so      # Same name on both
├── documentdb.so           # Same name on both
```

#### 2. Extension Control Files (Critical)
```
/usr/share/postgresql/16/extension/
├── documentdb_core.control   # Same name, same module_pathname
├── documentdb.control        # Same name, same module_pathname
```

The control file content must match:
```ini
# documentdb_core.control - MUST BE IDENTICAL
comment = 'Core API surface for DocumentDB'
default_version = '0.110-0'           # Same version!
module_pathname = '$libdir/documentdb_core'  # Same path!
```

#### 3. Extension Versions (Critical)
Both must have the **exact same version** (e.g., `0.110-0`).

### The "Golden Path" for Easy Replication

If Flex and AKS used **identical extension packaging**:

| Requirement | Current State | Ideal State |
|-------------|---------------|-------------|
| Library filename | Flex: `documentdb_core.so`, AKS: `pg_documentdb_core.so` | Both: `documentdb_core.so` |
| Extension name | Flex: `documentdb_core`, AKS: `pg_documentdb_core` | Both: `documentdb_core` |
| Extension version | Flex: `0.110-0`, AKS: `0.103-0` | Both: `0.110-0` |
| Control file module_pathname | Different | Identical |

**With these aligned, the only config needed would be:**
```bash
# override.conf - THE ONLY MANUAL CONFIG NEEDED
primary_conninfo = 'host=<FLEX_HOST> port=5432 user=azuresu password=xxx'
primary_slot_name = 'aks_slot'
```

Everything else would "just work" because pg_basebackup would copy configs that are already compatible with the target container's extension libraries.

### Recommendation for Product Team

To enable seamless Flex ↔ OSS DocumentDB replication:

1. **Standardize extension naming** across Flex and OSS images
2. **Align extension versions** or ensure backward compatibility
3. **Use identical `module_pathname`** in control files

This is fundamentally a **packaging/naming alignment issue**, not a replication protocol issue.
