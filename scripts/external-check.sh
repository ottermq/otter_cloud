#!/bin/sh
# Checks the platform from outside, as a customer sees it (#107, M5's
# completion criteria): run it from a network other than the node's.
#
#   OTTER_CHECK_EMAIL=… OTTER_CHECK_PASSWORD_FILE=… \
#     scripts/external-check.sh [-f flavor] [-w seconds] [-d days] [-p port ...] <portal URL>
#
#   -f  the flavor to create (default db-small)
#   -w  how long a deleted database's route may outlive it (default 30: the
#       edge polls every 10 s)
#   -d  warn when a certificate has fewer days left than this (default 21;
#       Traefik renews at 30)
#   -p  a port the edge must keep closed; repeatable (default 22 80 6432
#       8080 8081 9100 9101 9102). Leave 22 out from an address on the
#       edge's SSH allowlist.
#
# The account is a customer's with an active subscription, kept for this
# check. The script:
#   1. probes the edge: the closed ports, HTTPS only, internal paths not
#      routed, and both certificates' expiry;
#   2. logs in, creates an ephemeral database, and waits for it to run;
#   3. connects with psql and sslmode=verify-full within 60 s of running,
#      by SSLRequest and, with psql 17 or later, by direct TLS;
#   4. deletes it, and checks its route is gone within -w seconds.
# Run the node's isolation check alongside it (scripts/isolation-check.sh).
#
# Needs curl, jq, nc, openssl, and psql 16 or later with the system's CA
# store (PSQL names another psql, e.g. a wrapper running one in a
# container). Exits 1 if any check fails; a certificate near its expiry
# is a warning.
set -u

flavor=db-small route_wait=30 cert_days=21 ports=
while getopts f:w:d:p: opt; do
	case $opt in
	f) flavor=$OPTARG ;;
	w) route_wait=$OPTARG ;;
	d) cert_days=$OPTARG ;;
	p) ports="$ports $OPTARG" ;;
	*) exit 2 ;;
	esac
