//go:build !windows

package config

func applyConfigDirACL(string) error {
	return nil
}

func applyConfigFileACL(string) error {
	return nil
}
