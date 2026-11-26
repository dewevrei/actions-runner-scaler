//go:build !race

package gate

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// timedWriter notes when the response status is written.
type timedWriter struct {
	*httptest.ResponseRecorder
	at *time.Time
}

func (w timedWriter) WriteHeader(code int) {
	*w.at = time.Now()
	w.ResponseRecorder.WriteHeader(code)
}

// TestEventVerdictWithinOneSecond: for a valid 32 MiB event body, the
// verdict is recorded and written within 1 s of the body's end, on the
// real clock (0002 SPEC R131). It is built without -race, which slows the
// decode many times over; run it alone with
// go test -count=1 -run TestEventVerdictWithinOneSecond ./internal/gate
func TestEventVerdictWithinOneSecond(t *testing.T) {
	head := `{"repository":{"id":7},"pull_request":{"head":{"repo":{"id":7}},"base":{"repo":{"id":7}}},"pad":"`
	tail := `"}`
	var b bytes.Buffer
	b.WriteString(head)
	b.WriteString(strings.Repeat("a", MaxEventPayload-len(head)-len(tail)))
	b.WriteString(tail)
	if b.Len() != MaxEventPayload {
		t.Fatalf("body is %d bytes, want %d", b.Len(), MaxEventPayload)
	}

	h := newHarness(t, nil)
	tok := h.token(t)
	h.table.add("pool-a", tok, VerdictPending)
	body := newCountingBody(b.Bytes())
	var wrote time.Time
	w := timedWriter{httptest.NewRecorder(), &wrote}

	h.srv.ServeHTTP(w, eventRequest("pool-a", tok.Reveal(), "pull_request", body))

	if w.Code != http.StatusOK || w.Body.String() != `{"verdict":"allow"}` {
		t.Fatalf("got %d %q, want 200 allow", w.Code, w.Body.String())
	}
	body.mu.Lock()
	eof := body.eofAt
	body.mu.Unlock()
	if eof.IsZero() || wrote.IsZero() {
		t.Fatalf("body end %v, response %v; want both", eof, wrote)
	}
	if d := wrote.Sub(eof); d > time.Second {
		t.Errorf("verdict written %v after the body ended, want 1s or less", d)
	} else {
		t.Logf("verdict written %v after the body ended", d)
	}
}
