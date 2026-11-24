package config

import (
	"math"
	"testing"
	"time"
)

func TestParseByteSize(t *testing.T) {
	tests := []struct {
		in      string
		want    ByteSize
		wantErr bool
	}{
		{in: "0", want: 0},
		{in: "1024", want: 1024},
		{in: "10b", want: 10},
		{in: "1k", want: 1024},
		{in: "512m", want: 512 << 20},
		{in: "512M", want: 512 << 20},
		{in: "1g", want: 1 << 30},
		{in: "2G", want: 2 << 30},
		{in: "8589934591g", want: 8589934591 << 30},
		{in: "8589934592g", wantErr: true}, // overflows int64
		{in: "99999999999999999999", wantErr: true},
		{in: "", wantErr: true},
		{in: "g", wantErr: true},
		{in: "-1g", wantErr: true},
		{in: "+1g", wantErr: true},
		{in: "1.5g", wantErr: true},
		{in: "1gb", wantErr: true},
		{in: "1t", wantErr: true},
		{in: " 1g", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := ParseByteSize(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseByteSize(%q) = %d, want error", tt.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseByteSize(%q) error: %v", tt.in, err)
			}
			if got != tt.want {
				t.Errorf("ParseByteSize(%q) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}

func TestByteSizeString(t *testing.T) {
	tests := []struct {
		in   ByteSize
		want string
	}{
		{0, "0b"},
		{1023, "1023b"},
		{1024, "1k"},
		{256 * Mebibyte, "256m"},
		{256*Mebibyte - 1, "268435455b"},
		{2 * Gibibyte, "2g"},
		{1536 * Mebibyte, "1536m"},
		{math.MaxInt64, "9223372036854775807b"},
	}
	for _, tt := range tests {
		if got := tt.in.String(); got != tt.want {
			t.Errorf("ByteSize(%d).String() = %q, want %q", int64(tt.in), got, tt.want)
		}
	}
}

func TestParseDuration(t *testing.T) {
	tests := []struct {
		in      string
		want    time.Duration
		wantErr bool
	}{
		{in: "10m", want: 10 * time.Minute},
		{in: "10m0s", want: 10 * time.Minute},
		{in: "90s", want: 90 * time.Second},
		{in: "1h30m", want: 90 * time.Minute},
		{in: "0s", want: 0},
		{in: "-1s", want: -time.Second}, // parsed; Validate rejects it
		{in: "600", wantErr: true},
		{in: "", wantErr: true},
		{in: "ten minutes", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := parseDuration(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseDuration(%q) = %s, want error", tt.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseDuration(%q) error: %v", tt.in, err)
			}
			if got != tt.want {
				t.Errorf("parseDuration(%q) = %s, want %s", tt.in, got, tt.want)
			}
		})
	}
}
