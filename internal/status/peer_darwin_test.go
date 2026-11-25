package status

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// QueryPeer returns the report and the pid of the serving process, here
// this test process. The service package's tests check a server in another
// process.
func TestQueryPeer(t *testing.T) {
	path := filepath.Join(shortDir(t), "status.sock")
	serve(t, path, staticReporter{Report{State: Draining}})
	r, pid, err := QueryPeer(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if r.State != Draining {
		t.Errorf("state = %q, want draining", r.State)
	}
	if pid != os.Getpid() {
		t.Errorf("pid = %d, want %d", pid, os.Getpid())
	}
}

func TestQueryPeerNotRunning(t *testing.T) {
	path := filepath.Join(shortDir(t), "status.sock")
	_, pid, err := QueryPeer(context.Background(), path)
	if !errors.Is(err, ErrNotRunning) || pid != 0 {
		t.Errorf("QueryPeer = %d, %v, want 0 and ErrNotRunning", pid, err)
	}
}
