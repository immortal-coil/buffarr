package config

import "testing"

func TestParseCacheSize(t *testing.T) {
	for in, want := range map[string]int64{
		"":          0,
		"500g":      500 << 30,
		"2T":        2 << 40,
		"1024":      1024,
		"abc":       0,
		"-5g":       0, // negative
		"99999999t": 0, // overflows int64
	} {
		if got := ParseCacheSize(in); got != want {
			t.Errorf("ParseCacheSize(%q) = %d, want %d", in, got, want)
		}
	}
}
