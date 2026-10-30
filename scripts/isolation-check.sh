#!/bin/sh
# Probes what a node's PF policy promises (security.md R1, R16; runbook
# Part F): a tenant jail reaches nothing, and only the node host reaches
# it. Run on the node host as an operator with doas, after every PF change
# (#24); in M7 on a schedule (#122).
#
#   scripts/isolation-check.sh [-j jail] [-a jail-ip] [-g gateway]
#       [-t host:port ...] [-n other-jail ...] [-p port ...] [-c]
#
#   -j  the tenant jail probed from (default otc-pgc1)
#   -a  its address (default 10.64.0.10)
#   -g  the node host's address on the tenant network (default 10.64.0.1)
#   -t  a target the tenant jail must NOT reach, host:port, where something
#       listens (a closed port proves nothing); repeatable. The node's own:
#       its LAN address, the LAN gateway, other jails' services, the
#       control plane. The internet and the host's sshd on the tenant
#       network are always probed.
#   -n  a non-Otter jail on the node that must NOT reach the tenant
#       jail (R16); repeatable
#   -p  a tenant port the node host must reach (default 5432 and 6432)
#   -c  also the control plane of the minimum profile (#58): otter-api
#       reaches only otter-db's :5432, otter-db reaches nothing, the node
#       host reaches both, and neither the tenant jail nor a -n jail
#       reaches either. The -t targets are probed from both too.
#
# The defaults are architecture.md §7's first node. The node's private
# values (its LAN, its other jails) are parameters only: they stay out of
# this repo. Exits 1 if any probe gives the wrong answer.
set -u

jail=otc-pgc1 addr=10.64.0.10 gw=10.64.0.1 targets= others= ports= control=
# The control plane's jails and the host's address on their network
# (architecture.md §7, deploy/node/pf.conf).
api=otter-api api_addr=10.64.1.20 db=otter-db db_addr=10.64.1.10 platform_gw=10.64.1.1
while getopts j:a:g:t:n:p:c opt; do
	case $opt in
	j) jail=$OPTARG ;;
	a) addr=$OPTARG ;;
	g) gw=$OPTARG ;;
	t) targets="$targets $OPTARG" ;;
	n) others="$others $OPTARG" ;;
	p) ports="$ports $OPTARG" ;;
	c) control=1 ;;
	*) exit 2 ;;
	esac
done
ports=${ports:-5432 6432}

bad=0
# check <want: open|closed> <what> <command...>
check() {
	want=$1 what=$2
	shift 2
	if "$@" >/dev/null 2>&1; then got=open; else got=closed; fi
	if [ "$got" = "$want" ]; then
		printf 'ok    %-7s %s\n' "$got" "$what"
	else
		printf 'FAIL  %-7s %s (want %s)\n' "$got" "$what" "$want"
		bad=1
	fi
}
# from <jail> <host> <port>: a TCP connect from inside a jail, 3 s.
from() { doas jexec "$1" nc -z -w 3 "$2" "$3"; }

for j in $jail ${control:+$api $db}; do
	jls -j "$j" jid >/dev/null 2>&1 || {
		echo "isolation-check: no running jail $j" >&2
		exit 2
	}
done

# blocked <table>: packets the policy's "block ... from <table>" rule has
# dropped.
blocked() {
	doas pfctl -v -s rules 2>/dev/null | awk -v rule="block drop log quick from <$1>" '$0 == rule || index($0, rule " ") == 1 { getline; for (i = 1; i < NF; i++) if ($i == "Packets:") print $(i + 1) }'
}
before=$(blocked tenants)
[ -n "$control" ] && pbefore=$(blocked platform)

echo "== $jail ($addr) reaches nothing"
# The control: a probe from the jail can succeed, so "closed" below is PF's.
check open "$jail → 127.0.0.1:5432 (control)" from "$jail" 127.0.0.1 5432
# A TCP connect each: ICMP and DNS would fail inside a jail without raw
# sockets or a resolver whatever PF did, so they prove nothing.
for t in 1.1.1.1:443 9.9.9.9:53 $gw:22 $targets; do
	check closed "$jail → $t" from "$jail" "${t%:*}" "${t##*:}"
done

echo "== the node host reaches $jail"
for p in $ports; do
	check open "host → $addr:$p" nc -z -w 3 "$addr" "$p"
done

if [ -n "$others" ]; then
	echo "== non-Otter jails don't reach $jail (R16)"
	for j in $others; do
		for p in $ports; do
			check closed "$j → $addr:$p" from "$j" "$addr" "$p"
		done
	done
fi

if [ -n "$control" ]; then
	echo "== $api ($api_addr) reaches only $db:5432"
	check open "$api → $db_addr:5432 (control)" from "$api" "$db_addr" 5432
	for t in 1.1.1.1:443 9.9.9.9:53 $platform_gw:22 $addr:5432 $addr:6432 $targets; do
		check closed "$api → $t" from "$api" "${t%:*}" "${t##*:}"
	done
	echo "== $db ($db_addr) reaches nothing"
	check open "$db → 127.0.0.1:5432 (control)" from "$db" 127.0.0.1 5432
	for t in 1.1.1.1:443 $platform_gw:22 $api_addr:8080 $addr:5432 $targets; do
		check closed "$db → $t" from "$db" "${t%:*}" "${t##*:}"
	done
	echo "== the node host reaches both"
	check open "host → $db_addr:5432" nc -z -w 3 "$db_addr" 5432
	check open "host → $api_addr:8080" nc -z -w 3 "$api_addr" 8080
	echo "== nothing else reaches them"
	for j in $jail $others; do
		check closed "$j → $db_addr:5432" from "$j" "$db_addr" 5432
		check closed "$j → $api_addr:8080" from "$j" "$api_addr" 8080
	done
fi

echo "== PF dropped them"
# Without it, PF never sees traffic between members of one bridge (runbook D2).
check open "net.link.bridge.pfil_member is 1" test "$(sysctl -n net.link.bridge.pfil_member 2>/dev/null)" = 1
after=$(blocked tenants)
check open "the tenant block rule counted packets (${before:-?} → ${after:-?})" \
	test "${after:-0}" -gt "${before:-0}"
if [ -n "$control" ]; then
	pafter=$(blocked platform)
	check open "the platform block rule counted packets (${pbefore:-?} → ${pafter:-?})" \
		test "${pafter:-0}" -gt "${pbefore:-0}"
fi

[ $bad = 0 ] && echo "isolation-check: all probes as expected" || echo "isolation-check: FAILED" >&2
exit $bad
