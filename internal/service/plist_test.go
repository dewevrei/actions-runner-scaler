package service

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var update = flag.Bool("update", false, "rewrite testdata golden files")

func testJob() Job {
	return Job{
		Label:        Label,
		Program:      "/opt/scaler/bin/actions-runner-scaler",
		ConfigPath:   "/Users/someone/.config/actions-runner-scaler/config.toml",
		Home:         "/Users/someone",
		DrainTimeout: 10 * time.Minute,
	}
}

func TestPlistGolden(t *testing.T) {
	got, err := Plist(testJob())
	if err != nil {
		t.Fatal(err)
	}
	golden := filepath.Join("testdata", "agent.plist")
	if *update {
		if err := os.WriteFile(golden, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("Plist differs from %s (run with -update after checking):\n%s", golden, got)
	}
}

func TestExitTimeOut(t *testing.T) {
	tests := []struct {
		drain time.Duration
		want  int64
	}{
		{10 * time.Minute, 660},
		{time.Hour, 3660},
		{time.Second, 61},
		{1500 * time.Millisecond, 62}, // rounded up so the drain is never cut short
		{time.Nanosecond, 61},
		{time.Duration(1<<63 - 1), maxSeconds + 1 + 60},
	}
	for _, tt := range tests {
		j := testJob()
		j.DrainTimeout = tt.drain
		if got := j.ExitTimeOut(); got != tt.want {
			t.Errorf("ExitTimeOut for drain %s = %d, want %d", tt.drain, got, tt.want)
		}
	}
}

// The plist read back by an independent parser (plutil) has every key with
// the type launchd.plist(5) gives it.
func TestPlistFieldsViaPlutil(t *testing.T) {
	plutil := lookPlutil(t)
	j := testJob()
	j.DrainTimeout = 90 * time.Second
	data, err := Plist(j)
	if err != nil {
		t.Fatal(err)
	}
	path := writeTemp(t, data)
	out, err := exec.Command(plutil, "-convert", "json", "-o", "-", path).Output()
	if err != nil {
		t.Fatalf("plutil -convert json: %v", err)
	}
	var got struct {
		Label                string
		ProgramArguments     []string
		RunAtLoad            *bool
		KeepAlive            map[string]any
		EnvironmentVariables map[string]any
		ThrottleInterval     json.Number
		ExitTimeOut          json.Number
		StandardOutPath      string
		StandardErrorPath    string
	}
	dec := json.NewDecoder(bytes.NewReader(out))
	dec.UseNumber()
	dec.DisallowUnknownFields()
	if err := dec.Decode(&got); err != nil {
		t.Fatalf("decode %s: %v", out, err)
	}
	if got.Label != Label {
		t.Errorf("Label = %q", got.Label)
	}
	wantArgs := []string{j.Program, "run", "-config", j.ConfigPath}
	if strings.Join(got.ProgramArguments, "\x00") != strings.Join(wantArgs, "\x00") {
		t.Errorf("ProgramArguments = %q, want %q", got.ProgramArguments, wantArgs)
	}
	if got.RunAtLoad == nil || !*got.RunAtLoad {
		t.Errorf("RunAtLoad = %v, want true", got.RunAtLoad)
	}
	if v, ok := got.KeepAlive["SuccessfulExit"].(bool); !ok || v || len(got.KeepAlive) != 1 {
		t.Errorf("KeepAlive = %v, want {SuccessfulExit: false}", got.KeepAlive)
	}
	if got.ThrottleInterval != "60" {
		t.Errorf("ThrottleInterval = %s, want 60", got.ThrottleInterval)
	}
	if got.ExitTimeOut != "150" {
		t.Errorf("ExitTimeOut = %s, want 150 (90s drain + 60)", got.ExitTimeOut)
	}
	if len(got.EnvironmentVariables) != 1 || got.EnvironmentVariables["HOME"] != j.Home {
		t.Errorf("EnvironmentVariables = %v, want only HOME=%s", got.EnvironmentVariables, j.Home)
	}
	logDir := "/Users/someone/Library/Logs/actions-runner-scaler"
	if got.StandardOutPath != logDir+"/stdout.log" || got.StandardErrorPath != logDir+"/stderr.log" {
		t.Errorf("log paths = %q, %q", got.StandardOutPath, got.StandardErrorPath)
	}
	// plutil -convert json writes integers without quotes and booleans as
	// true/false, so the types above are the plist's.
	for _, typed := range []string{`"ThrottleInterval":60`, `"ExitTimeOut":150`, `"RunAtLoad":true`, `"SuccessfulExit":false`} {
		if !bytes.Contains(compactJSON(t, out), []byte(typed)) {
			t.Errorf("plutil JSON lacks %s: %s", typed, out)
		}
	}
}

// Paths with XML special characters come back unchanged, and plutil -lint
// accepts the file.
func TestPlistEscaping(t *testing.T) {
	j := testJob()
	j.Program = `/opt/a&b <c> "d" 'e'/scaler`
	j.ConfigPath = "/Users/someone/설정 & more/config.toml"
	j.Home = "/Users/a<b>&c"
	data, err := Plist(j)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("a&b")) || !bytes.Contains(data, []byte("a&amp;b &lt;c&gt; &#34;d&#34; &#39;e&#39;")) {
		t.Errorf("program path not escaped:\n%s", data)
	}
	plutil := lookPlutil(t)
	path := writeTemp(t, data)
	if out, err := exec.Command(plutil, "-lint", path).CombinedOutput(); err != nil {
		t.Fatalf("plutil -lint: %v: %s", err, out)
	}
	out, err := exec.Command(plutil, "-extract", "ProgramArguments", "json", "-o", "-", path).Output()
	if err != nil {
		t.Fatalf("plutil -extract: %v", err)
	}
	var args []string
	if err := json.Unmarshal(out, &args); err != nil {
		t.Fatal(err)
	}
	if len(args) != 4 || args[0] != j.Program || args[3] != j.ConfigPath {
		t.Errorf("ProgramArguments = %q, want program %q and config %q", args, j.Program, j.ConfigPath)
	}
	out, err = exec.Command(plutil, "-extract", "EnvironmentVariables.HOME", "raw", "-o", "-", path).Output()
	if err != nil {
		t.Fatalf("plutil -extract HOME: %v", err)
	}
	if got := strings.TrimSuffix(string(out), "\n"); got != j.Home {
		t.Errorf("HOME = %q, want %q", got, j.Home)
	}
}

