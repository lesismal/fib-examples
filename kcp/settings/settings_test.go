package settings

import (
	"flag"
	"io"
	"testing"
	"time"
)

func parse(t *testing.T, args ...string) Flags {
	t.Helper()
	flags := flag.NewFlagSet("t", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	f := FlagsOn(flags)
	if err := flags.Parse(args); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestDefaultsAreKcptuns(t *testing.T) {
	f := parse(t)
	c, err := f.Config()
	if err != nil {
		t.Fatal(err)
	}
	if c.NoDelay != 0 || c.Interval != 30*time.Millisecond || c.Resend != 2 || !c.NoCongestion {
		t.Fatalf("mode fast: %+v", c)
	}
	if c.Crypt == nil || c.DataShards != 10 || c.ParityShards != 3 {
		t.Fatalf("crypt %v, FEC %d/%d", c.Crypt, c.DataShards, c.ParityShards)
	}
	if got := f.Describe(); got != "mode fast, crypt aes, FEC 10/3" {
		t.Fatalf("Describe %q", got)
	}
}

func TestPresetsAndErrors(t *testing.T) {
	c, err := parse(t, "-mode", "fast3", "-crypt", "null", "-ds", "0").Config()
	if err != nil {
		t.Fatal(err)
	}
	if c.NoDelay != 1 || c.Interval != 10*time.Millisecond || c.Crypt != nil || c.DataShards != 0 {
		t.Fatalf("fast3, null, no FEC: %+v", c)
	}
	if got := parse(t, "-ds", "0").Describe(); got != "mode fast, crypt aes, no FEC" {
		t.Fatalf("Describe %q", got)
	}
	if _, err := parse(t, "-mode", "turbo").Config(); err == nil {
		t.Error("accepted an unknown mode")
	}
	if _, err := parse(t, "-crypt", "salsa20").Config(); err == nil {
		t.Error("accepted an unsupported crypt")
	}
}
