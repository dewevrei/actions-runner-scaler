package docker

import (
	"testing"

	"github.com/dewevrei/actions-runner-scaler/internal/config"
)

const (
	mib = int64(config.Mebibyte)
	gib = 1024 * mib
)

func lim(cpus float64, mem int64) Limits {
	return Limits{CPUs: cpus, Memory: config.ByteSize(mem)}
}

func TestRunnerShare(t *testing.T) {
	got := RunnerShare(lim(0.5, 512*mib), lim(1, 768*mib))
	want := Share{NanoCPUs: 1_500_000_000, Memory: 1280 * mib}
	if got != want {
		t.Errorf("RunnerShare = %+v, want %+v", got, want)
	}
}

// TestSlotShare proves that one slot is the runner container plus the
// BuildKit container with buildkit.enabled true, and the runner container
// alone with it false: on the same Docker capacity the limits differ.
func TestSlotShare(t *testing.T) {
	mac := Capacity{NCPU: 10, MemTotal: 8321515520}
	for _, tt := range []struct {
		enabled   bool
		want      Share
		wantLimit int
		wantText  string
	}{
		{true, Share{NanoCPUs: 3e9, Memory: 3 * gib}, 2,
			"3 CPUs (runner.cpus 1 + buildkit.cpus 2) and 3221225472 bytes (runner.memory 1g + buildkit.memory 2g)"},
		{false, Share{NanoCPUs: 1e9, Memory: gib}, 7,
			"1 CPUs (runner.cpus 1) and 1073741824 bytes (runner.memory 1g)"},
	} {
		cfg := config.Default()
		cfg.BuildKit.Enabled = tt.enabled
		got := SlotShare(&cfg)
		if got != tt.want {
			t.Errorf("enabled %v: SlotShare = %+v, want %+v", tt.enabled, got, tt.want)
		}
		if limit := ServerLimit(mac, got); limit != tt.wantLimit {
			t.Errorf("enabled %v: ServerLimit = %d, want %d", tt.enabled, limit, tt.wantLimit)
		}
		if text := SlotShareText(&cfg, got); text != tt.wantText {
			t.Errorf("enabled %v: SlotShareText = %q, want %q", tt.enabled, text, tt.wantText)
		}
	}
}

// TestServerLimit covers 0002 SPEC R201 and R202.
func TestServerLimit(t *testing.T) {
	mac := Capacity{NCPU: 10, MemTotal: 8321515520}
	tests := []struct {
		name   string
		c      Capacity
		runner Limits
		bk     Limits
		want   int
	}{
		{"Mac defaults: CPU 3, memory 2", mac, lim(1, gib), lim(2, 2*gib), 2},
		{"R253 test allocation", mac, lim(0.5, 512*mib), lim(1, 768*mib), 6},
		{"fractional sum 0.5+1 on 10 CPU", Capacity{NCPU: 10, MemTotal: 1 << 40}, lim(0.5, mib), lim(1, mib), 6},
		{"0.1+0.2 on 3 CPU is 10", Capacity{NCPU: 3, MemTotal: 1 << 40}, lim(0.1, mib), lim(0.2, mib), 10},
		{"CPU is the smaller", Capacity{NCPU: 4, MemTotal: 64 * gib}, lim(1, gib), lim(1, gib), 2},
		{"memory is the smaller", Capacity{NCPU: 64, MemTotal: 4 * gib}, lim(1, gib), lim(1, gib), 2},
		{"exact fit on both", Capacity{NCPU: 6, MemTotal: 6 * gib}, lim(1, gib), lim(2, 2*gib), 2},
		{"one byte short of two", Capacity{NCPU: 6, MemTotal: 6*gib - 1}, lim(1, gib), lim(2, 2*gib), 1},
		{"CPU short of two", Capacity{NCPU: 5, MemTotal: 64 * gib}, lim(1, gib), lim(1.5, gib), 2},
		{"CPU one nano short of two", Capacity{NCPU: 5, MemTotal: 64 * gib}, lim(1, gib), lim(1.500000001, gib), 1},
		{"share above NCPU gives 0", Capacity{NCPU: 2, MemTotal: 64 * gib}, lim(1, gib), lim(2, gib), 0},
		{"share above memory gives 0", Capacity{NCPU: 64, MemTotal: 2 * gib}, lim(1, gib), lim(1, 2*gib), 0},
		{"single key above NCPU gives 0", Capacity{NCPU: 2, MemTotal: 64 * gib}, lim(3, gib), lim(0.5, gib), 0},
	}
	for _, tt := range tests {
		got := ServerLimit(tt.c, RunnerShare(tt.runner, tt.bk))
		if got != tt.want {
			t.Errorf("%s: ServerLimit = %d, want %d", tt.name, got, tt.want)
		}
	}
}

func TestServerLimitZeroShare(t *testing.T) {
	c := Capacity{NCPU: 10, MemTotal: 8 * gib}
	for _, s := range []Share{
		{},
		{NanoCPUs: 0, Memory: gib},
		{NanoCPUs: 1e9, Memory: 0},
		{NanoCPUs: -1, Memory: gib},
		{NanoCPUs: 1e9, Memory: -1},
	} {
		if got := ServerLimit(c, s); got != 0 {
			t.Errorf("ServerLimit(%+v) = %d, want 0", s, got)
		}
	}
	if got := ServerLimit(Capacity{}, Share{NanoCPUs: 1e9, Memory: gib}); got != 0 {
		t.Errorf("empty capacity: ServerLimit = %d, want 0", got)
	}
}
