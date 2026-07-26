package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/HongyuanJia8/unring/internal/pgproxy"
)

func TestMainHelp(t *testing.T) {
	t.Parallel()

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := Main([]string{"help"}, strings.NewReader(""), &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("Main(help) exit code = %d, want 0; stderr: %s", exitCode, stderr.String())
	}
	if !strings.Contains(stdout.String(), "unring run") {
		t.Fatalf("help output did not mention run: %s", stdout.String())
	}
}

func TestPromptDefaultsToRollbackWithoutTerminal(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer
	decision := promptDecision(strings.NewReader("commit\n"), &output)
	if decision != pgproxy.DecisionRollback {
		t.Fatalf("promptDecision() = %q, want rollback", decision)
	}
	if !strings.Contains(output.String(), "--commit") {
		t.Fatalf("non-interactive guidance missing: %s", output.String())
	}
}
