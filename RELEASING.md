# Release policy

## Fresh v1 baseline

The supported baseline is v1.0.4. It contains the current public API and numeric
ledger, with no aliases for removed methods and no legacy database conversion.
Normal operations initialize tracking automatically; validation remains read-only.
Future incompatible public API changes require a new major version and Go module
path. Previously published version strings must never be reused.

## Retiring the pre-launch versions

Go proxies and the checksum database retain published module identities. Deleting
a GitHub release or moving a tag does not replace those downloads. The old v1.0.0
is already cached, so this baseline uses the unused v1.0.4 tag.

The go.mod file retracts every known prior v1 release. The administrative v1.9.2
tag carries those directives above the historical versions and retracts itself.
It has no GitHub release page and must not be pinned by consumers. Both v1.0.4 and
the administrative tag point to the same reviewed commit. This is the
[Go module self-retraction mechanism](https://go.dev/ref/mod#go-mod-file-retract),
not a second supported release.

Normal `@latest` and `@v1` queries must resolve to v1.0.4. Existing consumers must
also replace every direct or transitive requirement on a retired version:
retraction does not override requirements already in a module graph. Do not use
`replace`, `exclude`, or disabled checksum verification to force the reset.

Retire old GitHub release pages and tags only after preserving their references
and verifying the new release through the public Go proxy and checksum database.
Keep the administrative tag available so fresh Go clients can read the retractions.

Go reads retractions from the highest published version before filtering. A
later change to retraction policy therefore needs a newer self-retracted marker
while the supported release may remain in v1.0.x.

The higher versions are enumerated individually to avoid reserving unused future
versions. A later v1.0.5 or v1.1.2 remains selectable without changing this marker.
