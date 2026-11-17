# Getting started

For the person who received an invite: from the link to a `psql` prompt,
then what to know before an application depends on the database.

You need a browser, and `psql` 16 or later for the connection check in
step 3 (an older one works too, see [Verifying the server](#verifying-the-server)).

## 1. Create your account

Open the invite link. It works once, and only as a whole: the part after
`#` is the invite, and the page removes it from the address bar as soon as
it reads it, so a reload asks for the link again.

- The account name is optional.
- The password needs at least 12 characters.
- There is no "forgot my password" by email. If you lose it, ask the person
  who invited you for a reset link.

After signing up you land on the dashboard. It shows your plan and what it
allows. If it says there is no subscription, the plan hasn't been given to
your account yet: tell the person who invited you.

The beta plan, Founder Beta, is free and allows:

| | |
|---|---|
| Databases | 2 |
| Storage, all databases together | 10 GB |
| Connections, all databases together | 60 |

## 2. Create a database

**Create a database**, then choose:

- **A name.** It's yours to read in the portal; it doesn't appear in the
  connection details.
- **A size:**

  | Size | Storage | Connections |
  |---|---|---|
  | Small | 2 GB | 20 |
  | Medium | 5 GB | 30 |

- **An expiry, if you want one.** At that date the database is stopped, and
  deleted 7 days later unless you set a later date. Leave it empty for a
  database that stays. **Ephemeral** (only with an expiry) means it is
  deleted at the expiry with no copy kept: for tests and throwaway work.

The page then shows the database's **password, this one time**. Copy it and
save it somewhere safe before you leave or reload the page: the platform
keeps only a hash of it and can't show it again. If you lose it, reset it
from the database's page (step 4).

Creating takes a few minutes. The page says "The database is ready" when it
is.

## 3. Connect

Open the database. Its **Connection** section shows the host, port,
database, user, and a connection string without the password. Copy the
string and connect:

```sh
psql "postgresql://<user>@<host>:5432/<database>?sslmode=verify-full&sslrootcert=system"
```

That is the copied string with `sslmode=require` changed to
`sslmode=verify-full&sslrootcert=system`. `psql` asks for the password.
Then:

```sql
create table t (i int);
insert into t values (1);
select * from t;
```

### Verifying the server

TLS is required on every connection. `sslmode=require` encrypts but
doesn't check who answers; `verify-full` also checks that the certificate
is valid for the host, which is what you want. The certificate is from a
public authority:

- libpq 16 or later (`psql --version`): `sslrootcert=system` uses your
  system's trusted authorities.
- Older libpq: point `sslrootcert` at your system's CA bundle instead, for
  example `/etc/ssl/certs/ca-certificates.crt` on Debian and Ubuntu, or
  `/etc/ssl/cert.pem` on macOS and FreeBSD.
- Drivers in other languages have their own setting for this; set it to
  verify the certificate and the hostname.

The platform routes each connection by the hostname the client sends in
TLS (SNI). libpq 14 and later send it by default; a client that doesn't
can't be routed and fails during the TLS handshake. Use the host exactly
as shown, never an IP address.

## 4. Running it

The database's page has:

- **Stop:** refuses new connections and closes open ones. The data stays,
  and the database still counts against your plan. **Start** opens it again.
- **Reset password:** a new password, shown once, as at creation. The old
  one stops working at once, so applications using it need the new one.
- **Expiry:** set, change or clear it at any time.
- **Delete:** type the name to confirm. A copy is kept for 3 days, and the
  operator can restore it on request; after that it's gone. An ephemeral
  database keeps no copy.
- **Activity:** every operation on the database, who asked for it, and how
  it ended.

**Account** has your password and your signed-in sessions: you can end one
session, or all but the current one. Changing the password ends your other
sessions.

## Before an application depends on it

### Connections go through a pooler in transaction mode

Your application connects to PgBouncer, which lends it a server connection
**for one transaction at a time**. Most applications don't notice. What
doesn't carry from one transaction to the next:

- `SET` outside a transaction. Use `SET LOCAL` inside one, or set a
  default on your role: `ALTER ROLE CURRENT_USER SET …`. A role default
  reaches a server connection only when the pooler opens a new one, which
  can take up to an hour.
- `LISTEN`/`NOTIFY`, session advisory locks (`pg_advisory_lock`; the
  `pg_advisory_xact_lock` form is fine), temporary tables kept across
  transactions, and `WITH HOLD` cursors.

Prepared statements, including the protocol-level ones most drivers use,
work.

The plan's connection limit counts client connections to the pooler, so an
application's pool of 10 uses 10 of them. Keep pools small: a few server
connections are shared behind them anyway.

### Limits that apply in the database

- A statement that runs longer than **60 seconds** is canceled, and a
  session left idle inside an open transaction for **5 minutes** is ended.
  Both are defaults you can change within a transaction
  (`SET LOCAL statement_timeout = '5min'`) or for your role (above).
- Temporary files are limited to the database's storage.

### What you can do in the database

Your user owns the database: it creates schemas, tables and functions in
it. It is not a superuser and can't reach any other
database. Extensions you can create yourself (`CREATE EXTENSION …`):

`btree_gin`, `btree_gist`, `citext`, `cube`, `dict_int`, `fuzzystrmatch`,
`hstore`, `intarray`, `isn`, `lo`, `ltree`, `pg_trgm`, `pgcrypto`, `seg`,
`tablefunc`, `tcn`, `tsm_system_rows`, `tsm_system_time`, `unaccent`,
`uuid-ossp`. `gen_random_uuid()` needs none of them.

The server is PostgreSQL 18, shared with other databases. If you need a
superuser, other extensions, server settings or logical replication, this
plan isn't the right fit; say so, so we know.

### Storage

Storage is measured about once a minute, so a database can pass its
allocation briefly before anything happens:

| Use | What happens |
|---|---|
| 80 % | A warning on the database's page |
| 100 % | The database becomes **read-only**: new transactions can read but not write |
| 120 % | Connections are refused until the operator reopens it for cleanup |
| below 95 % | Back to read-write |

To free space while it's read-only, start a transaction with
`BEGIN READ WRITE`, then `DROP` or `TRUNCATE` what you don't need, and
`COMMIT`. `DROP`, `TRUNCATE` and `VACUUM FULL` give space back; a plain
`DELETE` frees it only for reuse inside the database.

### Backups

Each database is dumped every night. A dump is kept 7 days, and the first
of each week 28. Restoring one is done by the operator, on request. You can
take your own copy at any time with `pg_dump`, over the same connection.

## Help

Ask the person who invited you. Say which database (its name in the
portal), what you did, and the message you saw, word for word.
