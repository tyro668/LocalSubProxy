package connector

import (
	"context"
	"testing"
	"time"
)

func fakeLogin(probe func() ([]string, error)) (*CodexLogin, chan struct{}) {
	reaped := make(chan struct{})
	_, cancel := context.WithCancel(context.Background())
	return &CodexLogin{closeFn: func() { close(reaped) }, cancel: cancel, done: make(chan struct{}), probe: probe}, reaped
}

// Review7 #9: a hung probe must not block Close (cancel), and a second Check
// must not queue behind it.
func TestCodexLoginCheckDoesNotBlockCancel(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	started := make(chan struct{})
	l, reaped := fakeLogin(func() ([]string, error) { close(started); <-release; return nil, nil })
	errc := make(chan error, 1)
	go func() { _, _, err := l.Check(); errc <- err }()
	<-started
	// concurrent check returns at once (pending), not after the hung probe
	quick := make(chan struct{})
	go func() { l.Check(); close(quick) }()
	select {
	case <-quick:
	case <-time.After(2 * time.Second):
		t.Fatal("second Check blocked behind in-flight probe")
	}
	closed := make(chan struct{})
	go func() { l.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("Close waited for the in-flight Check")
	}
	select {
	case err := <-errc:
		if err == nil {
			t.Fatal("cancelled check reported success")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("in-flight Check did not return after Close")
	}
	<-reaped
}

func TestCodexLoginCheckDeadline(t *testing.T) {
	old := codexCheckTimeout
	codexCheckTimeout = 50 * time.Millisecond
	defer func() { codexCheckTimeout = old }()
	release := make(chan struct{})
	defer close(release)
	l, reaped := fakeLogin(func() ([]string, error) { <-release; return nil, nil })
	start := time.Now()
	if _, _, err := l.Check(); err == nil {
		t.Fatal("hung probe did not time out")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("deadline not enforced")
	}
	<-reaped // session closed after timeout
	if _, _, err := l.Check(); err == nil {
		t.Fatal("check after timeout should report closed")
	}
}

func TestCodexLoginCheckComplete(t *testing.T) {
	l, reaped := fakeLogin(func() ([]string, error) { return []string{"gpt"}, nil })
	m, ok, err := l.Check()
	if err != nil || !ok || len(m) != 1 {
		t.Fatalf("got %v %v %v", m, ok, err)
	}
	<-reaped
}
