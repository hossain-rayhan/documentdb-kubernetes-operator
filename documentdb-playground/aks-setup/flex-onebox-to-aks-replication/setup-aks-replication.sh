#!/bin/bash

# AKS DocumentDB Replication Setup Script
# This script sets up AKS DocumentDB as a streaming replication standby from Flex OneBox
#
# POC Flow (setup-standby command):
# 1. Scale down operators (CNPG and DocumentDB) to prevent them from killing/restarting pods
# 2. Stop PostgreSQL inside the existing pod (gateway container stays running!)
# 3. Clear data and run pg_basebackup from Flex primary
# 4. Fix Flex-specific config (include_dir, tablespace, etc.)
# 5. Start PostgreSQL in standby mode
# 6. Verify replication is streaming
#
# Key: The DocumentDB pod is NEVER deleted - only PostgreSQL is stopped/started inside it.
# This keeps the gateway sidecar alive, allowing mongosh connections throughout.

set -e

# Configuration - UPDATE THESE FOR YOUR ENVIRONMENT
NAMESPACE="${NAMESPACE:-documentdb-instance-ns}"
POD_NAME="${POD_NAME:-sample-documentdb-1}"
STANDBY_POD="${STANDBY_POD:-standby-setup}"
RESOURCE_GROUP="${RESOURCE_GROUP:-<your-resource-group>}"
CLUSTER_NAME="${CLUSTER_NAME:-<your-aks-cluster>}"
TARGET_VERSION="${TARGET_VERSION:-0.110-0}"
AKS_SUBSCRIPTION="${AKS_SUBSCRIPTION:-<your-subscription-id>}"
PVC_NAME="${PVC_NAME:-sample-documentdb-1}"

# Flex OneBox defaults - UPDATE THESE FOR YOUR FLEX VM
FLEX_HOST="${FLEX_HOST:-<flex-hostname>.eastus2.cloudapp.azure.com}"
FLEX_PORT="${FLEX_PORT:-5432}"
FLEX_USER="${FLEX_USER:-azuresu}"
FLEX_PASSWORD="${FLEX_PASSWORD:-<your-flex-password>}"

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
CYAN='\033[0;36m'
NC='\033[0m'

log() { echo -e "${BLUE}[INFO]${NC} $1"; }
success() { echo -e "${GREEN}[✓]${NC} $1"; }
warn() { echo -e "${YELLOW}[!]${NC} $1"; }
error() { echo -e "${RED}[✗]${NC} $1"; exit 1; }
step() { echo -e "\n${CYAN}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}"; echo -e "${CYAN}  $1${NC}"; echo -e "${CYAN}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}"; }

# =============================================================================
# Step 1: Get AKS Egress IP
# =============================================================================
get_egress_ip() {
    log "Setting Azure subscription to AKS subscription..."
    az account set -s "$AKS_SUBSCRIPTION" || error "Failed to set subscription. Run 'az login' first."
    
    log "Getting AKS egress IP (for Flex pg_hba.conf)..."
    
    # Get the managed resource group
    NODE_RG=$(az aks show --resource-group "$RESOURCE_GROUP" --name "$CLUSTER_NAME" --query "nodeResourceGroup" -o tsv 2>&1 | grep -v "behavior of this command" | tr -d '\r')
    
    if [ -z "$NODE_RG" ]; then
        error "Failed to get AKS node resource group. Check subscription and cluster name."
    fi
    
    log "Found node resource group: $NODE_RG"
    
    # Get public IPs
    EGRESS_IP=$(az network public-ip list --resource-group "$NODE_RG" --query "[?contains(name, 'kubernetes') == \`false\`].ipAddress" -o tsv 2>/dev/null | head -1 | tr -d '\r')
    LB_IP=$(az network public-ip list --resource-group "$NODE_RG" --query "[?contains(name, 'kubernetes')].ipAddress" -o tsv 2>/dev/null | tr -d '\r')
    
    echo ""
    echo "=============================================="
    echo "AKS IP Addresses"
    echo "=============================================="
    echo -e "Egress IP (for Flex pg_hba.conf): ${GREEN}${EGRESS_IP}${NC}"
    echo -e "LoadBalancer IP (inbound):        ${BLUE}${LB_IP}${NC}"
    echo ""
    echo "Add this to Flex pg_hba.conf:"
    echo -e "${YELLOW}host replication replicator ${EGRESS_IP}/32 md5${NC}"
    echo "=============================================="
}

