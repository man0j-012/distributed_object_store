// pkg/storage/atomicfile.go
package storage

import (
	"os"
)

// AtomicWrite writes data to tmpPath + ".tmp" then renames, guaranteeing
// that either the file is fully written or not present at all.
func AtomicWrite(path string, data []byte, perm os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
