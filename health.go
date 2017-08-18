package health

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"sync"
	"time"
)

type HealthCheckStatuses map[string]string

// Registers global /debug/health api endpoint, creates default registry
func RegisterEndpoint() {
	http.HandleFunc("/debug/health", StatusHandler)
}

// A Registry is a collection of checks. Most applications will use the global
// registry defined in DefaultRegistry. However, unit tests may need to create
// separate registries to isolate themselves from other tests.
type Registry struct {
	Logger           *log.Logger
	mu               sync.RWMutex
	registeredChecks map[string]Checker
}

// NewRegistry creates a new registry. This isn't necessary for normal use of
// the package, but may be useful for unit tests so individual tests have their
// own set of checks.
func NewRegistry() *Registry {
	return &Registry{
		Logger:           log.New(os.Stderr, "", log.LstdFlags),
		registeredChecks: make(map[string]Checker),
	}
}

// DefaultRegistry is the default registry where checks are registered. It is
// the registry used by the HTTP handler.
var DefaultRegistry *Registry

// StatusChange interface for changes to the status of a checker
type StatusChange interface {
	Up(context.Context)
	Down(context.Context)
}

// Checker is the interface for a Health Checker
type Checker interface {
	// Check returns nil if the service is okay.
	Check() error
}

// CheckFunc is a convenience type to create functions that implement
// the Checker interface
type CheckFunc func() error

// Check Implements the Checker interface to allow for any func() error method
// to be passed as a Checker
func (cf CheckFunc) Check() error {
	return cf()
}

// Updater implements a health check that is explicitly set.
type Updater interface {
	Checker

	// Update updates the current status of the health check.
	Update(status error)
}

// updater implements Checker and Updater, providing an asynchronous Update
// method.
// This allows us to have a Checker that returns the Check() call immediately
// not blocking on a potentially expensive check.
type updater struct {
	mu     sync.Mutex
	status error
}

// Check implements the Checker interface
func (u *updater) Check() error {
	u.mu.Lock()
	defer u.mu.Unlock()

	return u.status
}

// Update implements the Updater interface, allowing asynchronous access to
// the status of a Checker.
func (u *updater) Update(status error) {
	u.mu.Lock()
	defer u.mu.Unlock()

	u.status = status
}

// NewStatusUpdater returns a new updater
func NewStatusUpdater() Updater {
	return &updater{}
}

// thresholdUpdater implements Checker and Updater, providing an asynchronous Update
// method.
// This allows us to have a Checker that returns the Check() call immediately
// not blocking on a potentially expensive check.
type thresholdUpdater struct {
	mu        sync.Mutex
	status    error
	threshold int
	count     int
}

// Check implements the Checker interface
func (tu *thresholdUpdater) Check() error {
	tu.mu.Lock()
	defer tu.mu.Unlock()

	if tu.count >= tu.threshold {
		return tu.status
	}

	return nil
}

// thresholdUpdater implements the Updater interface, allowing asynchronous
// access to the status of a Checker.
func (tu *thresholdUpdater) Update(status error) {
	tu.mu.Lock()
	defer tu.mu.Unlock()

	if status == nil {
		tu.count = 0
	} else if tu.count < tu.threshold {
		tu.count++
	}

	tu.status = status
}

// NewThresholdStatusUpdater returns a new thresholdUpdater
func NewThresholdStatusUpdater(t int) Updater {
	return &thresholdUpdater{threshold: t}
}

type statusEmittingUpdater struct {
	Updater
	emitter StatusChange
	cbCtx   context.Context
	sent    bool
}

func (se *statusEmittingUpdater) Update(status error) {
	prev := se.Check()
	se.Updater.Update(status)
	cur := se.Check()
	if prev != cur || !se.sent {
		if cur == nil {
			se.emitter.Up(se.cbCtx)
		} else {
			se.emitter.Down(se.cbCtx)
		}
		se.sent = true
	}
}

