// Package deploy carries the files otter admin jail reclone puts back
// into a jail after a reclone (#216): what lives in a jail's root, and so
// comes back from the template without them. They are the ones in this
// directory, built into the binary, so a jail gets the version that
// matches the otter that converges it.
package deploy

import _ "embed"

var (
	//go:embed cluster/otter-pg-bootstrap
	PGBootstrap string
	//go:embed platform-db/otter-platform-db-bootstrap
	PlatformDBBootstrap string
	//go:embed platform-db/600.otter-platform-dump
	PlatformDump string
	//go:embed control-plane/rc.d/otter_api
	APIRCScript string
)
