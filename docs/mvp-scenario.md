# The MVP scenario

The M6 acceptance (#115): the whole customer path, done in a browser on the
real platform by a person on another network, and recorded here. Run it again
before a release that changes the portal or the path a database takes.

Who ran it is recorded only as "a person on another network"; the operator's
notes say who.

## Before

- The portal deployed (`deploy/control-plane/README.md`), the worker running,
  and the edge serving the portal and the databases' hostnames.
- An invite for the person: `otter admin invite create <email>`, with the link
  handed over out of band.
- On the person's machine: a browser, and `psql` 16 or later.
- A network other than the platform's own: mobile data counts.

## Checklist

Each step says what should happen. Record anything else, word for word.

| # | Step | Expected |
|---|---|---|
| 1 | Open the invite link | "Create your account", and the token gone from the address bar |
| 2 | Sign up with a password of fewer than 12 characters | The API's message, as written |
| 3 | Sign up properly | The dashboard: the plan by name, its limits, no databases |
| 4 | Create a database (Small) | The password, once, with a copy button and a warning; "Pending" |
| 5 | Save the password, then wait | "The database is ready" within a few minutes; the password still shown |
| 6 | Reload the page | No password anywhere |
| 7 | Open the database | Running, OK, connection details, a connection string with no password |
| 8 | Connect: `psql "<connection string>"`, with the password | Connected over TLS; `create table t (i int); insert into t values (1);` works |
| 9 | Stop it, read the confirmation, confirm | Stopped; `psql` is refused |
| 10 | Start it | Running; the row from step 8 is still there |
| 11 | Reset the password | A new password once; the old one is refused, the new one connects |
| 12 | Set an expiry a day ahead, then clear it | Shown, then gone |
| 13 | Activity, then the database's own activity | Create, stop, start, password reset; each by you, with no internal detail |
| 14 | Account: change the password | "Your other sessions have ended"; still logged in |
| 15 | Log in on a second browser or device, then end that session from Account | The other one is logged out at its next action |
| 16 | Delete the database: type its name | Deleting, then gone from the dashboard; the 3-day copy wording shown before |
| 17 | Log out, then open `/databases` | The login page; after logging in, back on the dashboard |
| 18 | Any page on a phone | Readable, no sideways scrolling |

## Runs

| Date | Version | By | Result | Notes |
|---|---|---|---|---|
