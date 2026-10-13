package gateway

import "testing"

// diskFree reads the real filesystem of a directory.
func TestDiskFreeReadsTheFilesystem(t *testing.T) {
	free, total, err := diskFree(t.TempDir())
	if err != nil {
		t.Skipf("no statfs here: %v", err)
	}
	if total == 0 || free > total {
		t.Fatalf("free %d of %d bytes makes no sense", free, total)
	}
}
