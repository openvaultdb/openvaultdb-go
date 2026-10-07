# Live ECB HTTP mount

`storage.engine: http` admits closed ECB and IANA resource profiles; this page
describes `ecb-daily/1`. The IANA profile is documented in [http-iana.md](http-iana.md).
ECB uses the published `dalgo2http` adapter and its `ecb-eurofxref/1`
decoder to read `https://www.ecb.europa.eu/stats/eurofxref/eurofxref-daily.xml`.
Manifests cannot supply alternate URLs, credentials, headers, transforms,
snapshot stores or cache settings. Each query reads one bounded live response:
10-second timeout, 2 MiB body limit, decoder row/depth bounds, HTTPS address
checks and no redirects. The mount never falls back to a retained copy.

```yaml
database: {id: ecb, schema_mode: strict, retention: none}
storage:
  engine: http
  http: {profile: ecb-daily/1, collection: daily}
schemas:
  collections:
    daily:
      fields:
        time: {type: string}
        currency: {type: string}
        rate: {type: string}
```

This is a transport definition, not an admitted public source. It deliberately
declares no inferred license or semantic mapping. The native fields are the
source reference date, quote currency and positive decimal rate lexeme.
Rates remain strings, including trailing zeros. EUR is the observed base,
not a synthesized source row. The collection name is local; the transport
record key is the currency within this single daily response and is not a
historical compound-key promise. The OVDB record API exposes no stable canonical
composite key in this preparatory mount; publisher semantic grain
`[time, currency]` therefore needs independent contract review before activation.

Single-collection structured queries and DTQL support plain field projections,
supported residual predicates and limits. Keyed Get/Exists, writes, ordering,
offsets, cursors and aggregations are unsupported. Typed HTTP refusals use
`501 operation_unsupported`. Relational HTTP execution stays outside the
default join-engine allowlist pending the complete observation contract and
consumer rollout; guarded DALgo reader leaves can be tested separately.
Discovery advertises queries, no point reads/writes and no default joins.

HTTP sources always have the immutable `retention: none` capability and require
absent/zero cache TTL. Existing server guards enforce no-store responses and
reject continuation headers/tokens before upstream reads, snapshot admission or
spooling. Strict schema mode avoids inferred catalogues; mounting writes no
source or result data. Each execution holds only the bounded current response
in RAM. Metadata changes cannot grant retention, point reads or writes.

Existing sourceRights/usedSourceIds transport carries explicitly configured
terms. Configure a stable `WithSourceRights` server identity when declarations
are supplied. This mount does not establish that the declarations satisfy ECB
conditions or that paid integration is allowed.

`dalgo2http.ContextWithProvenanceObserver` receives every successful resource
read, including reads filtered to zero rows. Its transient observation includes
collection, live mode, status, fetchedAt, decoder, EUR base, referenceDate,
upstream URL, content type, Last-Modified, ETag, SHA-256 and byte count. It stores
no body. Fetch time and HTTP update metadata are separate from source date.
The opt-in [providerReads producer](provider-reads.md) emits the closed
`ovdb-provider-read/1` envelope beside sourceRights/usedSourceIds on ordinary
structured queries and DTQL. Its per-execution collector freezes trusted
provider/resource/definition/decoder/rights bindings before reading and produces
canonical request/rights/observation digests from the transient observer. Strict
clients supply their independently admitted fresh execution ID through the
validated `OVDB-Execution-ID` header. Legacy requests retain a server nonce;
non-profile instances omit providerReads. This does not enable joins or establish
consumer interoperability, source admission or public activation.

Public activation still requires the closed versioned evidence contract and
tolerant/strict consumer rollout, affected semantic and rights admission,
DataTug's ephemeral route and storage-sink assertions, deployed cache-bypass
verification, and a bounded live OVDB-to-browser receipt. A mount/test success
does not satisfy those gates. Existing bootstrap copies remain inactive until
the working alternative and authorized disposition are proven.