done
shift $((OPTIND - 1))
if [ $# -ne 1 ] || [ -z "${OTTER_CHECK_EMAIL:-}" ] || [ ! -r "${OTTER_CHECK_PASSWORD_FILE:-}" ]; then
	echo "usage: OTTER_CHECK_EMAIL=… OTTER_CHECK_PASSWORD_FILE=… $0 [-f flavor] [-w seconds] [-d days] [-p port ...] <portal URL>" >&2
	exit 2
fi
portal=${1%/}
portal_host=${portal#https://}
portal_host=${portal_host%%/*}
ports=${ports:-22 80 6432 8080 8081 9100 9101 9102}
psql=${PSQL:-psql}

failed=0
ok() { echo "ok    $*"; }
bad() { echo "FAIL  $*"; failed=1; }
warn() { echo "warn  $*"; }

work=$(mktemp -d)
id='' name=''
cleanup() {
	# A database left behind by a failed run is deleted; the account's
	# session ends either way.
	if [ -n "$id" ]; then
		api DELETE "/api/instances/$id" "{\"confirm\":\"$name\"}" >/dev/null
	fi
	api POST /api/logout '{}' >/dev/null
	rm -rf "$work"
}
trap cleanup EXIT
trap 'exit 1' INT TERM

# api METHOD PATH [BODY]: the response body on stdout, the status in
# $work/status.
api() {
	curl -sS -m 20 -b "$work/jar" -c "$work/jar" -X "$1" -H 'Content-Type: application/json' \
		${3:+-d "$3"} -o "$work/body" -w '%{http_code}' "$portal$2" >"$work/status" 2>/dev/null
	cat "$work/body" 2>/dev/null
}
status() { cat "$work/status"; }

# routed HOST: whether the edge completes a TLS handshake for HOST on
# 5432. Without a route it drops the connection.
routed() {
	openssl s_client -starttls postgres -connect "$1:5432" -servername "$1" </dev/null >/dev/null 2>&1
}

# expiry LABEL HOST [OPENSSL ARGS]: the certificate HOST is served, and
# whether it outlives -d days.
# Shell variables are global: these names are its own.
expiry() {
	exp_label=$1 exp_addr=$2
	shift 2
	cert=$(openssl s_client -connect "$exp_addr" -servername "${exp_addr%:*}" "$@" </dev/null 2>/dev/null |
		openssl x509 2>/dev/null)
	if [ -z "$cert" ]; then
		bad "$exp_label: no certificate from $exp_addr"
	elif ! echo "$cert" | openssl x509 -noout -checkend 0 >/dev/null; then
		bad "$exp_label: certificate expired"
	elif ! echo "$cert" | openssl x509 -noout -checkend $((cert_days * 86400)) >/dev/null; then
		warn "$exp_label: certificate expires $(echo "$cert" | openssl x509 -noout -enddate | cut -d= -f2), within $cert_days days: renewal is late"
	else
		ok "$exp_label: certificate valid until $(echo "$cert" | openssl x509 -noout -enddate | cut -d= -f2)"
	fi
}

echo "== the edge"
for port in $ports; do
	if nc -z -w 5 "$portal_host" "$port" 2>/dev/null; then
		bad "port $port is open"
	else
		ok "port $port closed"
	fi
done
for port in 443 5432; do
	nc -z -w 5 "$portal_host" "$port" 2>/dev/null && ok "port $port open" || bad "port $port is closed"
done
code=$(curl -sS -m 10 -o /dev/null -w '%{http_code}' "$portal/healthz" 2>/dev/null)
[ "$code" = 200 ] && ok "portal answers" || bad "portal /healthz: $code"
curl -sSI -m 10 "$portal/healthz" 2>/dev/null | grep -qi '^strict-transport-security:' &&
	ok "HSTS" || bad "no HSTS header"
for p in /metrics /traefik; do
	code=$(curl -sS -m 10 -o /dev/null -w '%{http_code}' "$portal$p" 2>/dev/null)
	[ "$code" = 404 ] && ok "$p not routed" || bad "$p: $code, want 404"
done
expiry portal "$portal_host:443"

echo "== a database"
api POST /api/login "$(jq -n --arg e "$OTTER_CHECK_EMAIL" --rawfile p "$OTTER_CHECK_PASSWORD_FILE" \
	'{email: $e, password: ($p | rtrimstr("\n"))}')" >/dev/null
if [ "$(status)" != 200 ]; then
	bad "login: $(status)"
	exit 1
fi
name=check-$(date +%s)
created=$(api POST /api/instances "{\"name\":\"$name\",\"flavor\":\"$flavor\",\"ephemeral\":true}")
if [ "$(status)" != 202 ]; then
	bad "create: $(status) $created"
	name=
	exit 1
fi
id=$(echo "$created" | jq -r .instance.id)
echo "$created" | jq -r .password >"$work/password"
ok "created $name"

deadline=$(($(date +%s) + 600))
while :; do
	view=$(api GET "/api/instances/$id")
	st=$(echo "$view" | jq -r .status)
	case $st in
	running) break ;;
	failed)
		bad "$name failed"
		exit 1
		;;
	esac
	if [ "$(date +%s)" -gt "$deadline" ]; then
		bad "$name not running after 10 minutes (status $st)"
		exit 1
	fi
	sleep 2
done
running_at=$(date +%s)
host=$(echo "$view" | jq -r .connection.host)
port=$(echo "$view" | jq -r .connection.port)
db=$(echo "$view" | jq -r .connection.database)
user=$(echo "$view" | jq -r .connection.user)
ok "$name running: $host:$port"

# connect [OPTIONS]: psql with verify-full against the system's CAs.
connect() {
	PGPASSWORD=$(cat "$work/password") PGCONNECT_TIMEOUT=10 $psql \
		"host=$host port=$port dbname=$db user=$user sslmode=verify-full sslrootcert=system $*" \
		-XAtqc 'select 1' 2>"$work/psql.err"
}
# connected [OPTIONS]: whether psql connects within 60 s of running.
connected() {
	until [ "$(connect "$@")" = 1 ]; do
		[ $(($(date +%s) - running_at)) -gt 60 ] && return 1
		sleep 2
	done
}
major=$($psql --version | sed -n 's/^psql (PostgreSQL) \([0-9]*\).*/\1/p')
if [ "${major:-0}" -ge 17 ]; then
	modes="postgres direct"
else
	modes=postgres
fi
for mode in $modes; do
	opt=
	[ "${major:-0}" -ge 17 ] && opt="sslnegotiation=$mode"
	if connected $opt; then
		ok "psql $major ($mode): connected $(($(date +%s) - running_at)) s after running"
	else
		bad "psql $major ($mode): no connection within 60 s of running: $(cat "$work/psql.err")"
	fi
done

# Its route is there once psql is in (the edge polls every 10 s), and must
# be before its going away means anything.
if routed "$host"; then
	ok "$host routed"
else
	bad "$host not routed while running"
fi
expiry databases "$host:$port" -starttls postgres
api DELETE "/api/instances/$id" "{\"confirm\":\"$name\"}" >/dev/null
if [ "$(status)" != 202 ]; then
	bad "delete: $(status) $(cat "$work/body")"
	exit 1
fi
deleted_at=$(date +%s)
id=
while routed "$host"; do
	if [ $(($(date +%s) - deleted_at)) -gt "$route_wait" ]; then
		bad "$host still routed $route_wait s after the delete"
		break
	fi
	sleep 2
done
routed "$host" || ok "route gone $(($(date +%s) - deleted_at)) s after the delete"

if [ "$failed" = 0 ]; then
	echo "external-check: all checks passed"
else
	echo "external-check: some checks failed"
fi
exit "$failed"
