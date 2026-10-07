# Live IANA HTTP status registry mount

`iana-http-status/1` fixes one HTTPS CSV resource, the `strict-csv-three-column/1`
decoder, a ten-second maximum request, a 64 KiB response bound and at most 512
decoded rows. The adapter accepts exact `Value,Description,Reference` headers
and preserves every native value as a string, including ranges. `Value` is an
internal query transport key only; this mount grants no point-read or
single-integer status-code semantics.

```yaml
database: {id: iana-http-status, schema_mode: strict, retention: none}
storage:
  engine: http
  http: {profile: iana-http-status/1, collection: rows}
schemas:
  collections:
    rows:
      fields:
        Value: {type: string}
        Description: {type: string}
        Reference: {type: string}
```

The library permits direct native queries for an application that explicitly
constructs this fixed mount. The OVDB HTTP server refuses to start with an IANA
mount unless an independently reviewed `iana-native-operator/1` provider-read
profile supplies the exact source/resource binding, publisher-definition evidence,
rights notices, and IANA registry terms declaration. That profile permits
single-collection native JSON queries or DTQL with an explicit limit of 1..50.
It refuses point reads, writes, paging, aliases, joins, ordering, offsets,
unknown fields and inferred numeric status codes before source I/O.

Publisher-definition evidence may use the immutable publisher manifest pin, or
the independently checked official HTML descriptor with a separate Directory
discovery pin described in [provider read observations](provider-reads.md).
Directory-authored discovery metadata alone is not publisher verification.

Every admitted execution uses one live GET with no snapshot fallback. The
response and result are held only in bounded memory for that execution; no
cache, snapshot or recorded fixture is configured. No-store response headers
and transient `providerReads` evidence include the fixed resource URL, fetch
time, response digest and byte count, never CSV rows. IANA's
[registry licensing terms](https://www.iana.org/help/licensing-terms) cover
applicable protocol-registry rights and exclude linked RFCs and other material.
The source-rights declaration records those terms; it does not itself approve
an execution, a public route, semantic enrichment or retention elsewhere.

The Directory source remains inactive. This library profile and synthetic tests
do not establish current upstream response bytes, live GET access, a Cloud
route, or a browser journey.
