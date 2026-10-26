#!/bin/sh
# Tablespace-per-tenant storage quota POC (docs/poc-tablespace-quota.md, Q22).
#
# Builds one throwaway jail, "poc-ts", with no network, cloned from a release
# template, with PostgreSQL 18 and three tenant databases t1, t2, t3, each on
# its own ZFS dataset with a refquota. Then runs the POC's tests one at a time.
#
# It touches nothing but datasets and a jail named poc-ts*: no jail.conf, no
# jail_list, no JailDeck configuration. Run as root on the node:
#
#   POOL=<pool> TEMPLATE=<pool>/jails/templates/<release>@<snap> sh poc-tablespace-quota.sh <command>
#
# Commands, in the usual order:
#   preflight          read-only checks; changes nothing
#   setup              datasets, jail, packages, then setup-db
#   setup-db           initdb, t1..t3, load tables (resumable on its own)
#   load-start         pgbench on t2 and t3 in the background
#   q1 .. q10          one test each; q2-q4 are meant to be run three times
#   reset-t1           drop and recreate t1's data (between repeated runs)
#   status             quick state: datasets, PostgreSQL, pg_wal, load
#   load-stop
#   teardown           remove everything this script created (CONFIRM=poc-ts)
#
# Every command appends to $RESULTS/log.txt, with the commands it ran and what
# they printed. Hand that directory back for analysis.

set -eu

JAIL=poc-ts
POOL=${POOL:?set POOL to the pool holding <pool>/jails}
TEMPLATE=${TEMPLATE:?set TEMPLATE to a release template snapshot}
ROOT_DS=$POOL/jails/containers/$JAIL
RETAINED_DS=$POOL/jails/retained
QUOTA=${QUOTA:-200M}       # t1's refquota, in M
LOAD_QUOTA=${LOAD_QUOTA:-2G}  # t2/t3: room for pgbench_history to grow during the soak
BACKSTOP=${BACKSTOP:-4G}   # refquota on data (PGDATA + pg_wal)
SOAK_MIN=${SOAK_MIN:-30}   # q3 duration
RESULTS=${RESULTS:-/var/tmp/poc-ts-results}
PGVER=18

mkdir -p "$RESULTS"
LOG=$RESULTS/log.txt

say() { printf '%s %s\n' "$(date '+%F %T')" "$*" | tee -a "$LOG"; }
# run: print a command, run it, keep its output and exit code in the log.
run() {
	say "\$ $*"
	set +e
	"$@" >>"$LOG" 2>&1
	rc=$?
	set -e
	[ $rc -eq 0 ] || say "exit $rc"
	return $rc
}
# try: like run, but a failure is an observation, not an abort.
try() { run "$@" || true; }

mnt() { zfs get -H -o value mountpoint "$1"; }
JROOT() { mnt "$ROOT_DS"; }

jx() { jexec -U postgres "$JAIL" env PGHOST=/tmp "$@"; }
psql_db() { db=$1; shift; jx psql -X -v ON_ERROR_STOP=1 -d "$db" "$@"; }
sql() { db=$1; shift; try psql_db "$db" -c "$*"; }
pgctl() { jx pg_ctl -D /data/pgdata "$@"; }

jail_running() { jls -j "$JAIL" jid >/dev/null 2>&1; }
pg_running() { jail_running && jx pg_ctl -D /data/pgdata status >/dev/null 2>&1; }
# A postmaster in crash recovery is "running" but refuses every connection.
pg_accepting() { jail_running && jx pg_isready -q >/dev/null 2>&1; }

space() {
	say "--- space"
	zfs list -p -H -o name,referenced,available,refquota,compressratio -r "$ROOT_DS" | tee -a "$LOG"
	if jail_running; then
		say "pg_wal bytes: $(du -sk "$(JROOT)/data/pgdata/pg_wal" | awk '{print $1*1024}')"
	fi
}

alive() {
	say "--- is the cluster alive?"
	if pg_accepting; then
		say "postmaster: accepting connections"
		for db in t2 t3; do
			if psql_db "$db" -Atc 'select 1' >/dev/null 2>&1; then say "$db: answers"; else say "$db: DOES NOT ANSWER"; fi
		done
	else
		say "postmaster: NOT ACCEPTING CONNECTIONS (down or in recovery)"
	fi
}

logtail() {
	say "--- postgres log (last ${1:-30} lines)"
	tail -n "${1:-30}" "$(JROOT)/data/postgres.log" | tee -a "$LOG"
}

checkpoint_stats() {
	sql postgres "select num_timed, num_requested, num_done, buffers_written, stats_reset from pg_stat_checkpointer"
}

