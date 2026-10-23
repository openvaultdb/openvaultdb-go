package server

import "strings"

// isTugQLEndpointPath reports whether path ends in a TugQL endpoint, so every
// TugQL guard (provider request profile, continuation refusal, cache Vary)
// applies to it.
func isTugQLEndpointPath(path string) bool {
	return strings.HasSuffix(path, "/tugql")
}
