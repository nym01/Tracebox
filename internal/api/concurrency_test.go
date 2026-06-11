package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nym01/goboxd/internal/runner"
)

// spyRunner counts how many times Run is called and returns a fixed result.
type spyRunner struct {
	calls  int
	result runner.RunResult
}

func (s *spyRunner) Run(_ context.Context, _ runner.RunSpec) (runner.RunResult, error) {
	s.calls++
	return s.result, nil
}

// blockerRunner signals started then blocks until hold is closed.
type blockerRunner struct {
	hold    chan struct{}
	started chan struct{}
	result  runner.RunResult
}

func (b *blockerRunner) Run(_ context.Context, _ runner.RunSpec) (runner.RunResult, error) {
	b.started <- struct{}{}
	<-b.hold
	return b.result, nil
}

func newRunRequest() *http.Request {
	const body = `{"language":"py3","source":"print('hi')","tests":[{"stdin":"","expected_stdout":"hi\n"}]}`
	req := httptest.NewRequest(http.MethodPost, "/run", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return req
}

// TestSemaphoreWithinLimit verifies requests within the concurrency limit proceed immediately.
func TestSemaphoreWithinLimit(t *testing.T) {
	origRunner := defaultRunner
	origSem := sem
	defer func() { defaultRunner = origRunner; sem = origSem }()

	sem = make(chan struct{}, 2)
	spy := &spyRunner{result: runner.RunResult{ExitCode: 0, Stdout: "hi\n"}}
	defaultRunner = spy

	w := httptest.NewRecorder()
	run(w, newRunRequest())

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if spy.calls != 1 {
		t.Errorf("expected runner called once, got %d", spy.calls)
	}
}

// TestSemaphoreQueuing verifies requests beyond the limit queue and eventually process.
func TestSemaphoreQueuing(t *testing.T) {
	origRunner := defaultRunner
	origSem := sem
	defer func() { defaultRunner = origRunner; sem = origSem }()

	const limit = 2
	sem = make(chan struct{}, limit)

	hold := make(chan struct{})
	started := make(chan struct{}, 10)
	defaultRunner = &blockerRunner{
		hold:    hold,
		started: started,
		result:  runner.RunResult{ExitCode: 0, Stdout: "hi\n"},
	}

	var wg sync.WaitGroup
	for i := 0; i < limit; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			run(httptest.NewRecorder(), newRunRequest())
		}()
		<-started // wait until this goroutine is blocking inside Run (holding its semaphore slot)
	}

	// 3rd request should queue because both slots are occupied.
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		w := httptest.NewRecorder()
		run(w, newRunRequest())
		done <- w
	}()

	select {
	case <-done:
		t.Fatal("3rd request completed immediately; expected it to be queuing")
	case <-time.After(50 * time.Millisecond):
		// good: still waiting
	}

	close(hold) // release all blocked runners

	select {
	case w := <-done:
		if w.Code != http.StatusOK {
			t.Errorf("queued request: expected 200, got %d", w.Code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("queued request did not complete after slots freed")
	}

	wg.Wait()
}

// TestSemaphoreContextCancellation verifies a queued request returns without processing on cancellation.
func TestSemaphoreContextCancellation(t *testing.T) {
	origRunner := defaultRunner
	origSem := sem
	defer func() { defaultRunner = origRunner; sem = origSem }()

	sem = make(chan struct{}, 1)
	sem <- struct{}{} // pre-fill: occupy the only slot so the next request must queue

	spy := &spyRunner{result: runner.RunResult{ExitCode: 0, Stdout: "hi\n"}}
	defaultRunner = spy

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled: handler should bail out at the semaphore select

	w := httptest.NewRecorder()
	run(w, newRunRequest().WithContext(ctx))

	if spy.calls != 0 {
		t.Errorf("runner should not be called when context cancelled while queued, got %d calls", spy.calls)
	}
}
