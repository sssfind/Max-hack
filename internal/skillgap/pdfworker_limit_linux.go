//go:build linux

package skillgap

import (
	"errors"
	"os"
	"strconv"
	"strings"
	"syscall"
)

func applyPDFWorkerMemoryLimit(memoryLimit int64) error {
	if pdfRaceEnabled {
		return nil
	}

	statm, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		return err
	}
	fields := strings.Fields(string(statm))
	if len(fields) == 0 {
		return errors.New("read current virtual memory size")
	}
	virtualPages, err := strconv.ParseUint(fields[0], 10, 64)
	if err != nil {
		return err
	}
	pageSize := uint64(os.Getpagesize())
	if virtualPages > ^uint64(0)/pageSize {
		return errors.New("current virtual memory size overflows")
	}
	currentVirtualBytes := virtualPages * pageSize

	var existing syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_AS, &existing); err != nil {
		return err
	}
	target, err := pdfWorkerAddressSpaceLimit(currentVirtualBytes, memoryLimit, existing.Cur)
	if err != nil {
		return err
	}
	existing.Cur = target
	return syscall.Setrlimit(syscall.RLIMIT_AS, &existing)
}
