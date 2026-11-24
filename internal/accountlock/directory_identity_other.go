//go:build !darwin

package accountlock

// Existing spelling is resolved with directory entries and SameFile. Retain
// missing components on other platforms: per-directory casefold and network
// filesystem semantics are not inferred from the operating system name.
func directoryIdentityComponent(_, name string) (string, error) { return name, nil }
