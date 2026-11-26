package configfile

import (
	"bytes"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/BurntSushi/toml"

	"github.com/dewevrei/actions-runner-scaler/internal/config"
)

// Reasons AddTarget and RemoveTarget refuse (0003 SPEC R292). A returned
// error satisfies errors.Is with its reason. Their messages quote nothing
// from the file: a repository is shown only as the lower-case owner/repo of
// a valid URL.
var (
	ErrNotTOML    = errors.New("config file is not TOML")
	ErrDuplicate  = errors.New("repository is already in the config file")
	ErrNotFound   = errors.New("repository is not in the config file")
	ErrRepeated   = errors.New("repository is listed more than once")
	ErrLastTarget = errors.New("repository is the last target")
	// ErrShape means the file's targets are not written as [[targets]]
	// tables the byte editor can change, or the edit did not read back as
	// intended. The request itself is fine, so add-repo can show the block
	// to paste by hand.
	ErrShape = errors.New("config file targets cannot be edited safely")
)

// editRefusal is an error with its own message that satisfies errors.Is with
// kind.
type editRefusal struct {
	kind error
	msg  string
}

func refuseEdit(kind error, format string, args ...any) error {
	return &editRefusal{kind: kind, msg: fmt.Sprintf(format, args...)}
}

func (r *editRefusal) Error() string { return r.msg }

func (r *editRefusal) Unwrap() error { return r.kind }

// targetsHeader matches the header line of one [[targets]] table, with
// leading blanks, blanks inside the brackets and a trailing comment.
var targetsHeader = regexp.MustCompile(`^[ \t]*\[\[[ \t]*targets[ \t]*\]\][ \t]*(#.*)?\r?\n?$`)

// AddTarget returns data with one [[targets]] block for entry added at the
// end: a blank line and TargetBlock, after a newline if the last line has
// none. Every byte of data stays (0003 SPEC R290).
//
// It refuses (R292) when data is not TOML, already lists a repository twice
// or entry's repository (compared as R233 does), writes its targets other
// than as [[targets]] tables, or does not read back as data's targets plus
// entry. entry.URL must be a repository URL and, like entry.TokenFile,
// valid UTF-8.
func AddTarget(data []byte, entry TargetEntry) ([]byte, error) {
	key, ok := config.RepositoryKey(entry.URL)
	if !ok {
		return nil, errors.New("configfile: entry URL is not a repository URL")
	}
	if !utf8.ValidString(entry.URL) || !utf8.ValidString(entry.TokenFile) {
		return nil, errors.New("configfile: entry is not valid UTF-8")
	}
	doc, targets, err := parseTargets(data)
	if err != nil {
		return nil, err
	}
	keys, err := targetKeys(targets)
	if err != nil {
		return nil, err
	}
	if slices.Contains(keys, key) {
		return nil, refuseEdit(ErrDuplicate, "repository %s is already in the config file", key)
	}
	if _, err := headerLines(data, len(targets)); err != nil {
		return nil, err
	}
	edited := appendTargetBlock(data, TargetBlock(entry))
	return verifiedEdit(edited, doc, append(slices.Clone(targets), entryTable(entry)))
}

// RemoveTarget returns data without the [[targets]] block of the repository
// key, the lower-case owner/repo that config.RepositoryKey returns. The
// block runs from its header line through its last key line, which is the
// last line before the next line starting with "[" that is not blank or a
// comment. The blank and comment lines after it, and every other byte, stay
// (0003 SPEC R291).
//
// It refuses (R292) when data is not TOML, lists a repository twice, lacks
// key or has only that target, writes its targets other than as [[targets]]
// tables, or does not read back as data's targets less that one.
func RemoveTarget(data []byte, key string) ([]byte, error) {
	doc, targets, err := parseTargets(data)
	if err != nil {
		return nil, err
	}
	keys, err := targetKeys(targets)
	if err != nil {
		return nil, err
	}
	i := slices.Index(keys, key)
	if key == "" || i < 0 {
		return nil, refuseEdit(ErrNotFound, "repository %s is not in the config file", key)
	}
	if len(targets) == 1 {
		return nil, refuseEdit(ErrLastTarget, "removing %s would leave the config file without a repository; add another one first", key)
	}
	headers, err := headerLines(data, len(targets))
	if err != nil {
		return nil, err
	}
	lines := splitKeepingEnds(data)
	start := headers[i]
	end := blockEnd(lines, start)
	edited := slices.Concat(slices.Concat(lines[:start]...), slices.Concat(lines[end:]...))
	return verifiedEdit(edited, doc, slices.Delete(slices.Clone(targets), i, i+1))
}

