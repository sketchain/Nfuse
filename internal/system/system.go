// Package system holds host-environment preflight checks.
package system

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// KernelVersion returns the running kernel's (major, minor) version.
func KernelVersion() (major, minor int, raw string, err error) {
	b, err := os.ReadFile("/proc/sys/kernel/osrelease")
	if err != nil {
		return 0, 0, "", err
	}
	raw = strings.TrimSpace(string(b))
	// Format is like "6.18.5-arch1-1"; take the leading "MAJOR.MINOR".
	parts := strings.SplitN(raw, ".", 3)
	if len(parts) < 2 {
		return 0, 0, raw, fmt.Errorf("unrecognized kernel version %q", raw)
	}
	major, err = strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, raw, fmt.Errorf("kernel major %q: %w", parts[0], err)
	}
	// Minor may carry a suffix; strip non-digits.
	minStr := parts[1]
	for i, r := range minStr {
		if r < '0' || r > '9' {
			minStr = minStr[:i]
			break
		}
	}
	minor, err = strconv.Atoi(minStr)
	if err != nil {
		return 0, 0, raw, fmt.Errorf("kernel minor %q: %w", parts[1], err)
	}
	return major, minor, raw, nil
}

// CheckNetdevEgress verifies the kernel is new enough for the nftables netdev
// egress hook, which requires Linux >= 5.16. The ingress hook is older, but
// Nfuse meters both directions, so egress support is mandatory.
func CheckNetdevEgress() error {
	major, minor, raw, err := KernelVersion()
	if err != nil {
		return fmt.Errorf("cannot determine kernel version: %w", err)
	}
	if major < 5 || (major == 5 && minor < 16) {
		return fmt.Errorf("kernel %s is too old: nftables netdev egress hook requires Linux >= 5.16", raw)
	}
	return nil
}
