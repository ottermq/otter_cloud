#!/bin/sh
# Probes what a node's PF policy promises the edge (pf.conf, "The tunnel";
# security.md, beta checklist): over wg0 the edge reaches PgBouncer and the
# api's two ports, and nothing else on the node. Run on the edge, as any
# user, after every PF change on a node and every change to wg0 (#24);
# scripts/isolation-check.sh is the node's half.
#
#   scripts/tunnel-check.sh [-n node-tunnel-ip] [-b pgbouncer] [-o host:port ...]
#       [-t host:port ...]
#
#   -n  the node's tunnel address (default 10.62.0.2)
#   -b  a cluster's PgBouncer address (default 10.64.0.10)
#   -o  a target the edge must reach, host:port; repeatable. Default: the
#       PgBouncer's :6432 and otter-api's :8080 and :8081 (the minimum
#       profile, #58)
#   -t  another target the edge must NOT reach, host:port, where something
#       listens (a closed port proves nothing; check from the node host);
#       repeatable. Always probed: the node's sshd on its tunnel, tenant and
#       platform addresses, the cluster's PostgreSQL and the platform
#       database.
#
# Every target must be routed over wg0, or its probe proves nothing about
# the node: a target outside the node's AllowedIPs here (its LAN, say)
# needs a route and an AllowedIPs entry added for the run, which is what a
# compromised edge could do. The defaults are architecture.md §7's first
# node. Exits 1 if any probe gives the wrong answer.
set -u

node=10.62.0.2 pgb=10.64.0.10 reach= targets=
while getopts n:b:o:t: opt; do
	case $opt in
	n) node=$OPTARG ;;
	b) pgb=$OPTARG ;;
	o) reach="$reach $OPTARG" ;;
	t) targets="$targets $OPTARG" ;;
	*) exit 2 ;;
	esac
done
reach=${reach:-$pgb:6432 10.64.1.20:8080 10.64.1.20:8081}
# The tenant and platform gateways are the node host (pf.conf).
unreach="$node:22 10.64.0.1:22 10.64.1.1:22 $pgb:5432 10.64.1.10:5432 $targets"

bad=0
# check <want: open|closed> <host:port>: a TCP connect over wg0, 3 s.
check() {
	host=${2%:*} port=${2##*:}
	if ! ip -o route get "$host" 2>/dev/null | grep -q ' dev wg0 '; then
		printf 'FAIL  %-7s %s (not routed over wg0)\n' - "$2"
		bad=1
		return
	fi
	if nc -z -w 3 "$host" "$port" >/dev/null 2>&1; then got=open; else got=closed; fi
	if [ "$got" = "$1" ]; then
		printf 'ok    %-7s edge → %s\n' "$got" "$2"
	else
		printf 'FAIL  %-7s edge → %s (want %s)\n' "$got" "$2" "$1"
		bad=1
	fi
}

ip link show wg0 >/dev/null 2>&1 || {
	echo "tunnel-check: no wg0 here; run it on the edge" >&2
	exit 2
}

# The controls: the tunnel is up and the probes can succeed, so "closed"
# below is the node's PF.
echo "== the edge reaches what the node passes"
for t in $reach; do check open "$t"; done
echo "== and nothing else on the node"
for t in $unreach; do check closed "$t"; done

[ $bad = 0 ] && echo "tunnel-check: all probes as expected" || echo "tunnel-check: FAILED" >&2
exit $bad