# fill t1 until a write fails. Random data, because compression is on and
# repeated text would compress to almost nothing.
fill_t1() {
	sql t1 "create table if not exists f (id bigserial primary key, x bytea) ; alter table f set (autovacuum_enabled = false)"
	i=0
	while [ $i -lt 100 ]; do
		i=$((i + 1))
		if ! psql_db t1 -c "insert into f (x) select gen_random_bytes(1000) from generate_series(1, 10000)" >>"$LOG" 2>&1; then
			say "t1 insert batch $i failed (expected once full); last error above"
			return 0
		fi
	done
	say "t1 never filled after $i batches: QUOTA too large?"
	return 1
}

preflight() {
	say "=== preflight"
	[ "$(id -u)" -eq 0 ] || { say "must run as root"; exit 1; }
	run zfs list -H -o name "$TEMPLATE"
	if zfs list -H -o name "$ROOT_DS" >/dev/null 2>&1; then say "$ROOT_DS already exists"; fi
	if jail_running; then say "jail $JAIL already running"; fi
	run sysctl kern.racct.enable
	run zfs get -H -o property,value compression,recordsize,atime "$POOL/jails/containers"
	run zfs list -H -o name,available "$POOL"
	say "preflight done"
}

setup() {
	say "=== setup"
	if zfs list -H -o name "$ROOT_DS" >/dev/null 2>&1; then say "$ROOT_DS exists; run teardown first"; exit 1; fi
	run zfs clone "$TEMPLATE" "$ROOT_DS"
	# Children inherit their mountpoint (<jail root>/<name>) and use flat names:
	# the conventions the roadmap set for JailDeck child datasets.
	run zfs create -o refquota="$BACKSTOP" -o recordsize=16K "$ROOT_DS/data"
	run zfs create -o refquota="$QUOTA" -o recordsize=16K "$ROOT_DS/ts-t1"
	for t in t2 t3; do
		run zfs create -o refquota="$LOAD_QUOTA" -o recordsize=16K "$ROOT_DS/ts-$t"
	done
	r=$(JROOT)
	run cp /etc/resolv.conf "$r/etc/resolv.conf"
	run pkg -c "$r" install -y "postgresql${PGVER}-server" "postgresql${PGVER}-contrib"
	run jail -c name="$JAIL" path="$r" host.hostname="$JAIL" \
		ip4=disable ip6=disable persist mount.devfs devfs_ruleset=4 \
		sysvshm=new sysvsem=new sysvmsg=new
	setup_db
}

# The database half of setup, separate so a failure here can be resumed
# without re-installing packages.
setup_db() {
	say "=== setup-db"
	r=$(JROOT)
	# postgres exists only in the jail's passwd, so chown from inside it.
	run jexec "$JAIL" chown postgres:postgres /data /ts-t1 /ts-t2 /ts-t3
	run jx initdb -D /data/pgdata -E UTF8 --locale=C --auth-local=trust --auth-host=reject
	cat >>"$r/data/pgdata/postgresql.conf" <<-EOF
		# poc-ts
		listen_addresses = ''
		log_destination = 'stderr'   # the package default is syslog
		unix_socket_directories = '/tmp'
		shared_buffers = 256MB
		max_wal_size = 1GB
		checkpoint_timeout = 5min
		log_checkpoints = on
		log_temp_files = 0
		log_line_prefix = '%m [%p] %d %u '
	EOF
	run pgctl -l /data/postgres.log -w start
	for t in t1 t2 t3; do
		sql postgres "create role $t login"
		sql postgres "create tablespace ts_$t owner postgres location '/ts-$t'"
		sql postgres "create database $t owner $t tablespace ts_$t"
		sql postgres "revoke connect on database $t from public; grant connect on database $t to $t"
		sql "$t" "create extension pgcrypto"
	done
	sql postgres "revoke connect on database postgres from public"
	for t in t2 t3; do
		try jx pgbench -i -s 5 -q "$t"
	done
	space
	say "setup-db done"
}

load_start() {
	say "=== load-start"
	for t in t2 t3; do
		# daemon(8), so the load outlives the shell that started it
		jx daemon -f -o /data/pgbench-$t.log pgbench -c 2 -j 1 -T 86400 -P 60 "$t"
	done
	sleep 2
	try pgrep -lf -j "$JAIL" pgbench
}

load_stop() {
	say "=== load-stop"
	try pkill -j "$JAIL" -f pgbench
}

status() {
	say "=== status"
	space
	alive
	try pgrep -lf -j "$JAIL" pgbench
	checkpoint_stats
}

q1() {
	say "=== Q1: fill t1 until writes fail"
	fill_t1
	space
	alive
	logtail 10
}

# Dirty t1's existing pages without extending anything: DELETE only sets xmax
# in place. The checkpoint must then overwrite those blocks, which on ZFS
# needs free space.
dirty_t1() {
	sql t1 "delete from f where id % 7 = $(( $(date +%s) % 7 ))"
	sql t1 "select count(*) from f"   # sets hint bits: more dirty pages
}