func TestPlistLint(t *testing.T) {
	plutil := lookPlutil(t)
	data, err := Plist(testJob())
	if err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(plutil, "-lint", writeTemp(t, data)).CombinedOutput(); err != nil {
		t.Fatalf("plutil -lint: %v: %s", err, out)
	}
}

func TestPlistRejectsInvalidJob(t *testing.T) {
	tests := []struct {
		name   string
		change func(*Job)
	}{
		{"empty label", func(j *Job) { j.Label = "" }},
		{"label with slash", func(j *Job) { j.Label = "a/b" }},
		{"relative program", func(j *Job) { j.Program = "bin/scaler" }},
		{"relative config", func(j *Job) { j.ConfigPath = "config.toml" }},
		{"relative home", func(j *Job) { j.Home = "someone" }},
		{"empty home", func(j *Job) { j.Home = "" }},
		{"home with control character", func(j *Job) { j.Home = "/Users/a\x02" }},
		{"zero drain", func(j *Job) { j.DrainTimeout = 0 }},
		{"ExitTimeOut past int32", func(j *Job) { j.DrainTimeout = time.Duration(maxExitTimeOut-59) * time.Second }},
		{"maximum drain", func(j *Job) { j.DrainTimeout = time.Duration(1<<63 - 1) }},
		{"control character", func(j *Job) { j.ConfigPath = "/tmp/a\x01b" }},
		{"invalid UTF-8", func(j *Job) { j.Program = "/tmp/\xff" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			j := testJob()
			tt.change(&j)
			if data, err := Plist(j); err == nil {
				t.Errorf("Plist succeeded:\n%s", data)
			}
		})
	}
}

// The largest accepted drain gives ExitTimeOut exactly math.MaxInt32.
func TestPlistExitTimeOutCap(t *testing.T) {
	j := testJob()
	j.DrainTimeout = time.Duration(maxExitTimeOut-60) * time.Second
	data, err := Plist(j)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte("<integer>2147483647</integer>")) {
		t.Errorf("plist lacks ExitTimeOut 2147483647:\n%s", data)
	}
}

func TestExitTimeOutOf(t *testing.T) {
	j := testJob()
	j.DrainTimeout = 25 * time.Minute
	data, err := Plist(j)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := exitTimeOutOf(data); !ok || got != 1560*time.Second {
		t.Errorf("exitTimeOutOf = %s, %v, want 1560s, true", got, ok)
	}
	for _, bad := range []string{
		"",
		"not xml",
		`<plist><dict><key>Label</key><string>x</string></dict></plist>`,
		`<plist><dict><key>ExitTimeOut</key><integer>abc</integer></dict></plist>`,
		`<plist><dict><key>ExitTimeOut</key><integer>0</integer></dict></plist>`,
		// Nested ExitTimeOut is not the job's.
		`<plist><dict><key>KeepAlive</key><dict><key>ExitTimeOut</key><integer>5</integer></dict></dict></plist>`,
		// A key followed by a non-integer value does not pair with a later integer.
		`<plist><dict><key>ExitTimeOut</key><string>5</string><integer>7</integer></dict></plist>`,
	} {
		if got, ok := exitTimeOutOf([]byte(bad)); ok {
			t.Errorf("exitTimeOutOf(%q) = %s, true, want false", bad, got)
		}
	}
}

func lookPlutil(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("plutil")
	if err != nil {
		t.Skip("plutil not available")
	}
	return p
}

func writeTemp(t *testing.T, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agent.plist")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func compactJSON(t *testing.T, data []byte) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := json.Compact(&b, data); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}
