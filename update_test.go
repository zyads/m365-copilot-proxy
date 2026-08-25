package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestFindGo(t *testing.T) {
	// With a bare PATH the toolchain must still be found (GOROOT fallback).
	old := os.Getenv("PATH")
	os.Setenv("PATH", "/nonexistent")
	defer os.Setenv("PATH", old)
	p, err := findGo()
	if err != nil {
		t.Fatalf("findGo with bare PATH: %v", err)
	}
	if filepath.Base(p) != "go" && filepath.Base(p) != "go.exe" {
		t.Fatalf("odd go path %q", p)
	}
	if out, err := exec.Command(p, "version").Output(); err != nil || len(out) == 0 {
		t.Fatalf("resolved go does not run: %v", err)
	}
	// GO_BIN wins.
	os.Setenv("GO_BIN", filepath.Dir(p))
	defer os.Unsetenv("GO_BIN")
	if q, _ := findGo(); q != p && q != filepath.Join(filepath.Dir(p), "go") {
		t.Errorf("GO_BIN not honoured: %q", q)
	}
}