q2() {
	say "=== Q2: t1 full, dirty pages, CHECKPOINT"
	sql t1 "update f set x = x where id % 13 = 0"   # expected to fail
	dirty_t1
	checkpoint_stats
	sql postgres "checkpoint"
	checkpoint_stats
	space
	alive
	logtail 30
}

q3() {
	say "=== Q3: keep t1 full for $SOAK_MIN minutes under t2/t3 load"
	sql t1 "alter table f set (autovacuum_enabled = true)"   # real tenants have it
	m=0
	while [ $m -lt "$SOAK_MIN" ]; do
		m=$((m + 1))
		dirty_t1 >/dev/null 2>&1 || true
		fill_t1 >/dev/null 2>&1 || true
		sleep 60
		say "minute $m: pg_wal $(du -sk "$(JROOT)/data/pgdata/pg_wal" | awk '{print $1}') KiB; $(pg_running && echo up || echo DOWN)"
	done
	sql t1 "alter table f set (autovacuum_enabled = false)"
	checkpoint_stats
	space
	alive
	try grep -c -E 'checkpoint complete' "$(JROOT)/data/postgres.log"
	try grep -E 'ERROR|PANIC|FATAL|could not' "$(JROOT)/data/postgres.log"
}

q4() {
	say "=== Q4: crash while t1 is full with dirty pages"
	sql postgres "alter system set checkpoint_timeout = '1h'"
	sql postgres "select pg_reload_conf()"
	sql postgres "checkpoint"    # may fail; then the dirty set is even larger
	fill_t1
	dirty_t1
	say "kill -9 every postgres process in the jail"
	try pkill -9 -j "$JAIL" -x postgres
	sleep 3
	try pgctl -l /data/postgres.log -w -t 300 start
	if pg_accepting; then
		say "Q4 RESULT: recovered WITHOUT raising t1's quota"
	else
		say "Q4 RESULT: recovery did NOT complete; raising t1's refquota to finish it"
		logtail 40
		run zfs set refquota=$(( ${QUOTA%M} * 2 ))M "$ROOT_DS/ts-t1"
		try pgctl -l /data/postgres.log -w -t 300 start
		run zfs set refquota="$QUOTA" "$ROOT_DS/ts-t1"
	fi
	sql postgres "alter system reset checkpoint_timeout"
	sql postgres "select pg_reload_conf()"
	space
	alive
	logtail 40
}

q5() {
	say "=== Q5: free space in t1 and write again"
	say "--- way 1: DELETE + VACUUM"
	sql t1 "delete from f where id > (select max(id) / 2 from f)"
	sql t1 "vacuum f"
	space
	if psql_db t1 -c "insert into f (x) select gen_random_bytes(1000) from generate_series(1, 1000)" >>"$LOG" 2>&1; then
		say "Q5: writes work again after DELETE + VACUUM"
	else
		say "Q5: still failing after DELETE + VACUUM; way 2: DROP TABLE"
		sql t1 "drop table f"
		space
		sql t1 "create table f2 (x bytea); insert into f2 select gen_random_bytes(1000) from generate_series(1, 1000)"
	fi
	alive
}

q6() {
	say "=== Q6: VACUUM FULL and CREATE INDEX near the quota; where temp files go"
	reset_t1
	sql t1 "create table f (id bigserial primary key, x bytea); alter table f set (autovacuum_enabled = false)"
	# about 60% of the quota, so a full rewrite cannot fit
	n=$(( ${QUOTA%M} * 6 / 10 * 1048576 / 1100 ))
	sql t1 "insert into f (x) select gen_random_bytes(1000) from generate_series(1, $n)"
	space
	sql t1 "set maintenance_work_mem = '1MB'; create index on f (x)"
	sql t1 "set work_mem = '64kB'; select x from f order by x desc limit 1" # sort spills to temp
	sql t1 "vacuum full f"
	space
	alive
	try grep -E 'temporary file|pgsql_tmp' "$(JROOT)/data/postgres.log"
	say "temp files should be under pg_tblspc/<ts_t1 oid>; ts_t1 oid:"
	sql postgres "select oid, spcname from pg_tablespace"
}

q7() {
	say "=== Q7: tenant tries to write outside its tablespace"
	try jx psql -X -U t1 -d t1 -c "create table esc (x int) tablespace pg_default"
	try jx psql -X -U t1 -d t1 -c "create table esc2 (x int) tablespace ts_t2"
	try jx psql -X -U t1 -d t1 -c "set temp_tablespaces = pg_default; create temp table tt as select gen_random_bytes(100) x from generate_series(1, 10)"
	try jx psql -X -U t1 -d t1 -c "alter database t1 set tablespace pg_default"
	say "expected: all of the above denied (or temp_tablespaces silently ignored: check the log)"
	try grep -E 'permission denied|tablespace' "$(JROOT)/data/postgres.log"
}

