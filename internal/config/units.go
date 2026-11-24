package config

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// ByteSize is an amount of memory in bytes. In TOML it is written as a
// string of decimal digits with an optional unit suffix b, k, m or g
// (case-insensitive, binary multiples as Docker uses), such as "512m" or "1g".
type ByteSize int64

// Binary multiples accepted by ParseByteSize.
const (
	Byte     ByteSize = 1
	Kibibyte          = 1024 * Byte
	Mebibyte          = 1024 * Kibibyte
	Gibibyte          = 1024 * Mebibyte
)

// ParseByteSize parses a size such as "512m". A value without a suffix is a
// number of bytes. Errors quote s; configuration values are not secrets (see
// problems).
func ParseByteSize(s string) (ByteSize, error) {
	digits, unit := s, Byte
	if n := len(s); n > 0 {
		if u, ok := unitOf(s[n-1]); ok {
			digits, unit = s[:n-1], u
		}
	}
	if digits == "" || strings.TrimLeft(digits, "0123456789") != "" {
		return 0, fmt.Errorf("invalid size %q: want digits with an optional b, k, m or g suffix", s)
	}
	n, err := strconv.ParseInt(digits, 10, 64)
	if err != nil || n > math.MaxInt64/int64(unit) {
		return 0, fmt.Errorf("invalid size %q: too large", s)
	}
	return ByteSize(n) * unit, nil
}

// String formats b with the largest unit that divides it exactly.
func (b ByteSize) String() string {
	for _, u := range []struct {
		size   ByteSize
		suffix string
	}{{Gibibyte, "g"}, {Mebibyte, "m"}, {Kibibyte, "k"}} {
		if b != 0 && b%u.size == 0 {
			return strconv.FormatInt(int64(b/u.size), 10) + u.suffix
		}
	}
	return strconv.FormatInt(int64(b), 10) + "b"
}

func unitOf(suffix byte) (ByteSize, bool) {
	switch suffix {
	case 'b', 'B':
		return Byte, true
	case 'k', 'K':
		return Kibibyte, true
	case 'm', 'M':
		return Mebibyte, true
	case 'g', 'G':
		return Gibibyte, true
	}
	return 0, false
}

// parseDuration parses a Go duration string such as "10m". A bare number
// has no unit and is rejected rather than read as nanoseconds. Errors quote
// s like ParseByteSize.
func parseDuration(s string) (time.Duration, error) {
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q: want a value such as \"10m\" or \"90s\"", s)
	}
	return d, nil
}

// text captures any primitive TOML value (string, integer, float, boolean)
// as text, so a size or duration is parsed afterwards with its own message
// and an integer such as 536870912 is accepted as a size.
type text string

// UnmarshalText implements encoding.TextUnmarshaler.
func (t *text) UnmarshalText(b []byte) error {
	*t = text(b)
	return nil
}
