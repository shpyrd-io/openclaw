package main

import (
	"fmt"
	"os"
)

// prepareHome makes OpenClaw's state directory inside the volume. The
// volume's root belongs to root, handed to the process's group by the
// platform; the state is a directory of our own, 0700, as OpenClaw makes
// it itself.
func prepareHome(home string) error {
	if err := os.MkdirAll(home, 0o700); err != nil {
		return fmt.Errorf("making OpenClaw's state directory: %w", err)
	}
	if err := os.Chmod(home, 0o700); err != nil {
		return fmt.Errorf("OpenClaw's state directory must be ours alone: %w", err)
	}
	return nil
}
