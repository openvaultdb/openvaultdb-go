module github.com/openvaultdb/openvaultdb-go/dalgo2ovdb

go 1.26.1

// dalgo2ovdb is an external HTTP client adapter: it only imports dalgo (the
// interfaces it implements) at build time. The replace + internal-package
// import below are TEST-ONLY (see roundtrip_test.go) — they let the round-trip
// test boot a real in-process ovdb-server from this sibling module without a
// separate `go run` step. The adapter code itself never imports internal/*.
require github.com/dal-go/dalgo v0.62.4

require (
	github.com/RoaringBitmap/roaring/v2 v2.18.2 // indirect
	github.com/bits-and-blooms/bitset v1.24.4 // indirect
	github.com/mschoch/smat v0.2.0 // indirect
	github.com/openvaultdb/openvaultdb-go v0.0.0
	github.com/strongo/random v0.0.1 // indirect
)

require (
	github.com/gofrs/flock v0.13.0 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/ingitdb/dalgo2ingitdb v0.0.1 // indirect
	github.com/ingitdb/ingitdb-go/ingitdb v0.0.1 // indirect
	github.com/ingr-io/ingr-go v0.0.2 // indirect
	github.com/pelletier/go-toml/v2 v2.3.1 // indirect
	go.starlark.net v0.0.0-20260613233743-8ba36ccb83fb // indirect
	golang.org/x/sys v0.45.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

replace github.com/openvaultdb/openvaultdb-go => ../
