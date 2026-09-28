//go:build !linux

package skillgap

func applyPDFWorkerMemoryLimit(_ int64) error {
	return nil
}
