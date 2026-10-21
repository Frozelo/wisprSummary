package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
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

func TestParseEnvLine(t *testing.T) {
	cases := []struct {
		in, k, v string
		ok       bool
	}{
		{"A=1", "A", "1", true},
		{"export B=2", "B", "2", true},
		{"C=Asia/Omsk # my zone", "C", "Asia/Omsk", true},
		{`D="x # not a comment"`, "D", "x # not a comment", true},
		{"E=", "E", "", true},
		{"# comment", "", "", false},
		{"no equals", "", "", false},
		{"  ", "", "", false},
	}
	for _, c := range cases {
		k, v, ok := parseEnvLine(c.in)
		if k != c.k || v != c.v || ok != c.ok {
			t.Errorf("%q -> %q %q %v, want %q %q %v", c.in, k, v, ok, c.k, c.v, c.ok)
		}
	}
}

func TestParseChatID(t *testing.T) {
	if n, err := parseChatID(""); n != 0 || err != nil {
		t.Errorf("empty: %d %v", n, err)
	}
	if n, err := parseChatID("-1001"); n != -1001 || err != nil {
		t.Errorf("negative: %d %v", n, err)
	}
	for _, bad := range []string{"7470679a2", "0", "1 2"} {
		if _, err := parseChatID(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestEnvValidation(t *testing.T) {
	t.Setenv("POLL_INTERVAL", "")
	if d, err := durationEnv("POLL_INTERVAL", 2*time.Minute, 30*time.Second); d != 2*time.Minute || err != nil {
		t.Errorf("default: %v %v", d, err)
	}
	for _, bad := range []string{"2", "1s", "abc", "-5m"} {
		t.Setenv("POLL_INTERVAL", bad)
		if _, err := durationEnv("POLL_INTERVAL", 2*time.Minute, 30*time.Second); err == nil {
			t.Errorf("POLL_INTERVAL=%q accepted", bad)
		}
	}
	t.Setenv("BACKFILL_HOURS", "6")
	if n, err := hoursEnv("BACKFILL_HOURS"); n != 6 || err != nil {
		t.Errorf("hours: %d %v", n, err)
	}
	for _, bad := range []string{"-1", "x", "1.5", "999999"} {
		t.Setenv("BACKFILL_HOURS", bad)
		if _, err := hoursEnv("BACKFILL_HOURS"); err == nil {
			t.Errorf("BACKFILL_HOURS=%q accepted", bad)
		}
	}
	t.Setenv("TIMEZONE", "Europe/Moscow")
	if loc, err := locationEnv("TIMEZONE"); err != nil || loc.String() != "Europe/Moscow" {
		t.Errorf("tz: %v %v", loc, err)
	}
	t.Setenv("TIMEZONE", "Asia/Novosibirk")
	if _, err := locationEnv("TIMEZONE"); err == nil {
		t.Error("typo in TIMEZONE accepted")
	}
}