// parseTargets decodes data and returns the document and its targets. A
// "targets" that is not a list of tables, such as one table written with
// dotted keys, is a shape refusal.
func parseTargets(data []byte) (doc map[string]any, targets []map[string]any, err error) {
	doc, err = decodeFileDocument(data)
	if err != nil {
		var pe toml.ParseError
		if errors.As(err, &pe) {
			return nil, nil, refuseEdit(ErrNotTOML, "config file is not TOML (line %d, column %d)", pe.Position.Line, pe.Position.Col)
		}
		return nil, nil, refuseEdit(ErrNotTOML, "config file is not TOML")
	}
	v, present := doc["targets"]
	if !present {
		return doc, nil, nil
	}
	targets, ok := asTableList(v)
	if !ok {
		return nil, nil, refuseEdit(ErrShape, "the targets of the config file are not [[targets]] tables")
	}
	return doc, targets, nil
}

// asTableList returns v as a list of tables: an array of tables, or an inline
// array of inline tables, which the header count later refuses. An inline
// array still lists the repositories it holds, so a duplicate is found
// before the shape is refused.
func asTableList(v any) ([]map[string]any, bool) {
	switch v := v.(type) {
	case []map[string]any:
		return v, true
	case []any:
		tables := make([]map[string]any, len(v))
		for i, item := range v {
			table, ok := item.(map[string]any)
			if !ok {
				return nil, false
			}
			tables[i] = table
		}
		return tables, true
	}
	return nil, false
}

func decodeFileDocument(data []byte) (map[string]any, error) {
	var doc map[string]any
	if _, err := toml.NewDecoder(bytes.NewReader(data)).Decode(&doc); err != nil {
		return nil, err
	}
	return doc, nil
}

// targetKeys returns each target's repository key, empty for a target whose
// url is not a valid repository URL (it has its own configuration error),
// and refuses a file that lists a repository twice.
func targetKeys(targets []map[string]any) ([]string, error) {
	keys := make([]string, len(targets))
	seen := make(map[string]bool, len(targets))
	for i, t := range targets {
		url, _ := t["url"].(string)
		key, ok := config.RepositoryKey(url)
		if !ok {
			continue
		}
		if seen[key] {
			return nil, refuseEdit(ErrRepeated, "the config file lists repository %s more than once; remove the extra entries first", key)
		}
		seen[key] = true
		keys[i] = key
	}
	return keys, nil
}

// headerLines returns the index of every [[targets]] header line of data,
// which must be n. A different count means a target is written another way
// (an inline array) or a header-looking line sits inside a string.
func headerLines(data []byte, n int) ([]int, error) {
	var headers []int
	for i, line := range splitKeepingEnds(data) {
		if targetsHeader.Match(line) {
			headers = append(headers, i)
		}
	}
	if len(headers) != n {
		return nil, refuseEdit(ErrShape, "the config file has %d [[targets]] header lines for %d targets", len(headers), n)
	}
	return headers, nil
}

// splitKeepingEnds splits data into lines, each with its line ending.
func splitKeepingEnds(data []byte) [][]byte {
	lines := bytes.SplitAfter(data, []byte("\n"))
	if n := len(lines); n > 0 && len(lines[n-1]) == 0 {
		lines = lines[:n-1]
	}
	return lines
}

// blockEnd returns the index after the last key line of the block whose
// header is lines[start].
func blockEnd(lines [][]byte, start int) int {
	end := start + 1
	for end < len(lines) && !startsTable(lines[end]) {
		end++
	}
	for end > start+1 && blankOrComment(lines[end-1]) {
		end--
	}
	return end
}

// startsTable reports whether line starts, after blanks, with "[": a table
// header.
func startsTable(line []byte) bool {
	return bytes.HasPrefix(bytes.TrimLeft(line, " \t"), []byte("["))
}

func blankOrComment(line []byte) bool {
	s := strings.TrimSpace(string(line))
	return s == "" || strings.HasPrefix(s, "#")
}

// appendTargetBlock returns data followed by a blank line and block, with a
// newline first when the last line has none.
func appendTargetBlock(data []byte, block string) []byte {
	out := make([]byte, 0, len(data)+len(block)+2)
	out = append(out, data...)
	if len(data) > 0 {
		if data[len(data)-1] != '\n' {
			out = append(out, '\n')
		}
		out = append(out, '\n')
	}
	return append(out, block...)
}

// entryTable is the decoded form of the block TargetBlock writes for e.
func entryTable(e TargetEntry) map[string]any {
	t := map[string]any{"url": e.URL, "max-runners": int64(e.MaxRunners)}
	if e.Idle != nil {
		t["idle"] = int64(*e.Idle)
	}
	if e.TokenFile != "" {
		t["token-file"] = e.TokenFile
	}
	return t
}

// verifiedEdit returns edited when it reads back as doc with its targets
// replaced by targets, and refuses otherwise (0003 SPEC R292). It compares
// the whole document, so a changed value anywhere, not only in the targets,
// is found.
func verifiedEdit(edited []byte, doc map[string]any, targets []map[string]any) ([]byte, error) {
	want := make(map[string]any, len(doc)+1)
	for k, v := range doc {
		want[k] = v
	}
	want["targets"] = targets
	got, err := decodeFileDocument(edited)
	if err != nil || !reflect.DeepEqual(got, want) {
		return nil, refuseEdit(ErrShape, "the edited config file would not read back as the original with exactly this change")
	}
	return edited, nil
}
