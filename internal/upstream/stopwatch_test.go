package upstream

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// answering serves every request after delay.
func answering(t *testing.T, delay time.Duration) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(delay):
		case <-r.Context().Done(): // the caller gave up
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestStopwatchCountsTheAnswerNotTheConnection(t *testing.T) {
	const dial, answer = 300 * time.Millisecond, 60 * time.Millisecond
	srv := answering(t, answer)
	// Opening a connection is slow; the upstream itself answers in 60 ms.
	hc := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			time.Sleep(dial)
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
	}}
	c := New(hc, srv.URL, nil)

	for _, attempt := range []string{"on a new connection", "on the connection kept open"} {
		var sw Stopwatch
		started := time.Now()
		if _, err := c.Get(WithStopwatch(context.Background(), &sw), "/quote"); err != nil {
			t.Fatal(err)
		}
		wall := time.Since(started)
		if got := sw.Elapsed(); got < answer || got > answer+dial/2 {
			t.Errorf("%s: stopwatch = %v (the call took %v), want about the %v the upstream needed", attempt, got, wall, answer)
		}
		if attempt == "on a new connection" && wall < dial {
			t.Fatalf("the dial was not slow (%v): the test proves nothing", wall)
		}
	}
}

func TestStopwatchAddsUpRequestsAndLeavesOutWhatHappensBetweenThem(t *testing.T) {
	const answer = 40 * time.Millisecond
	c := New(http.DefaultClient, answering(t, answer).URL, nil)
	var sw Stopwatch
	ctx := WithStopwatch(context.Background(), &sw)
	if _, err := c.Get(ctx, "/route"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond) // the caller's own work
	if _, err := c.PostJSON(ctx, "/build", map[string]string{}); err != nil {
		t.Fatal(err)
	}
	if got := sw.Elapsed(); got < 2*answer || got > 2*answer+200*time.Millisecond {
		t.Errorf("stopwatch = %v, want about the %v of two answers", got, 2*answer)
	}
}

func TestStopwatchCountsARequestThatFails(t *testing.T) {
	c := New(http.DefaultClient, answering(t, 2*time.Second).URL, nil)
	var sw Stopwatch
	ctx, cancel := context.WithTimeout(WithStopwatch(context.Background(), &sw), 150*time.Millisecond)
	defer cancel()
	if _, err := c.Get(ctx, "/quote"); err == nil {
		t.Fatal("expected the deadline to end the request")
	}
	if got := sw.Elapsed(); got < 100*time.Millisecond || got > time.Second {
		t.Errorf("stopwatch = %v, want about the 150ms until the request was given up", got)
	}
}

func TestRequestsWithoutAStopwatchAreNotTimed(t *testing.T) {
	c := New(http.DefaultClient, answering(t, 0).URL, nil)
	if _, err := c.Get(context.Background(), "/quote"); err != nil {
		t.Fatal(err)
	}
}