# =============================================================================
# Step 2: Check Current Extension Versions
# =============================================================================
check_extension_versions() {
    log "Checking current DocumentDB extension versions..."
    
    kubectl exec -it "$POD_NAME" -n "$NAMESPACE" -c postgres -- \
        psql -U postgres -c "SELECT extname, extversion FROM pg_extension WHERE extname LIKE '%documentdb%';"
}

# =============================================================================
# Step 3: Update Extension Version (catalog only)
# =============================================================================
update_extension_version() {
    log "Updating extension version to $TARGET_VERSION in pg_extension catalog..."
    
    # Update both extensions
    kubectl exec -it "$POD_NAME" -n "$NAMESPACE" -c postgres -- \
        psql -U postgres -c "UPDATE pg_extension SET extversion = '$TARGET_VERSION' WHERE extname IN ('documentdb', 'documentdb_core');"
    
    success "Extension versions updated to $TARGET_VERSION"
    
    # Verify
    log "Verifying update..."
    kubectl exec -it "$POD_NAME" -n "$NAMESPACE" -c postgres -- \
        psql -U postgres -c "SELECT extname, extversion FROM pg_extension WHERE extname LIKE '%documentdb%';"
}

# =============================================================================
# Step 4: Remove Extra Tables (if needed)
# =============================================================================
remove_extra_tables() {
    log "Checking for extra tables to remove..."
    
    # Check current tables
    TABLES=$(kubectl exec -it "$POD_NAME" -n "$NAMESPACE" -c postgres -- \
        psql -U postgres -t -c "SELECT tablename FROM pg_tables WHERE schemaname = 'documentdb_data' ORDER BY tablename;" 2>/dev/null | tr -d ' ')
    
    # Remove extra document/retry tables (keep only _1)
    for table in documents_2 documents_3 retry_2 retry_3; do
        if echo "$TABLES" | grep -q "$table"; then
            log "Dropping table documentdb_data.$table..."
            kubectl exec -it "$POD_NAME" -n "$NAMESPACE" -c postgres -- \
                psql -U postgres -c "DROP TABLE IF EXISTS documentdb_data.$table CASCADE;"
            success "Dropped $table"
        fi
    done
    
    success "Extra tables removed (if any existed)"
}

# =============================================================================
# Step 5: Test Connectivity to Flex
# =============================================================================
test_flex_connection() {
    log "Testing connectivity to Flex at $FLEX_HOST:$FLEX_PORT..."
    
    # Use existing standby pod if available, otherwise use documentdb pod
    TEST_POD=$STANDBY_POD
    if ! kubectl get pod $STANDBY_POD -n $NAMESPACE &>/dev/null; then
        TEST_POD=$POD_NAME
        CONTAINER_FLAG="-c postgres"
    else
        CONTAINER_FLAG=""
    fi
    
    kubectl exec $CONTAINER_FLAG $TEST_POD -n "$NAMESPACE" -- \
        bash -c "PGPASSWORD='$FLEX_PASSWORD' psql -h $FLEX_HOST -p $FLEX_PORT -U $FLEX_USER -d postgres -c 'SELECT 1 as connected;'"
    
    if [ $? -eq 0 ]; then
        success "Successfully connected to Flex!"
    else
        error "Failed to connect to Flex. Check network/firewall settings."
    fi
}