// NewStatusEmittingUpdater wraps an Updater with a status emitting updater
func NewStatusEmittingUpdater(updater Updater, emitter StatusChange, ctx context.Context) Updater {
	return &statusEmittingUpdater{emitter: emitter, Updater: updater, cbCtx: ctx}
}

// PeriodChecker is a Checker that is run on a periodic basis
type periodicChecker struct {
	Checker
	ticker *time.Ticker
	done   chan struct{}
}

// NewPeriodChecker creates a new checker that periodically runs the provided Checker
// on the given period and then asynchronously updates the given Updater.
func NewPeriodicChecker(check Checker, period time.Duration, updater Updater) *periodicChecker {
	chkr := &periodicChecker{ticker: time.NewTicker(period), done: make(chan struct{}), Checker: check}
	go func() {
		for {
			select {
			case <-chkr.ticker.C:
				updater.Update(check.Check())
			case <-chkr.done:
				return
			}
		}
	}()
	return chkr
}

func (p *periodicChecker) Stop() {
	p.ticker.Stop()
	close(p.done)
}

// PeriodicChecker wraps an updater to provide a periodic checker
func PeriodicChecker(check Checker, period time.Duration) Checker {
	return NewPeriodicChecker(check, period, NewStatusUpdater())
}

// PeriodicThresholdChecker wraps an updater to provide a periodic checker that
// uses a threshold before it changes status
func PeriodicThresholdChecker(check Checker, period time.Duration, threshold int) Checker {
	return NewPeriodicChecker(check, period, NewThresholdStatusUpdater(threshold))
}

// CheckStatus returns a map with all the current health check results
func (registry *Registry) CheckStatus() HealthCheckStatuses {
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	statusKeys := make(map[string]string)
	for k, v := range registry.registeredChecks {
		err := v.Check()
		if err != nil {
			statusKeys[k] = err.Error()
		} else {
			statusKeys[k] = "ok"
		}
	}

	return statusKeys
}

// CheckStatus returns a map with all the current health checks from the
// default registry.
func CheckStatus() HealthCheckStatuses {
	return DefaultRegistry.CheckStatus()
}

// CheckErrorStatus returns a map with all the current health check errors
func (registry *Registry) CheckErrorStatus() HealthCheckStatuses {
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	statusKeys := make(HealthCheckStatuses)
	for k, v := range registry.registeredChecks {
		err := v.Check()
		if err != nil {
			statusKeys[k] = err.Error()
		}
	}

	return statusKeys
}

// CheckErrorStatus returns a map with all the current health check errors from the
// default registry.
func CheckErrorStatus() HealthCheckStatuses {
	return DefaultRegistry.CheckErrorStatus()
}

