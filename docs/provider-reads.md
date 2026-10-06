# Provider read observations v1

This foundation supplies an **opt-in producer and Go evidence validator**, not
public ECB activation. Ordinary JSON query and DTQL responses from an admitted
HTTP instance can include the closed `providerReads` envelope with format
`ovdb-provider-read/1`. Legacy instances omit it. Opted-in database discovery
lists that format in `requiredEvidenceFormats`; consumers must advertise support
in trusted admission/selection before configuring this producer.

`server.WithProviderReadProfiles` takes externally verified configuration. It
binds an exact mounted no-retention HTTP instance and canonical local collection
to a canonical `providerSourceId`, executor-scoped `rightsSourceId`, original
resource ID and lowercase SHA-256 definition/decoder/rights digests. The server's
stable `WithSourceRights` identity supplies the proxy executor, independently of
request Host. Definition verification must bind exact model/meaning artifacts,
native field mapping, EUR direction, semantic grain and transient transport keys.
This API checks equality and bounds, not artifact existence or semantic approval.
It cannot admit arbitrary URL, decoder, snapshot or cache configuration.

Source rights remain the complete frozen normalized inventory. Rights digest is
SHA-256 of RFC 8785 UTF-8 JSON:

```text
{format:"ovdb-rights-binding/1",right:<complete normalized SourceRight>}
{format:"ovdb-resource-request/1",resourceId,method,upstreamUrl,params}
{format:"ovdb-read-observation-id/1",execution:<complete execution>,
 binding:<complete binding>,read:<observation omitting only observationId>}
```

The new `pkg/providerreads` implements ECMAScript number serialization, UTF-16
key ordering and Unicode scalar validation without normalization. The shared
synthetic corpus in `pkg/providerreads/testdata/parity-v1.json` was generated and
validated by DALgo JS's consumer at commit
`6b6060a617ccb6eaf2ebd8f39445ff92a27526f9`. Go tests compare full canonical
metadata and rights/request/observation digests in direct and proxy cases. The
provider/resource/body facts agree while execution authorities, attestations and
correctly bound digest values differ. No real ECB bytes are in that corpus.

The producer uses dalgo2http's per-call provenance observer. Its body hash, byte
count and bounded content type/ETag/Last-Modified fields describe the actual
single transient response consumed by decoding. The returned reference date is
separate from UTC fetchedAt and HTTP metadata. These executor observations are
not signatures or publisher verification of live bytes. Evidence never contains
body, rows, secrets or replay references. No second fetch is made for evidence.

A per-execution collector detaches the preflight inventory before reads,
validates planned resources and rights, and finalizes actual usage before the
whole buffered response is written. Usage includes filtered-to-zero and
projected-away inputs. Identical observation IDs deduplicate; changed requests or
read times create distinct observations. Conflicts, late resources, absent
observations and over-budget inventories fail before output. The ECB server
profile permits one response and reserves bounded evidence space before fetching;
combined rights/used IDs/providerReads is at most 256 KiB and also charged to the
whole-response limit. Generic plans may lower these budgets, including zero reads.
The collector deduplicates metadata only; callers may not infer reuse of source
bytes from that. HTTP relational joins/self-joins remain refused before fetching.
No joins, historic lookup or live source activation are enabled by this change.

Go consumers call `providerreads.Decode` for the closed wire shape, then
`providerreads.Validate` with independently admitted execution, immutable
bindings, requests, source rights and actual legacy usage before releasing rows.
Missing required evidence must fail the selecting product's gate; optional legacy
absence remains unknown. Every strict transport must preserve known envelopes
and refuse unsupported required formats.

## Rollout gates

- Update `dal-go/dalgo2openvaultdb`'s Go HTTP decoder and
  `dalgo-http-adapters/packages/ovdb`'s JS decoder: their existing row-only
  responses may discard providerReads. This repository does not change them.
- Deploy aware strict readers and validate source/expectedServerId equality,
  complete source rights, notices and provider/executor bindings before producer
  opt-in. The producer does not rewrite rights or relax old identity checks.
- Verify the exact immutable live definition, decoder, ModelSpec/MeaningGraph
  artifacts and declared terms/notices at admission. A definition digest does not
  certify future XML bytes.
- Complete owned no-retention/no-store storage-sink review and the bounded actual
  browser/OVDB journey, error/cancel/navigation/empty-result notices and applicable
  paid/free-only gates. Synthetic tests do not establish these product receipts.

No source/result retention, snapshot, export or real-source fixture is authorized
by this API. Public activation and default joins remain blocked pending all gates.
