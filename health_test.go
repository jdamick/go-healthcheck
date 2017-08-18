package health

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jdamick/go-healthcheck/checks"
)

// TestReturns200IfThereAreNoChecks ensures that the result code of the health
// endpoint is 200 if there are not currently registered checks.
func TestReturns200IfThereAreNoChecks(t *testing.T) {
	recorder := httptest.NewRecorder()

	req, err := http.NewRequest("GET", "https://fakeurl.com/debug/health", nil)
	if err != nil {
		t.Errorf("Failed to create request.")
	}

	StatusHandler(recorder, req)

	if recorder.Code != 200 {
		t.Errorf("Did not get a 200.")
	}
}

// TestReturns500IfThereAreErrorChecks ensures that the result code of the
// health endpoint is 500 if there are health checks with errors
func TestReturns503IfThereAreErrorChecks(t *testing.T) {
	recorder := httptest.NewRecorder()

	req, err := http.NewRequest("GET", "https://fakeurl.com/debug/health", nil)
	if err != nil {
		t.Errorf("Failed to create request.")
	}

	// Create a manual error
	Register("some_check", CheckFunc(func() error {
		return errors.New("This Check did not succeed")
	}))
	defer Unregister("some_check")

	StatusHandler(recorder, req)

	if recorder.Code != 503 {
		t.Errorf("Did not get a 503.")
	}
}

func TestUnregisterStops(t *testing.T) {
	var wg sync.WaitGroup
	wg.Add(1)

	var count int

	reg := NewRegistry()
	chkr := CheckFunc(func() error {
		count++
		wg.Done()
		return nil
	})

	reg.Register("something", NewPeriodicChecker(chkr, 100*time.Millisecond, NewStatusUpdater()))
	wg.Wait()
	reg.Unregister("something")
	time.Sleep(1 * time.Second)
	if count > 1 {
		t.Errorf("should stop checking")
	}
}

func TestPanicIfAlreadyRegistered(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Errorf("Should have Panicked")
		}
	}()
	reg := NewRegistry()

	chk := CheckFunc(func() error { return nil })
	reg.MustRegister("testing", chk)
	reg.MustRegister("testing", chk)
}

func TestRegisterFunc(t *testing.T) {
	// check
	const name = "default check"
	RegisterFunc(name, func() error { return fmt.Errorf("asldkfj") })
	defer Unregister(name)
	if !checkExists(CheckStatus(), name) {
		t.Errorf("should have registered: %v", name)
	}
	if !checkExists(CheckErrorStatus(), name) {
		t.Errorf("should have registered an error: %v", name)
	}
	Unregister(name)
	if checkExists(CheckStatus(), name) {
		t.Errorf("should have unregistered: %v", name)
	}

}
func TestRegisterPeriodicFunc(t *testing.T) {
	const name = "default check2"
	RegisterPeriodicFunc(name, 1*time.Second, func() error { return nil })
	if !checkExists(CheckStatus(), name) {
		t.Errorf("should have registered: %v", name)
	}
	Unregister(name)
}

func TestRegisterPeriodicThresholdFunc(t *testing.T) {
	const name = "default check3"
	RegisterPeriodicThresholdFunc(name, 1*time.Second, 1, func() error { return nil })
	if !checkExists(CheckStatus(), name) {
		t.Errorf("should have registered: %v", name)
	}
	Unregister(name)
}

func checkExists(s HealthCheckStatuses, name string) bool {
	if _, found := s[name]; found {
		return true
	}
	return false
}

type StatusChangeTestor struct {
	UpCount   uint32
	DownCount uint32
	IsUp      bool
}

func (s *StatusChangeTestor) Up(ctx context.Context) {
	atomic.AddUint32(&s.UpCount, 1)
	s.IsUp = true
}

func (s *StatusChangeTestor) Down(ctx context.Context) {
	atomic.AddUint32(&s.DownCount, 1)
	s.IsUp = false
}

func TestStatusEmitter(t *testing.T) {
	var wg sync.WaitGroup
	wg.Add(4) // after 2 failures, so:
	// 1st passes
	// 2nd fails (no callback)
	// 3nd fails (callback)
	// But, we add 1 extra to make sure the callback has time to update counters before we check the counter.

	var reqCount uint32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddUint32(&reqCount, 1) > 1 {
			w.WriteHeader(http.StatusInternalServerError)
		} else {
			w.WriteHeader(http.StatusOK)
		}
		wg.Done()
	}))
	defer server.Close()

	chkr := checks.HTTPChecker(server.URL, http.StatusOK, 2*time.Second, http.Header{})
	ctx := context.Background()
	e := &StatusChangeTestor{}
	updater := NewStatusEmittingUpdater(NewThresholdStatusUpdater(2), e, ctx)

	reg := NewRegistry()
	reg.Register("something", NewPeriodicChecker(chkr, 100*time.Millisecond, updater))
	defer reg.Unregister("something")
	wg.Wait()

	if e.IsUp {
		t.Errorf("Should be down. %#v", e)
	}
	if e.UpCount != 1 || e.DownCount != 1 {
		t.Errorf("Should recieve 1 up (initial check) & 1 down (failure after threshold): %#v", e)
	}
}

// TestHealthHandler ensures that our handler implementation correct protects
// the web application when things aren't so healthy.
func TestHealthHandler(t *testing.T) {
	// clear out existing checks.
	DefaultRegistry = NewRegistry()

	// protect an http server
	handler := http.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	// wrap it in our health handler
	handler = Handler(handler)

	// use this swap check status
	updater := NewStatusUpdater()
	Register("test_check", updater)

	// now, create a test server
	server := httptest.NewServer(handler)

	checkUp := func(t *testing.T, message string) {
		resp, err := http.Get(server.URL)
		if err != nil {
			t.Fatalf("error getting success status: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("unexpected response code from server when %s: %d != %d", message, resp.StatusCode, http.StatusNoContent)
		}
		// NOTE(stevvooe): we really don't care about the body -- the format is
		// not standardized or supported, yet.
	}

	checkDown := func(t *testing.T, message string) {
		resp, err := http.Get(server.URL)
		if err != nil {
			t.Fatalf("error getting down status: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("unexpected response code from server when %s: %d != %d", message, resp.StatusCode, http.StatusServiceUnavailable)
		}
	}

	// server should be up
	checkUp(t, "initial health check")

	// now, we fail the health check
	updater.Update(fmt.Errorf("the server is now out of commission"))
	checkDown(t, "server should be down") // should be down

	// bring server back up
	updater.Update(nil)
	checkUp(t, "when server is back up") // now we should be back up.
}
