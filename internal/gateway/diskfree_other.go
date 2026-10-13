//go:build !unix

package gateway

import "errors"

// diskFree is not available here; the spool's batch-count bound still applies.
func diskFree(dir string) (free, total uint64, err error) {
	return 0, 0, errors.New("free disk space unknown on this system")
}
