//go:build e2e

// Package e2e runs the real edgeproxy and demobackend binaries as separate
// processes and checks their behaviour over real HTTP, the way a user would
// see it. Run with:
//
//	go test -tags e2e ./e2e/
//
// Set E2E_RACE=1 to build the binaries with the race detector.
package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

var binDir string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "edgeproxy-e2e-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	binDir = dir

	for _, name := range []string{"edgeproxy", "demobackend"} {
		args := []string{"build", "-o", binPath(name)}
		if os.Getenv("E2E_RACE") == "1" {
			args = append(args, "-race")
		}
		args = append(args, "./cmd/"+name)
		cmd := exec.Command("go", args...)
		cmd.Dir = ".." // module root
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "build %s: %v\n", name, err)
			os.RemoveAll(dir)
			os.Exit(1)
		}
	}

	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func binPath(name string) string {
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return filepath.Join(binDir, name)
}
