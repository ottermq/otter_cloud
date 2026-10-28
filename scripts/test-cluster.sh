#!/bin/sh
# Starts a throwaway PostgreSQL 18 set up like a shared cluster after
# otter-pg-bootstrap (architecture.md §6): TLS on, otter_admin only by client
# certificate, everything else refused. For the tests of internal/postgres.
#
#   scripts/test-cluster.sh <dir> [port]
#
# <dir> receives the certificates the tests use; export
# OTTER_TEST_CLUSTER_DIR=<dir> to run them. Every key here is a test key,
# made fresh each run, and trusted by nothing else.
set -eu

DIR=$1
PORT=${2:-55433}
NAME=${OTTER_TEST_CLUSTER_NAME:-otter-test-cluster}
CONTAINER=${CONTAINER:-docker}

rm -rf "$DIR"
mkdir -p "$DIR/init"
cd "$DIR"

# The admin CA, which signs node client certificates; and a stranger CA,
# whose certificate for otter_admin must be refused.
for ca in ca stranger; do
	openssl req -x509 -new -nodes -newkey ec -pkeyopt ec_paramgen_curve:P-256 -days 2 \
		-subj "/CN=otter test $ca" -keyout "$ca.key" -out "$ca.crt" 2>/dev/null
done
client() { # name ca cn
	openssl req -new -nodes -newkey ec -pkeyopt ec_paramgen_curve:P-256 -subj "/CN=$3" \
		-keyout "$1.key" -out "$1.csr" 2>/dev/null
	openssl x509 -req -in "$1.csr" -CA "$2.crt" -CAkey "$2.key" -CAcreateserial -days 2 -out "$1.crt" 2>/dev/null
	rm "$1.csr"
}
client admin ca otter_admin
client stranger-admin stranger otter_admin
client tenant ca otc_0123456789ab

# The cluster's own self-signed certificate, as the bootstrap makes one.
openssl req -x509 -new -nodes -newkey ec -pkeyopt ec_paramgen_curve:P-256 -days 2 \
	-subj "/CN=otc-pg-test" -keyout server.key -out server.crt 2>/dev/null
chmod 0644 ./*.key ./*.crt

cat > init/10-cluster.sh <<'INIT'
#!/bin/sh
set -eu
cp /certs/server.crt /certs/server.key /certs/ca.crt "$PGDATA/"
chmod 0600 "$PGDATA/server.key"
cat >> "$PGDATA/postgresql.conf" <<CONF
ssl = on
ssl_cert_file = 'server.crt'
ssl_key_file = 'server.key'
ssl_ca_file = 'ca.crt'
CONF
# The node host's address is the docker gateway here; on a node it is one /32.
cat > "$PGDATA/pg_hba.conf" <<HBA
local   all  all                       trust
hostssl all  otter_admin  0.0.0.0/0    cert
host    all  all          0.0.0.0/0    reject
HBA
# A password too, so the test can show it is not enough.
psql -v ON_ERROR_STOP=1 -U postgres -c "CREATE ROLE otter_admin LOGIN CREATEROLE CREATEDB PASSWORD 'otter-admin-password'"
INIT
chmod 0755 init/10-cluster.sh

$CONTAINER rm -f "$NAME" >/dev/null 2>&1 || true
$CONTAINER run -d --rm --name "$NAME" -e POSTGRES_PASSWORD=unused -e POSTGRES_HOST_AUTH_METHOD=reject \
	-v "$DIR:/certs:ro" -v "$DIR/init:/docker-entrypoint-initdb.d:ro" \
	-p "127.0.0.1:$PORT:5432" postgres:18 >/dev/null

echo "127.0.0.1" > host
echo "$PORT" > port
for _ in $(seq 60); do
	if $CONTAINER exec "$NAME" pg_isready -q -h 127.0.0.1 2>/dev/null; then
		echo "export OTTER_TEST_CLUSTER_DIR='$DIR'"
		exit 0
	fi
	sleep 1
done
echo "test-cluster: PostgreSQL did not come up" >&2
$CONTAINER logs "$NAME" >&2
exit 1
