// Package version holds the IBS release version, set at build time:
//
//	go build -ldflags "-X ibs/internal/version.Version=1.0.0" ./cmd/ibs
package version

var Version = "dev"

// MinCompatible is the oldest IBS version this one works with: sites, nodes
// and relays from that version on understand each other. Raise it when a
// change breaks older versions.
const MinCompatible = "1.1.0"
