package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCommonKeepsGoCacheInsideDemoRuntime(t *testing.T) {
	demoDir, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("resolve demo directory: %v", err)
	}
	command := exec.Command(
		"sh", "-c", `. "$0"; printf '%s\n' "$GOCACHE"`, filepath.Join(demoDir, "common.sh"),
	)
	command.Env = append(os.Environ(), "UNRING_BIN=/bin/true")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("source common.sh: %v\n%s", err, output)
	}
	want := filepath.Join(demoDir, "runtime", "go-build-cache") + "\n"
	if string(output) != want {
		t.Fatalf("demo GOCACHE = %q, want %q", output, want)
	}
}

func TestStopDBPreservesStateWhenPostgresStopFails(t *testing.T) {
	demoDir := copyStopScript(t)
	writeDemoFile(t, filepath.Join(demoDir, "pgdata", "PG_VERSION"), "17\n")
	writeDemoFile(t, filepath.Join(demoDir, "runtime", "sentinel"), "keep\n")
	writeDemoFile(t, filepath.Join(demoDir, "pg.log"), "keep\n")
	fakeBin := t.TempDir()
	writeDemoFile(t, filepath.Join(fakeBin, "pg_ctl"), `#!/bin/sh
case " $* " in
  *" status "*) exit 0 ;;
  *" stop "*) exit 1 ;;
esac
exit 2
`)
	if err := os.Chmod(filepath.Join(fakeBin, "pg_ctl"), 0o755); err != nil {
		t.Fatalf("make fake pg_ctl executable: %v", err)
	}

	command := exec.Command("sh", filepath.Join(demoDir, "stop-db.sh"))
	command.Env = append(os.Environ(), "PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	output, err := command.CombinedOutput()
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) || exitError.ExitCode() == 0 {
		t.Fatalf("stop-db.sh exit = %v, want nonzero\n%s", err, output)
	}
	if !strings.Contains(string(output), "leaving demo state in place") ||
		strings.Contains(string(output), "Stopped the demo") {
		t.Fatalf("stop-db.sh claimed success after pg_ctl failure:\n%s", output)
	}
	for _, path := range []string{
		filepath.Join(demoDir, "pgdata", "PG_VERSION"),
		filepath.Join(demoDir, "runtime", "sentinel"),
		filepath.Join(demoDir, "pg.log"),
	} {
		if _, statErr := os.Stat(path); statErr != nil {
			t.Fatalf("stop failure removed %s: %v", path, statErr)
		}
	}
}

func copyStopScript(t *testing.T) string {
	t.Helper()
	source, err := os.ReadFile("stop-db.sh")
	if err != nil {
		t.Fatalf("read stop-db.sh: %v", err)
	}
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "stop-db.sh"), source, 0o755); err != nil {
		t.Fatalf("copy stop-db.sh: %v", err)
	}
	return directory
}

func writeDemoFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("create directory for %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