# =============================================================================
# Step 6: Setup as Standby - AUTOMATED POC
# =============================================================================
setup_standby() {
    step "Setting up AKS as Streaming Replication Standby"
    
    echo -e "Primary (Flex): ${GREEN}$FLEX_HOST:$FLEX_PORT${NC}"
    echo -e "Standby (AKS):  ${BLUE}$NAMESPACE/$POD_NAME${NC}"
    echo ""
    
    warn "This will DESTROY all data on AKS and set it up as a read-only standby!"
    read -p "Are you sure? (yes/no): " confirm
    if [ "$confirm" != "yes" ]; then
        echo "Aborted."
        exit 0
    fi
    
    # Step 1: Scale down operators
    step "Step 1: Scaling down operators"
    log "Scaling down CNPG and DocumentDB operators to prevent interference..."
    kubectl scale deployment -n cnpg-system --all --replicas=0 2>/dev/null || true
    kubectl scale deployment -n documentdb-operator --all --replicas=0 2>/dev/null || true
    sleep 3
    success "Operators scaled down"
    
    # Step 2-4: Stop PostgreSQL, run basebackup, fix config, start PostgreSQL (ALL IN ONE SESSION)
    # This must be done in ONE kubectl exec because stopping PostgreSQL causes container restart
    step "Step 2: Running pg_basebackup from Flex (all-in-one)"
    log "Stop PG → Clear data → Basebackup → Fix config → Start PG"
    
    kubectl exec --request-timeout=0 -n $NAMESPACE $POD_NAME -c postgres -- bash -c "
        export PGPASSWORD='$FLEX_PASSWORD'
        PGDATA=/var/lib/postgresql/data/pgdata
        
        echo '=== Stopping PostgreSQL ==='
        /usr/lib/postgresql/16/bin/pg_ctl stop -D \$PGDATA -m fast 2>/dev/null || true
        sleep 2
        
        echo '=== Clearing old data ==='
        rm -rf \$PGDATA/*
        
        # Create tablespace dir inside the PVC (not /mnt which is read-only)
        mkdir -p /var/lib/postgresql/data/pg_tmp
        
        echo '=== Running pg_basebackup from Flex ==='
        PGPASSWORD='$FLEX_PASSWORD' pg_basebackup -h $FLEX_HOST -p $FLEX_PORT -U $FLEX_USER \
            -D \$PGDATA -Fp -Xs -P -R -v \
            --tablespace-mapping=/mnt/pg_tmp=/var/lib/postgresql/data/pg_tmp
        
        echo '=== Fixing Flex-specific configuration ==='
        # Comment out Flex-specific include_dir (causes 'No such file or directory' on AKS)
        sed -i \"s|^include_dir='/datadrive/pg/data/conf.d'|#include_dir='/datadrive/pg/data/conf.d'|\" \$PGDATA/postgresql.conf
        
        # Fix tablespace symlink if needed
        if [ -L \$PGDATA/pg_tblspc/16386 ]; then
            rm \$PGDATA/pg_tblspc/16386
            mkdir -p /var/lib/postgresql/data/pg_tmp/PG_16_202307071
            chown -R postgres:postgres /var/lib/postgresql/data/pg_tmp
            ln -s /var/lib/postgresql/data/pg_tmp \$PGDATA/pg_tblspc/16386
            chown -h postgres:postgres \$PGDATA/pg_tblspc/16386
        fi
        
        # Add local trust entry to pg_hba.conf for local queries  
        echo 'host all all 127.0.0.1/32 trust' >> \$PGDATA/pg_hba.conf
        
        # Add required settings to postgresql.auto.conf
        cat >> \$PGDATA/postgresql.auto.conf << 'CONF'
max_worker_processes = 32
max_connections = 1000
CONF
        
        echo '=== Creating /var/run/postgresql ==='
        mkdir -p /var/run/postgresql
        
        echo '=== Starting PostgreSQL in standby mode ==='
        /usr/lib/postgresql/16/bin/pg_ctl start -D \$PGDATA -l /tmp/pg.log
        sleep 5
        
        echo '=== PostgreSQL log ==='
        tail -15 /tmp/pg.log 2>/dev/null || echo 'No log'
        
        echo '=== Verifying standby status ==='
        psql -h 127.0.0.1 -U $FLEX_USER -d postgres -c \"SELECT pg_is_in_recovery() as is_standby, pg_last_wal_receive_lsn() as received, pg_last_wal_replay_lsn() as replayed;\"
    "
    success "Basebackup and standby setup completed"
    
    # Step 3: Final verification
    step "Step 3: Final verification"
    sleep 3
    
    kubectl exec -n $NAMESPACE $POD_NAME -c postgres -- bash -c "
        psql -h 127.0.0.1 -U $FLEX_USER -d postgres -c \"SELECT pg_is_in_recovery() as is_standby, pg_last_wal_receive_lsn() as received, pg_last_wal_replay_lsn() as replayed;\"
    " 2>/dev/null | grep -v "collation version"
    
    echo ""
    success "🎉 AKS is now a streaming replication standby from Flex!"
    echo ""
    echo -e "${GREEN}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}"
    echo -e "${GREEN}  POC Ready - Gateway Available!${NC}"
    echo -e "${GREEN}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}"
    echo ""
    echo "The gateway sidecar is still running. Connect via MongoDB protocol:"
    echo ""
    EXTERNAL_IP=$(kubectl get svc documentdb-service-documentdb-preview -n $NAMESPACE -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null)
    echo "  mongosh mongodb://<user>:<pass>@${EXTERNAL_IP}:10260/?directConnection=true"
    echo ""
    echo "Or test replication:"
    echo "  1. Insert on Flex VM"
    echo "  2. Query on AKS: $0 query-standby"
    echo ""
    echo "Note: Standby is READ-ONLY. MongoDB writes will fail."
}

# =============================================================================
# Step 7: Print Connection Info
# =============================================================================
print_connection_info() {
    log "Getting AKS DocumentDB connection info..."
    
    # Get credentials from secret
    kubectl get secret documentdb-preview-app -n "$NAMESPACE" -o jsonpath='{.data}' 2>/dev/null | \
        jq -r 'to_entries[] | select(.key == "password" or .key == "username" or .key == "host") | "\(.key): \(.value | @base64d)"' || true
    
    # Get external IP
    EXTERNAL_IP=$(kubectl get svc -n "$NAMESPACE" -o jsonpath='{.items[?(@.spec.type=="LoadBalancer")].status.loadBalancer.ingress[0].ip}' 2>/dev/null)
    
    echo ""
    echo "=============================================="
    echo "AKS DocumentDB Connection"
    echo "=============================================="
    echo "External IP: $EXTERNAL_IP"
    echo "Port: 10260 (MongoDB gateway)"
    echo "=============================================="
}

# =============================================================================
# Step 8: Query Standby
# =============================================================================
query_standby() {
    log "Querying standby for replication status and data..."
    
    # Determine which pod to use
    if kubectl get pod $POD_NAME -n $NAMESPACE &>/dev/null; then
        TARGET_POD=$POD_NAME
        CONTAINER="-c postgres"
    elif kubectl get pod $STANDBY_POD -n $NAMESPACE &>/dev/null; then
        TARGET_POD=$STANDBY_POD
        CONTAINER=""
    else
        error "No standby pod found. Run setup-standby first."
    fi
    
    kubectl exec $CONTAINER -n $NAMESPACE $TARGET_POD -- bash -c "
        echo '=== Replication Status ==='
        su postgres -c \"psql -U $FLEX_USER -d postgres -c \\\"SELECT pg_is_in_recovery() as is_standby, pg_last_wal_receive_lsn() as received, pg_last_wal_replay_lsn() as replayed, now() - pg_last_xact_replay_timestamp() as replication_lag;\\\"\"
        
        echo ''
        echo '=== Document Counts ==='
        su postgres -c \"psql -U $FLEX_USER -d postgres -c \\\"SELECT 
            (SELECT COUNT(*) FROM documentdb_data.documents_1) as docs_1,
            (SELECT COUNT(*) FROM documentdb_data.documents_2) as docs_2,
            pg_last_xact_replay_timestamp() as last_replicated;\\\"\"
    " 2>/dev/null | grep -v "collation version" | grep -v "HINT:"
}

# =============================================================================
# Step 9: Scale Operators Back Up
# =============================================================================
scale_operators_up() {
    step "Scaling operators back up"
    warn "This will restore CNPG and DocumentDB operators."
    warn "They may try to reconcile and potentially interfere with the standby."
    read -p "Continue? (yes/no): " confirm
    if [ "$confirm" != "yes" ]; then
        echo "Aborted."
        exit 0
    fi
    
    kubectl scale deployment -n cnpg-system --all --replicas=1 2>/dev/null || true
    kubectl scale deployment -n documentdb-operator --all --replicas=1 2>/dev/null || true
    success "Operators scaled back up"
}

# =============================================================================
# Step 10: Cleanup POC
# =============================================================================
cleanup_poc() {
    step "Cleaning up POC resources"
    
    log "Deleting standby setup pod..."
    kubectl delete pod $STANDBY_POD -n $NAMESPACE --force --grace-period=0 2>/dev/null || true
    
    log "Scaling operators back up..."
    kubectl scale deployment -n cnpg-system --all --replicas=1 2>/dev/null || true
    kubectl scale deployment -n documentdb-operator --all --replicas=1 2>/dev/null || true
    
    success "POC resources cleaned up"
    echo ""
    echo "Note: The CNPG operator will recreate the DocumentDB pod."
    echo "Run: kubectl get pods -n $NAMESPACE -w"
}

# =============================================================================
# Step 11: Status Check
# =============================================================================
status_check() {
    step "Checking POC Status"
    
    echo "=== Operators ==="
    echo -n "CNPG: "
    kubectl get pods -n cnpg-system --no-headers 2>/dev/null | wc -l | xargs -I {} sh -c 'if [ {} -eq 0 ]; then echo "SCALED DOWN"; else echo "RUNNING"; fi'
    echo -n "DocumentDB: "
    kubectl get pods -n documentdb-operator --no-headers 2>/dev/null | wc -l | xargs -I {} sh -c 'if [ {} -eq 0 ]; then echo "SCALED DOWN"; else echo "RUNNING"; fi'
    
    echo ""
    echo "=== Pods in $NAMESPACE ==="
    kubectl get pods -n $NAMESPACE
    
    echo ""
    echo "=== Standby Status ==="
    
    # Check original pod first, then standby pod
    if kubectl get pod $POD_NAME -n $NAMESPACE &>/dev/null; then
        TARGET_POD=$POD_NAME
        CONTAINER="-c postgres"
        echo "Using pod: $POD_NAME (with gateway)"
    elif kubectl get pod $STANDBY_POD -n $NAMESPACE &>/dev/null; then
        TARGET_POD=$STANDBY_POD
        CONTAINER=""
        echo "Using pod: $STANDBY_POD (standalone)"
    else
        echo "No standby pod found"
        return
    fi
    
    kubectl exec $CONTAINER -n $NAMESPACE $TARGET_POD -- bash -c "
        if pgrep -x postgres > /dev/null; then
            echo 'PostgreSQL: RUNNING'
            su postgres -c \"psql -U $FLEX_USER -d postgres -t -c \\\"SELECT 'Replication: ' || CASE WHEN pg_is_in_recovery() THEN 'STREAMING from $FLEX_HOST' ELSE 'NOT IN RECOVERY' END;\\\"\" 2>/dev/null | tr -d ' '
        else
            echo 'PostgreSQL: NOT RUNNING'
        fi
    " 2>/dev/null | grep -v "collation"
    
    echo ""
    echo "=== Service Endpoints ==="
    kubectl get endpoints documentdb-service-documentdb-preview -n $NAMESPACE 2>/dev/null || echo "Service not found"
}

# =============================================================================
# Main
# =============================================================================
usage() {
    echo ""
    echo -e "${CYAN}AKS DocumentDB Streaming Replication POC${NC}"
    echo ""
    echo "Usage: $0 <command>"
    echo ""
    echo -e "${GREEN}Quick Start (POC):${NC}"
    echo "  setup-standby      🚀 Full automated setup (recommended)"
    echo "  query-standby      📊 Query standby for data/status"
    echo "  status             📋 Check POC status"
    echo "  cleanup            🧹 Clean up POC and restore operators"
    echo ""
    echo -e "${BLUE}Individual Steps:${NC}"
    echo "  egress-ip          Get AKS egress IP for Flex pg_hba.conf"
    echo "  check-version      Check current extension versions"
    echo "  update-version     Update extension version to $TARGET_VERSION"
    echo "  remove-tables      Remove extra documentdb_data tables"
    echo "  test-connection    Test connectivity to Flex"
    echo "  connection-info    Print AKS connection info"
    echo "  scale-operators    Scale operators back up"
    echo ""
    echo -e "${YELLOW}Configuration:${NC}"
    echo "  Current Flex:      $FLEX_HOST:$FLEX_PORT"
    echo "  Current User:      $FLEX_USER"
    echo "  Namespace:         $NAMESPACE"
    echo "  PVC:               $PVC_NAME"
    echo ""
    echo -e "${YELLOW}Environment Variables:${NC}"
    echo "  FLEX_HOST          Flex hostname (default: $FLEX_HOST)"
    echo "  FLEX_PORT          Flex port (default: $FLEX_PORT)"
    echo "  FLEX_USER          Flex user (default: $FLEX_USER)"
    echo "  FLEX_PASSWORD      Flex password"
    echo "  NAMESPACE          K8s namespace (default: $NAMESPACE)"
    echo "  PVC_NAME           PVC name (default: $PVC_NAME)"
    echo ""
}

case "${1:-}" in
    egress-ip)
        get_egress_ip
        ;;
    check-version)
        check_extension_versions
        ;;
    update-version)
        update_extension_version
        ;;
    remove-tables)
        remove_extra_tables
        ;;
    test-connection)
        test_flex_connection
        ;;
    setup-standby)
        setup_standby
        ;;
    query-standby)
        query_standby
        ;;
    connection-info)
        print_connection_info
        ;;
    scale-operators)
        scale_operators_up
        ;;
    cleanup)
        cleanup_poc
        ;;
    status)
        status_check
        ;;
    prepare)
        get_egress_ip
        echo ""
        update_extension_version
        echo ""
        remove_extra_tables
        echo ""
        print_connection_info
        ;;
    *)
        usage
        ;;
esac
