# Legal texts

The beta terms ([termos-beta.md](termos-beta.md)) and the privacy notice
([privacidade.md](privacidade.md)), in Portuguese, the binding language
for Brazilian users (Q15, #125). They are the source of the pages the
portal serves; the published page is the one that binds.

## Fields filled in at publication

The texts name the controller and how to reach them, which the published
pages must show (LGPD art. 9 and 41) but this repository doesn't keep.
Each `{{field}}` is filled in when the portal serves the page, from the
api's environment, set in `api.env` on the server, which stays out of Git
like the api's other private settings:

| Field | Variable | What |
|---|---|---|
| `{{controlador}}` | `OTTER_LEGAL_CONTROLLER` | The controller's full name |
| `{{contato}}` | `OTTER_LEGAL_CONTACT` | The email for data requests and everything the terms cover |
| `{{pais_entrada}}` | `OTTER_LEGAL_EDGE_COUNTRY` | The country of the public entry point |
| `{{data_publicacao}}` | `OTTER_LEGAL_PUBLISHED` | The date this version was published |

A field left unset keeps the page from being served, so a text never goes
out with a blank where the controller's name belongs. Changing a text, or
the date, is a new version: the terms and the notice say users are told
before a change takes effect.
