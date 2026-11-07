// Package settings gives the KCP examples the flags kcptun uses for the
// KCP layer, so that both ends of an example agree, and either end can
// talk to a kcp-go program set up the same way.
package settings

import (
	"crypto/pbkdf2"
	"crypto/sha1"
	"flag"
	"fmt"
	"time"

	"github.com/lesismal/fib/kcp"
)

// Flags are the KCP settings an example takes.
type Flags struct {
	mode, crypt, key *string
	ds, ps, mtu      *int
}

// FlagsOn adds -mode, -crypt, -key, -ds, -ps and -mtu to flags, with
// kcptun's defaults.
func FlagsOn(flags *flag.FlagSet) Flags {
	return Flags{
		mode:  flags.String("mode", "fast", "speed preset: normal, fast, fast2 or fast3, as kcptun's"),
		crypt: flags.String("crypt", "aes", "cipher: null, none, xor, aes, aes-128, aes-192, 3des or aes-128-gcm"),
		key:   flags.String("key", "it's a secrect", "passphrase both ends share"),
		ds:    flags.Int("ds", 10, "FEC data shards; 0 turns FEC off"),
		ps:    flags.Int("ps", 3, "FEC parity shards"),
		mtu:   flags.Int("mtu", 1350, "largest datagram, framing included"),
	}
}

// modes are kcptun's presets: nodelay, interval (ms), resend, nc.
var modes = map[string][4]int{
	"normal": {0, 40, 2, 1},
	"fast":   {0, 30, 2, 1},
	"fast2":  {1, 20, 2, 1},
	"fast3":  {1, 10, 2, 1},
}

// Config is the kcp.Config the flags describe. The key is derived from the
// passphrase as kcptun derives it.
func (f Flags) Config() (kcp.Config, error) {
	m, ok := modes[*f.mode]
	if !ok {
		return kcp.Config{}, fmt.Errorf("unknown -mode %q", *f.mode)
	}
	key, err := pbkdf2.Key(sha1.New, *f.key, []byte("kcp-go"), 4096, 32)
	if err != nil {
		return kcp.Config{}, err
	}
	crypt, err := kcp.NewCrypt(*f.crypt, key)
	if err != nil {
		return kcp.Config{}, err
	}
	return kcp.Config{
		NoDelay:      m[0],
		Interval:     time.Duration(m[1]) * time.Millisecond,
		Resend:       m[2],
		NoCongestion: m[3] == 1,
		Crypt:        crypt,
		DataShards:   *f.ds,
		ParityShards: *f.ps,
		MTU:          *f.mtu,
	}, nil
}

// Describe is a one-line summary of the settings, for a banner.
func (f Flags) Describe() string {
	fec := "no FEC"
	if *f.ds > 0 {
		fec = fmt.Sprintf("FEC %d/%d", *f.ds, *f.ps)
	}
	return fmt.Sprintf("mode %s, crypt %s, %s", *f.mode, *f.crypt, fec)
}