// Register associates the checker with the provided name.
// It will panic if the name is already registered with a Checker.
func (registry *Registry) MustRegister(name string, check Checker) {
	if registry == nil {
		registry = DefaultRegistry
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	_, ok := registry.registeredChecks[name]
	if ok {
		panic("Check already exists: " + name)
	}
	registry.registeredChecks[name] = check
}

// Register associates the checker with the provided name.
func (registry *Registry) Register(name string, check Checker) {
	if registry == nil {
		registry = DefaultRegistry
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	registry.registeredChecks[name] = check
}

// Register associates the checker with the provided name in the default
// registry.
func Register(name string, check Checker) {
	DefaultRegistry.Register(name, check)
}

// Unregister a checker for the provided name
func (registry *Registry) Unregister(name string) {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if chk, found := registry.registeredChecks[name]; found {
		if prdchk, ok := chk.(*periodicChecker); ok {
			prdchk.Stop()
		}
	}
	delete(registry.registeredChecks, name)
}

// Unregister a checker for the provided name in the default registry.
func Unregister(name string) {
	DefaultRegistry.Unregister(name)
}

// RegisterFunc allows the convenience of registering a checker directly from
// an arbitrary func() error.
func (registry *Registry) RegisterFunc(name string, check func() error) {
	registry.Register(name, CheckFunc(check))
}

// RegisterFunc allows the convenience of registering a checker in the default
// registry directly from an arbitrary func() error.
func RegisterFunc(name string, check func() error) {
	DefaultRegistry.RegisterFunc(name, check)
}

// RegisterPeriodicFunc allows the convenience of registering a PeriodicChecker
// from an arbitrary func() error.
func (registry *Registry) RegisterPeriodicFunc(name string, period time.Duration, check CheckFunc) {
	registry.Register(name, PeriodicChecker(CheckFunc(check), period))
}

// RegisterPeriodicFunc allows the convenience of registering a PeriodicChecker
// in the default registry from an arbitrary func() error.
func RegisterPeriodicFunc(name string, period time.Duration, check CheckFunc) {
	DefaultRegistry.RegisterPeriodicFunc(name, period, check)
}

// RegisterPeriodicThresholdFunc allows the convenience of registering a
// PeriodicChecker from an arbitrary func() error.
func (registry *Registry) RegisterPeriodicThresholdFunc(name string, period time.Duration, threshold int, check CheckFunc) {
	registry.Register(name, PeriodicThresholdChecker(CheckFunc(check), period, threshold))
}

// RegisterPeriodicThresholdFunc allows the convenience of registering a
// PeriodicChecker in the default registry from an arbitrary func() error.
func RegisterPeriodicThresholdFunc(name string, period time.Duration, threshold int, check CheckFunc) {
	DefaultRegistry.RegisterPeriodicThresholdFunc(name, period, threshold, check)
}

// StatusHandler returns a JSON blob with all the currently registered Health Checks in the default registry
// and their corresponding status.
// Returns 503 if any Error status exists, 200 otherwise
func StatusHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		checks := CheckErrorStatus()
		status := http.StatusOK

		// If there is an error, return 503
		if len(checks) != 0 {
			status = http.StatusServiceUnavailable
		}

		statusResponse(w, r, status, checks)
	} else {
		http.NotFound(w, r)
	}
}

// Handler returns a handler that will return 503 response code if the health
// checks have failed. If everything is okay with the health checks, the
// handler will pass through to the provided handler. Use this handler to
// disable a web application when the health checks fail.
func Handler(handler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checks := CheckErrorStatus()
		if len(checks) != 0 {
			w.WriteHeader(http.StatusServiceUnavailable)
			e, err := json.Marshal(struct {
				Message string      `json:"message"`
				Detail  interface{} `json:"detail,omitempty"`
			}{
				Message: "service unavailable",
				Detail:  "health check failed: please see /debug/health",
			})

			if err == nil {
				w.Header().Set("Content-Type", "application/json; charset=utf-8")
				w.Header().Set("Content-Length", fmt.Sprint(len(e)))
				if _, err = w.Write(e); err != nil {
					DefaultRegistry.Logger.Printf("error writing health status response body: %v", err)
				}
			}

			return
		}

		handler.ServeHTTP(w, r) // pass through
	})
}

// statusResponse completes the request with a response describing the health
// of the service.
func statusResponse(w http.ResponseWriter, r *http.Request, status int, checks map[string]string) {
	p, err := json.Marshal(checks)
	if err != nil {
		DefaultRegistry.Logger.Printf("error serializing health status: %v", err)
		p, err = json.Marshal(struct {
			ServerError string `json:"server_error"`
		}{
			ServerError: "Could not parse error message",
		})
		status = http.StatusInternalServerError

		if err != nil {
			DefaultRegistry.Logger.Printf("error serializing health status failure message: %v", err)
			return
		}
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Length", fmt.Sprint(len(p)))
	w.WriteHeader(status)
	if _, err := w.Write(p); err != nil {
		DefaultRegistry.Logger.Printf("error writing health status response body: %v", err)
	}
}

func init() {
	DefaultRegistry = NewRegistry()
}
