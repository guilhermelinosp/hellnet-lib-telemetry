package instrument

import (
	"context"
	"os/exec"
	"strings"
	"testing"
)

func TestContractPackagesDoNotPullSDKOrZap(t *testing.T) {
	cmd := exec.CommandContext(context.Background(), "go", "list", "-deps", "./instrument", "./messaging") //nolint:gosec // fixed local validation command
	cmd.Dir = ".."
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps: %v\n%s", err, out)
	}
	for _, forbidden := range []string{
		"go.opentelemetry.io/otel/sdk",
		"go.uber.org/zap",
		"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp",
		"go.opentelemetry.io/otel/exporters",
	} {
		if strings.Contains(string(out), forbidden) {
			t.Fatalf("contract dependencies contain forbidden package %q", forbidden)
		}
	}
}
