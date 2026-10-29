#!/bin/sh
# Starts a throwaway PostgreSQL 18 set up like a shared cluster after
# otter-pg-bootstrap (architecture.md §6): TLS on, otter_admin only by client
# certificate, everything else refused. For the tests of internal/postgres.
#
#   scripts/test-cluster.sh <dir> [port] [pool-port]
#
# <dir> receives the certificates the tests use; export
# OTTER_TEST_CLUSTER_DIR=<dir> to run them. Every key here is a test key,
# made fresh each run, and trusted by nothing else.
set -eu

DIR=$1
BOOTSTRAP=$(cd "$(dirname "$0")/../deploy/cluster" && pwd)
PORT=${2:-55433}
POOL_PORT=${3:-55434}
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
chmod 0644 ./*.key ./*.crt

# The cluster is set up by the node's own bootstrap (deploy/cluster), in two
# halves: PostgreSQL in the image's first start, PgBouncer once it is up.
# The node host's address is the docker gateway here; on a node it is one
# /32. Tenants come through PgBouncer on the jail's loopback there; here,
# straight in too, so the tests can log in as one.
cat > init/10-cluster.sh <<'INIT'
#!/bin/sh
set -eu
OTTER_HBA_EXTRA='hostssl sameuser  all             0.0.0.0/0     scram-sha-256' \
	/otter/otter-pg-bootstrap -n 0.0.0.0/0 -l "$(awk 'END { print $1 }' /etc/hosts)" \
	-N otc-pg-test -a /certs/ca.crt postgres
# A password too, so the test can show it is not enough.
psql -X -q -v ON_ERROR_STOP=1 -U postgres -c "ALTER ROLE otter_admin PASSWORD 'otter-admin-password'"
INIT
chmod 0755 init/10-cluster.sh

$CONTAINER rm -f "$NAME" >/dev/null 2>&1 || true
$CONTAINER run -d --rm --name "$NAME" -e POSTGRES_PASSWORD=unused -e POSTGRES_HOST_AUTH_METHOD=reject \
	-e PGDATA=/var/db/postgres/data18 \
	-v "$DIR:/certs:ro" -v "$DIR/init:/docker-entrypoint-initdb.d:ro" -v "$BOOTSTRAP:/otter:ro" \
	-p "127.0.0.1:$PORT:5432" -p "127.0.0.1:$POOL_PORT:6432" postgres:18 >/dev/null

echo "127.0.0.1" > host
echo "$PORT" > port
echo "$POOL_PORT" > pool-port
echo "$NAME" > container
up=
for _ in $(seq 60); do
	if $CONTAINER exec "$NAME" pg_isready -q -h 127.0.0.1 2>/dev/null; then
		up=1
		break
	fi
	sleep 1
done
if [ -z "$up" ]; then
	echo "test-cluster: PostgreSQL did not come up" >&2
	$CONTAINER logs "$NAME" >&2
	exit 1
fi

# PgBouncer next to it, as in a cluster jail, set up by the bootstrap's
# second half. Debian's package runs it as postgres, from its own run and
# log directories; the files the worker reads or writes are where a FreeBSD
# cluster jail has them.
$CONTAINER exec -u root -i "$NAME" sh -eu <<'POOL' >/dev/null
apt-get update -qq
DEBIAN_FRONTEND=noninteractive apt-get install -y -qq pgbouncer >/dev/null
service pgbouncer stop >/dev/null 2>&1 || true
PGB_USER=postgres PGB_RUN=/var/run/postgresql PGB_LOG=/var/log/postgresql \
	/otter/otter-pg-bootstrap -n 0.0.0.0/0 -l "$(awk 'END { print $1 }' /etc/hosts)" pgbouncer
su postgres -s /bin/sh -c 'pgbouncer -d /usr/local/etc/pgbouncer.ini'
POOL
# The cluster's certificate, made by the bootstrap, for the tests to pin.
$CONTAINER cp "$NAME:/var/db/postgres/data18/server.crt" server.crt
echo "export OTTER_TEST_CLUSTER_DIR='$DIR'"
