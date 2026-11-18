# Legal texts

The beta terms ([termos-beta.md](../../internal/legal/texts/termos-beta.md))
and the privacy notice
([privacidade.md](../../internal/legal/texts/privacidade.md)), in
Portuguese, the binding language for Brazilian users (Q15, #125). They live
in `internal/legal/texts/`, embedded in the binary; the portal serves them
at `/termos` and `/privacidade`. The published page is the one that binds.

## Fields filled in at publication

The texts name the controller and how to reach them, which the published
pages must show (LGPD art. 9 and 41) but this repository doesn't keep.
Each `{{field}}` is filled in by the api from its environment, set in
`api.env` on the server, which stays out of Git like the api's other
private settings. Values are inserted as escaped text, after the Markdown
is rendered.

| Field | Variable | What |
|---|---|---|
| `{{controlador}}` | `OTTER_LEGAL_CONTROLLER` | The controller's full name |
| `{{contato}}` | `OTTER_LEGAL_CONTACT` | The email for data requests and everything the terms cover; a plain address, shown as a link |
| `{{pais_entrada}}` | `OTTER_LEGAL_EDGE_COUNTRY` | The country of the public entry point |
| `{{data_publicacao}}` | `OTTER_LEGAL_PUBLISHED` | This version's date, `YYYY-MM-DD`; shown as `DD/MM/YYYY` |

- **None set:** the texts aren't published. `GET /api/legal/{name}` answers
  404, and sign-up asks for nothing.
- **All set:** both pages are served, and sign-up needs a checkbox
  accepting them. The `auth.signup` audit event records the version
  accepted (`accepted_terms`), the date in `OTTER_LEGAL_PUBLISHED`.
- **Some set, or one invalid:** the api refuses to start, so a page never
  goes out with a blank where the controller's name belongs.

The api logs the version at start, never the name or the contact.

## Publishing a new version

A change to a text, or to the fields, is a new version: set a new
`OTTER_LEGAL_PUBLISHED`. The texts promise users notice before a change that
affects their rights takes effect (15 days for the terms), so tell them
first. New sign-ups then accept the new version; accounts that accepted an
older one keep using the service, as the terms say.
