// Package postgres starts throwaway PostgreSQL servers for integration tests.
package postgres

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Start returns a connection string for an isolated test server. When
// UNRING_TEST_DATABASE_URL is set, that server is used instead and the caller
// remains responsible for choosing unique object names.
func Start(t *testing.T) string {
	t.Helper()

	if connectionString := os.Getenv("UNRING_TEST_DATABASE_URL"); connectionString != "" {
		return connectionString
	}

	initdb, err := exec.LookPath("initdb")
	if err != nil {
		skipOrFailUnavailable(t, "initdb is not available: %v", err)
	}
	pgCtl, err := exec.LookPath("pg_ctl")
	if err != nil {
		skipOrFailUnavailable(t, "pg_ctl is not available: %v", err)
	}
	if err := checkSystemVSharedMemoryAccess(); err != nil {
		skipOrFailUnavailable(
			t,
			"System V shared memory is unavailable; refusing to run initdb and risk leaking a segment: %v",
			err,
		)
	}

	root := t.TempDir()
	dataDirectory := filepath.Join(root, "data")
	// PostgreSQL includes the full directory in its Unix-socket pathname.
	// macOS limits that pathname to 103 bytes, while t.TempDir can already be
	// close to that length. Use an explicitly short directory even though the
	// tests themselves connect over TCP.
	socketDirectory, err := os.MkdirTemp("/tmp", "unring-pg-")
	if err != nil {
		t.Fatalf("create short Postgres socket directory: %v", err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(socketDirectory); err != nil {
			t.Errorf("remove Postgres socket directory: %v", err)
		}
	})

	initCommand := exec.Command(initdb,
		"-D", dataDirectory,
		"--username=postgres",
		"--auth=trust",
		"--encoding=UTF8",
		"--no-sync",
	)
	initCommand.Env = append(os.Environ(), "LC_ALL=C", "LANG=C")
	if output, err := initCommand.CombinedOutput(); err != nil {
		if initdbBlockedBySharedMemory(string(output)) {
			skipOrFailUnavailable(
				t,
				"this environment denies PostgreSQL System V shared memory: %v\n%s",
				err,
				output,
			)
		}
		t.Fatalf("initialize throwaway Postgres: %v\n%s", err, output)
	}

	port := freePort(t)
	logPath := filepath.Join(root, "postgres.log")
	serverOptions := fmt.Sprintf(
		"-h 127.0.0.1 -p %d -k %s -F",
		port, socketDirectory,
	)
	startCommand := exec.Command(pgCtl,
		"-D", dataDirectory,
		"-l", logPath,
		"-o", serverOptions,
		"-w", "start",
	)
	startCommand.Env = append(os.Environ(), "LC_ALL=C", "LANG=C")

	startSucceeded := false
	t.Cleanup(func() {
		statusCommand := exec.Command(pgCtl, "-D", dataDirectory, "status")
		statusCommand.Env = append(os.Environ(), "LC_ALL=C", "LANG=C")
		statusOutput, statusErr := statusCommand.CombinedOutput()
		if statusErr != nil {
			if startSucceeded {
				t.Errorf("check throwaway Postgres before cleanup: %v\n%s",
					statusErr, statusOutput)
			}
			return
		}

		stopCommand := exec.Command(pgCtl,
			"-D", dataDirectory,
			"-m", "immediate",
			"-w", "stop",
		)
		stopCommand.Env = append(os.Environ(), "LC_ALL=C", "LANG=C")
		if output, err := stopCommand.CombinedOutput(); err != nil {
			t.Errorf("stop throwaway Postgres: %v\n%s", err, output)
		}
	})

	if output, err := startCommand.CombinedOutput(); err != nil {
		logOutput, _ := os.ReadFile(logPath)
		combined := string(output) + string(logOutput)
		t.Fatalf("start throwaway Postgres: %v\n%s", err, combined)
	}
	startSucceeded = true

	connectionURL := &url.URL{
		Scheme: "postgresql",
		User:   url.User("postgres"),
		Host:   net.JoinHostPort("127.0.0.1", strconv.Itoa(port)),
		Path:   "/postgres",
	}
	query := connectionURL.Query()
	query.Set("sslmode", "disable")
	connectionURL.RawQuery = query.Encode()
	return connectionURL.String()
}

func checkSystemVSharedMemoryAccess() error {
	ipcs, err := exec.LookPath("ipcs")
	if err != nil {
		// Some minimal environments do not install ipcs. In that case retain
		// the historical behavior and let initdb make the availability check.
		return nil
	}
	output, err := exec.Command(ipcs, "-m").CombinedOutput()
	if err != nil {
		return fmt.Errorf("inspect existing segments: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func skipOrFailUnavailable(t *testing.T, format string, arguments ...any) {
	t.Helper()
	message := fmt.Sprintf(format, arguments...)
	if os.Getenv("UNRING_REQUIRE_POSTGRES") == "1" {
		t.Fatalf("Postgres integration test required but unavailable: %s", message)
	}
	t.Skipf("Postgres integration test skipped: %s", message)
}

func initdbBlockedBySharedMemory(output string) bool {
	shmgetFailed := strings.Contains(output, "Failed system call was shmget(")
	deniedBySandbox := strings.Contains(
		output,
		"could not create shared memory segment: Operation not permitted",
	)
	systemLimitReached := strings.Contains(
		output,
		"could not create shared memory segment: No space left on device",
	) && strings.Contains(output, "all available shared memory IDs have been taken")
	return shmgetFailed && (deniedBySandbox || systemLimitReached)
}

func freePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("choose Postgres test port: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatalf("release Postgres test port: %v", err)
	}
	return port
}
