//go:build !windows

package tally

// TallyListeningPorts has no socket table to read off Windows; callers fall
// back to their known endpoints.
func TallyListeningPorts() ([]int, error) { return nil, nil }
