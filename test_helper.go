package filesystem_fat32

import "testing"

// openTestFS opens the image at path and returns the concrete *fat32FS
// implementation for package-internal tests. It fails the test on error and
// registers a cleanup to Close the filesystem.
func openTestFS(t testing.TB, path string) *fat32FS {
	t.Helper()
	fsIfc, err := Open(path, -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = fsIfc.Close() })
	return fsIfc.(*fat32FS)
}
