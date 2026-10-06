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

Strict clients freeze a complete independently admitted plan before awaiting
credentials or network I/O, then send its fresh 128-bit execution ID as exactly
one `OVDB-Execution-ID` request header containing 32 lowercase hexadecimal
characters. On profile-enabled structured query and DTQL routes the producer
validates this header before collector reservation or upstream I/O and binds its
value into `execution.id` and the hashed observation envelope. Empty, malformed,
comma-coalesced or duplicate values return `400 invalid_execution_id` with
`Cache-Control: no-store`. Errors contain no supplied header value. Missing
headers retain the fresh server-generated nonce for legacy callers; a strict
client cannot obtain independent admission by copying that nonce from a response.
Non-profile instances ignore the header. Client IDs correlate executions only:
they do not authenticate, admit sources, select resources or alter server-owned
executor/bindings/rights. Clients generate a new cryptographic ID per execution;
the server stores no replay ledger or new source/result state.

If CORS origins are configured, provider opt-in adds `OVDB-Execution-ID` to the
allowed request headers on that server's independent CORS configuration. It
preserves the configured origins and leaves non-profile server defaults intact.

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
`providerreads.ValidateMetadata` with independently admitted execution, immutable
bindings, requests, source rights and actual legacy usage before releasing rows.
This checks full response rights/usage equality before the lower-level envelope
`Validate` check; validating digests alone is insufficient.
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
- Verify the paired consumer sends the independently frozen execution ID and
  rejects changed/missing execution evidence through an actual synthetic Go HTTP
  producer to consumer journey before claiming interoperability.
- Verify the exact immutable live definition, decoder, ModelSpec/MeaningGraph
  artifacts and declared terms/notices at admission. A definition digest does not
  certify future XML bytes.
- Complete owned no-retention/no-store storage-sink review and the bounded actual
  browser/OVDB journey, error/cancel/navigation/empty-result notices and applicable
  paid/free-only gates. Synthetic tests do not establish these product receipts.

No source/result retention, snapshot, export or real-source fixture is authorized
by this API. Public activation and default joins remain blocked pending all gates.

## Independent review r1 disposition

M1 (provider alias/retirement lifetime) is fixed: provider-bound instances now use
the same shared-alias lease and final-retirement guard as immutable profiles.
Removing a nonfinal alias preserves the shared waitgroup and resources; final
removal retires the pointer before draining, including a canceled caller wait.
Tests hold leases through two aliases, add a new lease through the surviving
alias, verify final close occurs once after all releases, and reject remounting
the retiring/closed pointer under its native ID.

m1 (generic URL mismatch) is fixed by explicitly narrowing the Go v1 admission
profile. It accepts lowercase ASCII DNS authorities with at least two labels and
an alphabetic top-level label, optional canonical nondefault numeric port, an
explicit path, reviewed ASCII URI characters and valid percent escapes. It
refuses literal/encoded dot path segments, credentials/fragments, default/zero-
padded ports, IP/numeric-host forms, single-label/trailing-dot hosts, raw Unicode
and characters requiring WHATWG normalization. All `xn--` punycode labels are refused, including valid A-labels, until an
exact reviewed WHATWG-compatible IDNA policy is supported. URLs are never rewritten after binding. This is a fail-closed
subset of the broader JS core HTTPS gate; future host/locator grammar expansion
requires its own cross-runtime review. The fixed ECB locator remains accepted.
`testdata/url-grammar-v1.json` records Go/JS acceptance separately: all 22 cases
were checked against the exact JS consumer above, with no Go-accepted URL rejected
by JS. No finding was declined. Independent review r2 remains pending.

## Independent review r2 disposition

B1 (staticcheck QF1001) is fixed in its own commit by applying De Morgan's law to
the character grammar without changing the accepted character set. No lint
suppression or hook bypass was used.

The residual m1 invalid-punycode mismatch is fixed conservatively: every `xn--`
DNS label is rejected before binding, including valid punycode labels. This
narrows the v1 Go profile further without changing the fixed ECB locator. The
reviewer's expanded 1,012-URL differential corpus is rerun against the exact JS
consumer; Go-admitted URLs must all pass it unchanged. No findings declined;
independent review r3 remains pending.
