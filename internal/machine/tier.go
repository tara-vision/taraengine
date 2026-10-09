// Package machine describes the host: GPU, memory, RAM tier, driver.
package machine

// TierFor maps host RAM to the registry's tiers with slack for what the OS reports: 14 GiB and up is
// the 16 tier, 30 GiB and up the 32 tier, 46 GiB and up the 48 tier.
func TierFor(ramBytes int64) string {
	const gib = int64(1) << 30
	switch {
	case ramBytes >= 46*gib:
		return "48"
	case ramBytes >= 30*gib:
		return "32"
	case ramBytes >= 14*gib:
		return "16"
	}
	return "small"
}
