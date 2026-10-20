// Package lifecycle implements monitor/unmonitor/delete (R4-R7, R16):
// the operator-facing actions that change a library entry's protected
// status or its on-disk presence.
package lifecycle

import (
	"github.com/immortal-coil/buffarr/internal/catalog"
)

// Monitor pins a library entry, protecting it from delete (R4, R5).
func Monitor(cat *catalog.Catalog, platform, gameKey string) error {
	return cat.SetMonitored(platform, gameKey, true)
}

// Unmonitor unpins a library entry. It does not touch cached files or
// reported size -- deletion always requires the separate, explicit Delete
// action (R7).
func Unmonitor(cat *catalog.Catalog, platform, gameKey string) error {
	return cat.SetMonitored(platform, gameKey, false)
}
