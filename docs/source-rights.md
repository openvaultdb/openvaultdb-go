# Source data terms runtime foundation

Source data terms describe an authorized source's declarations. They do not
change access policies, admit a provider, license a derived result, or certify
that additional terms are compatible with SPDX conditions.

## Authoring and inheritance

The additive runtime syntax is:

```yaml
database:
  id: example
  schema_mode: schemaless
  license: MIT
storage:
  engine: ingitdb
  path: data
recordset_licenses:
  Rates:
    name: Source reuse conditions
    url: https://example.org/terms#reuse
    text: |
      Example source-data terms.
      Preserve source attribution.
```

An embedder configures the server declaration and stable identity with
`server.WithSourceRights("example-server", declaration)`. Passing nil supplies
an identity without declaring server terms. A database containing declarations
requires this option; `server.NewChecked` reports missing identity as a startup
configuration error. An HTTP request's Host never defines evidence identity.

A declaration is a legacy SPDX string or a closed object with optional `name`,
`spdx`, `url`, and `text`. Objects require SPDX, URL, or text; a custom declaration
requires URL and/or text. Supplied strings must be nonblank. Null, empty objects,
wrong types, duplicate/unknown keys, unsafe URLs and invalid overrides fail.
Omission inherits. The nearest whole declaration wins: recordset, then database,
then server. A URL-only leaf does not inherit an ancestor's SPDX or text.
Declaration values are copied when the database opens and the server is built.
Changing a caller's manifest/declaration pointers cannot change mounted terms.

The standalone `pkg/license` exposes explicit Directory and Publisher SPDX
validation profiles. The runtime parser uses the existing Directory-shaped
single-atom syntax (up to 64 ASCII bytes) and the bounded 2–4 distinct known-atom
` AND ` conjunction syntax. It does not impose Publisher's 18-atom single-ID
allowlist: for example, `CC-BY-3.0` parses at runtime and fails the Publisher
profile. Parsing does not imply admission by the actual Directory or Publisher.
Legacy strings serialize unchanged. Resolved evidence normalizes them to objects.

Name is limited to 256 UTF-8 bytes, URL to 2048, and text to 65536. Text permits
ordinary newlines/tabs but rejects forbidden controls. URLs require absolute
HTTPS without credentials; fragments are preserved. Renderers never fetch a
terms URL. Human pages escape text and use full-window external links.

SQL recordset overrides use the exact canonical declared collection name;
unknown names and alternate quoted spellings fail opening. Schemaless/partial document engines
allow canonical validated collection names independently of field schemas.
Strict document engines require a declared collection.
This does not add schema references or query joins.

## Evidence and supported endpoints

`sourceRights` is an additive terms inventory, with the reviewed
`ovdb-data-rights/1` item shape: `sourceId`, `source`, normalized `declaration`,
`declarationScope`, `declaredAt`, `evidenceOrigin`, `pins`, and `transformations`.
Attribution/free-source fields exist in the public type for the later verified
publication stage. This foundation emits `server-declared` for generic structured
terms and `legacy-metadata` for legacy SPDX strings, with empty pins and
transformations. It never emits `publisher-verified` or invents immutable pins.

`sourceId` is `ovdb:` plus Go `url.PathEscape(serverId)`, `/`, escaped database
ID, `/`, and escaped canonical recordset name. Server/database scope source
identities leave the lower components empty. For example:
`ovdb:fixture-server/fx/Rates%20%2F%20100%25` identifies `Rates / 100%`.
PathEscape leaves `+ : @ $ & =` intact; do not substitute encodeURIComponent.
Inventory entries sort by sourceId and retain distinct source identities even
when their effective declaration is identical.

- `/.well-known/openvaultdb` and `/v1/status` include server terms. Public
  well-known database entries and owner database listings include database terms.
- `/v1/databases/{db}` includes effective database and all disclosed recordset
  terms beside existing collections/schemas. There is no separate collection
  metadata endpoint. Protected-schema discovery keeps its existing refusal.
- `/read?key=...` and `/records/{key}` include terms for their authorized root
  recordset beside the existing key/data result.
- Ordinary `/query` and `/dtql` include complete classified source terms and
  `usedSourceIds`; even an empty filtered scan counts as considered.
- Relational `/dtql` and `/v1/dtql` freeze every classified input before running,
  including derived/subquery sources. Used IDs come from actual executor source
  evidence. An empty root scan can leave a planned subquery unused. An unexpected
  executed identity fails before output. Existing join/policy/engine refusals hold.
- Snapshot `/dtql` paging retains the initial inventory with its materialized
  spool. Every page/retry returns the same rights and used IDs. Relational
  snapshots remain unsupported, as before.
- Existing human server/database/collection pages show effective scope, safe
  links/text, or “Source data terms not declared”.

The rights inventory is frozen before output, limited to 256 KiB of encoded
metadata (including used IDs), and charged to result/snapshot/page budgets.
Oversized metadata or metadata-plus-data fails before releasing any rows.
Capture failures on policy-protected single-source reads are reported only
following the mandatory read authorization, so terms metadata cannot reveal
whether a protected collection exists. Error/denied responses carry no rights.

When every considered source lacks terms, results retain the legacy response
shape. For mixed declared/undeclared sources, usedSourceIds includes **all**
considered authorized inputs, while sourceRights includes only actual declarations.
A used ID without a rights entry means “Source data terms not declared”; readers
must not fabricate a declaration or infer permission. No unrelated configured
source is included.

## Remaining publication work

This is the compatible library/runtime reader stage. Publisher/Directory profile
adoption, versioned descriptor schemas, verified provenance/pin ingestion, raw
server/database identity binding, descriptor-to-generated-runtime equality,
operator CLI/config wiring, and actual ECB publication/deployment remain later
gates. Existing publisher/provider/public availability is unchanged. This stage
has no ECB provider, refresh claim, Cloud Run experiment, paid execution, or
DataTug historical-reopen promise. Browser federation must additionally freeze
its own complete planned lookup inventory and preserve cached lookup evidence.
