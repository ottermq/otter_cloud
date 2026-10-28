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
# Tenants come through PgBouncer on the jail's loopback there; here, straight
# in, so the tests can log in as one.
cat > "$PGDATA/pg_hba.conf" <<HBA
local   all  all                       trust
hostssl all       otter_admin     0.0.0.0/0     cert
hostssl sameuser  all             0.0.0.0/0     scram-sha-256
host    sameuser  all             127.0.0.1/32  scram-sha-256
host    postgres  pgbouncer_auth  127.0.0.1/32  scram-sha-256
host    all       all             0.0.0.0/0     reject
HBA
# A password too, so the test can show it is not enough.
psql -v ON_ERROR_STOP=1 -U postgres \
	-c "CREATE ROLE otter_admin LOGIN CREATEROLE CREATEDB PASSWORD 'otter-admin-password'" \
	-c "GRANT SET ON PARAMETER temp_file_limit TO otter_admin" \
	-c "REVOKE CONNECT ON DATABASE postgres, template1 FROM PUBLIC" \
	-c "GRANT CONNECT ON DATABASE postgres TO otter_admin" \
	-c "CREATE ROLE pgbouncer_auth LOGIN PASSWORD 'pgbouncer-auth-password'" \
	-c "GRANT CONNECT ON DATABASE postgres TO pgbouncer_auth"
# PgBouncer's lookup of SCRAM secrets, as on a cluster (architecture.md §6).
psql -v ON_ERROR_STOP=1 -U postgres -d postgres <<'SQL'
CREATE SCHEMA pgbouncer AUTHORIZATION postgres;
CREATE FUNCTION pgbouncer.get_auth(p_usename text)
  RETURNS TABLE (username text, password text)
  LANGUAGE sql SECURITY DEFINER SET search_path = pg_catalog AS
  $$ SELECT usename::text, passwd::text FROM pg_shadow WHERE usename = p_usename AND NOT usesuper $$;
REVOKE ALL ON FUNCTION pgbouncer.get_auth(text) FROM PUBLIC;
GRANT USAGE ON SCHEMA pgbouncer TO pgbouncer_auth;
GRANT EXECUTE ON FUNCTION pgbouncer.get_auth(text) TO pgbouncer_auth;
SQL
INIT
chmod 0755 init/10-cluster.sh

$CONTAINER rm -f "$NAME" >/dev/null 2>&1 || true
$CONTAINER run -d --rm --name "$NAME" -e POSTGRES_PASSWORD=unused -e POSTGRES_HOST_AUTH_METHOD=reject \
	-v "$DIR:/certs:ro" -v "$DIR/init:/docker-entrypoint-initdb.d:ro" \
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

# PgBouncer next to it, as in a cluster jail. Its main file is the
# bootstrap's; the worker writes only the included databases file (#52).
# The admin console's password is in a file only root reads, which the
# worker never sees: it runs psql in the jail through JailDeck.
$CONTAINER exec -u root -i "$NAME" sh -eu <<'POOL' >/dev/null
apt-get update -qq
DEBIAN_FRONTEND=noninteractive apt-get install -y -qq pgbouncer >/dev/null
service pgbouncer stop >/dev/null 2>&1 || true
mkdir -p /etc/pgbouncer/pgbouncer.d
install -o postgres -m 0600 /certs/server.key /etc/pgbouncer/pgbouncer.key
install -o postgres -m 0644 /certs/server.crt /etc/pgbouncer/pgbouncer.crt
cat > /etc/pgbouncer/pgbouncer.users <<USERS
"pgbouncer_auth" "pgbouncer-auth-password"
"pgbouncer_admin" "pgbouncer-admin-password"
USERS
chown postgres /etc/pgbouncer/pgbouncer.users && chmod 0600 /etc/pgbouncer/pgbouncer.users
printf '127.0.0.1:6432:pgbouncer:pgbouncer_admin:pgbouncer-admin-password\n' > /root/.pgpass && chmod 0600 /root/.pgpass
: > /etc/pgbouncer/pgbouncer.d/otter-databases.ini
cat > /etc/pgbouncer/pgbouncer.ini <<INI
[databases]
; The auth database must have an entry. Tenants routed here are refused by
; PostgreSQL: pg_hba is sameuser, and CONNECT on postgres is revoked.
postgres = host=127.0.0.1 port=5432 dbname=postgres

%include /etc/pgbouncer/pgbouncer.d/otter-databases.ini

[pgbouncer]
listen_addr = 0.0.0.0
listen_port = 6432
unix_socket_dir =
auth_type = scram-sha-256
auth_file = /etc/pgbouncer/pgbouncer.users
auth_user = pgbouncer_auth
auth_dbname = postgres
auth_query = SELECT username, password FROM pgbouncer.get_auth(\$1)
admin_users = pgbouncer_admin
pool_mode = transaction
max_prepared_statements = 200
client_tls_sslmode = require
client_tls_key_file = /etc/pgbouncer/pgbouncer.key
client_tls_cert_file = /etc/pgbouncer/pgbouncer.crt
client_tls_ca_file = /etc/pgbouncer/pgbouncer.crt
logfile = /var/log/postgresql/pgbouncer.log
pidfile = /var/run/postgresql/pgbouncer.pid
INI
su postgres -s /bin/sh -c 'pgbouncer -d /etc/pgbouncer/pgbouncer.ini'
POOL
echo "export OTTER_TEST_CLUSTER_DIR='$DIR'"
