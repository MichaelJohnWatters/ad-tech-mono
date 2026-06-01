package main

import "github.com/MichaelJohnWatters/ad-tech-mono/pkg/idgen"

// DeriveID delegates to the shared idgen package so the seed binary and any
// runtime service that needs to regenerate the same UUID get identical output.
func DeriveID(kind, externalKey string) string {
	return idgen.Derive(kind, externalKey)
}
