package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTheStateDirectoryIsMadeInsideTheVolumeAndIsOursAlone(t *testing.T) {
	vol := t.TempDir()
	home := filepath.Join(vol, "openclaw")
	if err := prepareHome(home); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(home)
	if err != nil || st.Mode().Perm() != 0o700 {
		t.Fatalf("home: %v %v", st, err)
	}
	if err := prepareHome(home); err != nil {
		t.Fatalf("a second time: %v", err)
	}
}
