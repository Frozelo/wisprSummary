package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDotEnvDoesNotOverrideEnvironment(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, ".env")
	os.WriteFile(p, []byte("# c\nTG_A=from-file\nTG_B=\"quoted\"\nTG_C=file\n"), 0o600)
	t.Setenv("TG_C", "real-env")
	os.Unsetenv("TG_A")
	os.Unsetenv("TG_B")
	t.Cleanup(func() { os.Unsetenv("TG_A"); os.Unsetenv("TG_B") })

	loadDotEnv(p)
	if os.Getenv("TG_A") != "from-file" || os.Getenv("TG_B") != "quoted" || os.Getenv("TG_C") != "real-env" {
		t.Fatalf("A=%q B=%q C=%q", os.Getenv("TG_A"), os.Getenv("TG_B"), os.Getenv("TG_C"))
	}
}
