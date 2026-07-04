#!/usr/bin/env bash

set -euo pipefail

# Configuration
SERVICE="gpu-fallback-webhook"
NAMESPACE="gpu-fallback"
SECRET="gpu-fallback-webhook-certs"
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TMP_DIR=$(mktemp -d)

# Cleanup on exit
cleanup() {
  rm -rf "${TMP_DIR}"
}
trap cleanup EXIT

echo "Generating TLS certificates for webhook service..."

# 1. Create CA certificate and key
openssl genrsa -out "${TMP_DIR}/ca.key" 2048
openssl req -x509 -new -nodes -key "${TMP_DIR}/ca.key" -subj "/CN=GPU Fallback Webhook CA" -days 3650 -out "${TMP_DIR}/ca.crt"

# 2. Create server certificate and key
openssl genrsa -out "${TMP_DIR}/server.key" 2048

# 3. Create CSR configuration with SANs (Subject Alternative Names)
cat <<EOF > "${TMP_DIR}/csr.conf"
[req]
req_extensions = v3_req
distinguished_name = req_distinguished_name
[req_distinguished_name]
[ v3_req ]
basicConstraints = CA:FALSE
keyUsage = nonRepudiation, digitalSignature, keyEncipherment
subjectAltName = @alt_names
[alt_names]
DNS.1 = ${SERVICE}
DNS.2 = ${SERVICE}.${NAMESPACE}
DNS.3 = ${SERVICE}.${NAMESPACE}.svc
DNS.4 = ${SERVICE}.${NAMESPACE}.svc.cluster.local
EOF

# 4. Generate CSR
openssl req -new -key "${TMP_DIR}/server.key" -out "${TMP_DIR}/server.csr" \
    -subj "/CN=${SERVICE}.${NAMESPACE}.svc" \
    -config "${TMP_DIR}/csr.conf"

# 5. Sign the CSR with the CA
openssl x509 -req -in "${TMP_DIR}/server.csr" \
    -CA "${TMP_DIR}/ca.crt" \
    -CAkey "${TMP_DIR}/ca.key" \
    -CAcreateserial \
    -out "${TMP_DIR}/server.crt" \
    -days 3650 \
    -extensions v3_req \
    -extfile "${TMP_DIR}/csr.conf"

echo "Creating Kubernetes Namespace '${NAMESPACE}' (if it doesn't exist)..."
kubectl create namespace "${NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -

echo "Deleting old Kubernetes Secret if it exists..."
kubectl delete secret "${SECRET}" -n "${NAMESPACE}" --ignore-not-found

echo "Creating Kubernetes Secret with generated TLS certificates..."
kubectl create secret generic "${SECRET}" \
    --from-file=tls.key="${TMP_DIR}/server.key" \
    --from-file=tls.crt="${TMP_DIR}/server.crt" \
    -n "${NAMESPACE}"

# 6. Read CA bundle and patch the mutating webhook configuration
CA_BUNDLE=$(base64 "${TMP_DIR}/ca.crt" | tr -d '\n')

echo "Patching webhook configuration with CA Bundle..."
sed "s/CA_BUNDLE_PLACEHOLDER/${CA_BUNDLE}/g" "${DIR}/webhook-configuration.yaml" > "${DIR}/webhook-configuration-active.yaml"

echo "Successfully generated certs and created Secret: ${SECRET}"
echo "Patched configuration written to: ${DIR}/webhook-configuration-active.yaml"
echo "You can now run: kubectl apply -f ${DIR}/deployment.yaml -f ${DIR}/webhook-configuration-active.yaml"
