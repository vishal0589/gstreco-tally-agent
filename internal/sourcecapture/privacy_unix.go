//go:build !windows

package sourcecapture

// os.Mkdir(0700) and files created with 0600 enforce owner-only access.
func protectDirectory(_ string) error { return nil }