q8() {
	say "=== Q8: dump t1, restore into a fresh database on another tablespace"
	try jx sh -c "pg_dump -Fc -d t1 -f /data/t1.dump"
	sql postgres "create database t1r owner t1 tablespace ts_t3"
	try jx pg_restore --no-tablespaces --no-owner --role=t1 -d t1r /data/t1.dump
	sql t1r "select count(*) from f"
	sql postgres "drop database t1r"
}

q9() {
	say "=== Q9: deletion path: snapshot, drop, move the dataset out, survive jail destroy"
	s="$ROOT_DS/ts-t1@final"
	sql t1 "checkpoint"
	run zfs snapshot "$s"
	sql postgres "drop database t1"
	sql postgres "drop tablespace ts_t1"
	run zfs list -H -o name "$RETAINED_DS"
	run zfs rename -u "$ROOT_DS/ts-t1" "$RETAINED_DS/$JAIL-t1"
	run zfs unmount "$RETAINED_DS/$JAIL-t1"
	run zfs mount "$RETAINED_DS/$JAIL-t1"
	run zfs get -H -o property,value,source mountpoint "$RETAINED_DS/$JAIL-t1"
	run zfs list -H -t snapshot -o name -r "$RETAINED_DS/$JAIL-t1"
	say "the retained dataset must mount under the retained tree, not in the jail;"
	say "teardown later destroys the jail and must leave it in place until CONFIRM"
	say "recreate t1 so later tests still work: run 'setup-t1'"
}

setup_t1() {
	say "=== setup-t1"
	if ! zfs list -H -o name "$ROOT_DS/ts-t1" >/dev/null 2>&1; then
		run zfs create -o refquota="$QUOTA" -o recordsize=16K "$ROOT_DS/ts-t1"
		run jexec "$JAIL" chown postgres:postgres /ts-t1
		sql postgres "create tablespace ts_t1 owner postgres location '/ts-t1'"
		sql postgres "create database t1 owner t1 tablespace ts_t1"
		sql t1 "create extension pgcrypto"
	fi
}

q10() {
	say "=== Q10: how far ZFS lets writes go past refquota"
	ds=$ROOT_DS/q10
	run zfs create -o refquota=100M -o compression=off "$ds"
	set +e
	dd if=/dev/random of="$(mnt "$ds")/fill" bs=1m count=1000 2>>"$LOG"
	set -e
	sync
	written=$(stat -f %z "$(mnt "$ds")/fill")
	ref=$(zfs get -H -p -o value referenced "$ds")
	say "Q10: refquota 104857600 bytes; file size $written; referenced $ref; overshoot $((ref - 104857600)) bytes"
	space
	run zfs destroy "$ds"
}

reset_t1() {
	say "=== reset-t1"
	sql t1 "drop table if exists f; drop table if exists f2"
	space
}

teardown() {
	say "=== teardown"
	[ "${CONFIRM:-}" = "$JAIL" ] || { say "set CONFIRM=$JAIL to remove everything"; exit 1; }
	load_stop || true
	if jail_running; then
		try pgctl -m immediate stop
		try jail -r "$JAIL"
	fi
	# jail -r unmounts devfs only for jails it has a configuration for; this one
	# was created from the command line.
	if mount -p | awk '{print $2}' | grep -qx "$(JROOT)/dev"; then
		try umount "$(JROOT)/dev"
	fi
	# Release any holds a backup tool put on this script's own snapshots.
	for ds in "$ROOT_DS" "$RETAINED_DS/$JAIL-t1"; do
		zfs list -H -o name "$ds" >/dev/null 2>&1 || continue
		zfs list -H -t snapshot -o name -r "$ds" | while read -r snap; do
			zfs holds -H "$snap" | while read -r _ tag _; do try zfs release "$tag" "$snap"; done
		done
		try zfs destroy -r "$ds"
	done
	say "teardown done; results kept in $RESULTS"
}

case "${1:-}" in
preflight) preflight ;;
setup) setup ;;
setup-db) setup_db ;;
setup-t1) setup_t1 ;;
load-start) load_start ;;
load-stop) load_stop ;;
status) status ;;
q1) q1 ;; q2) q2 ;; q3) q3 ;; q4) q4 ;; q5) q5 ;;
q6) q6 ;; q7) q7 ;; q8) q8 ;; q9) q9 ;; q10) q10 ;;
reset-t1) reset_t1 ;;
teardown) teardown ;;
*) sed -n '2,25p' "$0"; exit 2 ;;
esac
