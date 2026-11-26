package configfile

import (
	"reflect"
	"testing"

	"github.com/BurntSushi/toml"
)

func intPtr(n int) *int { return &n }

func TestTargetBlock(t *testing.T) {
	tests := []struct {
		name  string
		entry TargetEntry
		want  string
	}{
		{
			name:  "defaults only",
			entry: TargetEntry{URL: "https://github.com/o/r", MaxRunners: 1},
			want:  "[[targets]]\nurl = \"https://github.com/o/r\"\nmax-runners = 1\n",
		},
		{
			name:  "idle zero",
			entry: TargetEntry{URL: "https://github.com/o/r", MaxRunners: 3, Idle: intPtr(0)},
			want:  "[[targets]]\nurl = \"https://github.com/o/r\"\nmax-runners = 3\nidle = 0\n",
		},
		{
			name:  "idle and token file",
			entry: TargetEntry{URL: "https://github.com/o/r", MaxRunners: 2, Idle: intPtr(1), TokenFile: "/home/u/.config/t"},
			want:  "[[targets]]\nurl = \"https://github.com/o/r\"\nmax-runners = 2\nidle = 1\ntoken-file = \"/home/u/.config/t\"\n",
		},
		{
			name:  "token file only",
			entry: TargetEntry{URL: "https://github.com/o/r", MaxRunners: 1, TokenFile: "/t"},
			want:  "[[targets]]\nurl = \"https://github.com/o/r\"\nmax-runners = 1\ntoken-file = \"/t\"\n",
		},
		{
			name:  "quoting",
			entry: TargetEntry{URL: "https://github.com/o/r", MaxRunners: 1, TokenFile: `/a "b"\c` + "\t\x01\x7fé"},
			want:  "[[targets]]\nurl = \"https://github.com/o/r\"\nmax-runners = 1\ntoken-file = \"/a \\\"b\\\"\\\\c\\t\\u0001\\u007Fé\"\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := TargetBlock(tt.entry)
			if got != tt.want {
				t.Fatalf("TargetBlock =\n%s\nwant\n%s", got, tt.want)
			}
			assertDecodes(t, got, tt.entry)
		})
	}
}

// assertDecodes checks that block reads back as entry.
func assertDecodes(t *testing.T, block string, entry TargetEntry) {
	t.Helper()
	var file struct {
		Targets []struct {
			URL        string `toml:"url"`
			MaxRunners int    `toml:"max-runners"`
			Idle       *int   `toml:"idle"`
			TokenFile  string `toml:"token-file"`
		} `toml:"targets"`
	}
	if _, err := toml.Decode(block, &file); err != nil {
		t.Fatalf("decode: %v\n%s", err, block)
	}
	if len(file.Targets) != 1 {
		t.Fatalf("decoded %d targets", len(file.Targets))
	}
	d := file.Targets[0]
	got := TargetEntry{URL: d.URL, MaxRunners: d.MaxRunners, Idle: d.Idle, TokenFile: d.TokenFile}
	if !reflect.DeepEqual(got, entry) {
		t.Fatalf("decoded %+v, want %+v", got, entry)
	}
}
