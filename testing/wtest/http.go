package wtest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// Handler is the alias the mock upstream uses internally. It mirrors
// http.HandlerFunc but allows factories ([RespondJSON], etc.) to be
// passed without an explicit conversion.
type Handler = http.HandlerFunc

// MockUpstreamServer is a mux-backed httptest.Server. Tests register
// handlers per (method, path), call .URL() on it for the base URL,
// and the cleanup is auto-wired via t.Cleanup.
type MockUpstreamServer struct {
	t       testing.TB
	srv     *httptest.Server
	mu      sync.Mutex
	routes  map[string]Handler // key = METHOD + " " + PATH
	default_ Handler
	calls   []Call
}

// Call records one request received by the mock server. Tests inspect
// .Calls() to assert what the worker hit and how many times.
type Call struct {
	Method  string
	Path    string
	Query   string
	Headers http.Header
	Body    string
}

// MockUpstream returns a builder for the mock server. Add handlers
// with .OnGET / .OnPOST / .OnAny, then .Build() to start it.
func MockUpstream(t testing.TB) *MockUpstreamServer {
	t.Helper()
	m := &MockUpstreamServer{
		t:      t,
		routes: map[string]Handler{},
	}
	return m
}

// OnGET registers a GET handler for path. Returns the server so calls
// can be chained.
func (m *MockUpstreamServer) OnGET(path string, h Handler) *MockUpstreamServer {
	return m.On("GET", path, h)
}

// OnPOST registers a POST handler for path.
func (m *MockUpstreamServer) OnPOST(path string, h Handler) *MockUpstreamServer {
	return m.On("POST", path, h)
}

// On registers a handler for an arbitrary method.
func (m *MockUpstreamServer) On(method, path string, h Handler) *MockUpstreamServer {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.routes[method+" "+path] = h
	return m
}

// OnAny registers a default handler that fires when no exact route
// matches. Without one, unmatched paths return 404.
func (m *MockUpstreamServer) OnAny(h Handler) *MockUpstreamServer {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.default_ = h
	return m
}

// Build starts the server and returns it. Closer is registered with
// t.Cleanup so individual tests don't have to defer.
func (m *MockUpstreamServer) Build() *MockUpstreamServer {
	m.t.Helper()
	if m.srv != nil {
		return m
	}
	m.srv = httptest.NewServer(http.HandlerFunc(m.dispatch))
	m.t.Cleanup(m.srv.Close)
	return m
}

// URL returns the base URL of the started server. Calls Build()
// implicitly so tests can do MockUpstream(t).OnGET(...).URL().
func (m *MockUpstreamServer) URL() string {
	m.Build()
	return m.srv.URL
}

// Close shuts the server down. Optional; t.Cleanup already calls it.
func (m *MockUpstreamServer) Close() {
	if m.srv != nil {
		m.srv.Close()
	}
}

// Calls returns a snapshot of every request the mock has received.
// Safe to call from tests at any time; the underlying slice is copied.
func (m *MockUpstreamServer) Calls() []Call {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Call, len(m.calls))
	copy(out, m.calls)
	return out
}

// CallCount returns the number of requests the mock has received.
func (m *MockUpstreamServer) CallCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.calls)
}

// Reset clears the recorded call log. Useful when a single test runs
// multiple sub-scenarios against the same server.
func (m *MockUpstreamServer) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = nil
}

func (m *MockUpstreamServer) dispatch(w http.ResponseWriter, r *http.Request) {
	body := readBody(r)
	m.mu.Lock()
	m.calls = append(m.calls, Call{
		Method:  r.Method,
		Path:    r.URL.Path,
		Query:   r.URL.RawQuery,
		Headers: r.Header.Clone(),
		Body:    body,
	})
	h, ok := m.routes[r.Method+" "+r.URL.Path]
	if !ok {
		// fall through to default if registered
		h = m.default_
	}
	m.mu.Unlock()
	if h == nil {
		http.NotFound(w, r)
		return
	}
	h(w, r)
}

func readBody(r *http.Request) string {
	if r.Body == nil {
		return ""
	}
	const max = 1 << 20 // 1 MiB
	buf := make([]byte, max)
	n, _ := r.Body.Read(buf)
	_ = r.Body.Close()
	return string(buf[:n])
}

// RespondJSON returns a handler that writes the given object as JSON
// with the given status. Use for typical API stubs.
func RespondJSON(status int, body any) Handler {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}
}

// RespondStatus returns a handler that just writes status + empty body.
// Most useful for 429 / 401 / 503 paths where the body doesn't matter.
func RespondStatus(status int) Handler {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
	}
}

// RespondString returns a handler that writes status + body as plain
// text. Use for HTML / text fixtures (e.g. login pages).
func RespondString(status int, body string) Handler {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = strings.NewReader(body).WriteTo(w)
	}
}

// RespondError returns a handler that writes a 500 with the given
// message in the body. Shorthand for the common "force a server error"
// path.
func RespondError(msg string) Handler {
	return func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, msg, http.StatusInternalServerError)
	}
}

// RespondHeader returns a handler that sets the given header (key,
// value) and writes status with no body. Useful for asserting redirect
// follow, content-type detection, etc.
func RespondHeader(status int, key, value string) Handler {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(key, value)
		w.WriteHeader(status)
	}
}

// RespondSequence cycles through the given handlers, one per request.
// After the last handler the sequence repeats. Use to model "first
// call rate-limited, second call succeeds" without writing a stateful
// closure each time.
func RespondSequence(handlers ...Handler) Handler {
	if len(handlers) == 0 {
		return RespondStatus(204)
	}
	var (
		mu sync.Mutex
		i  int
	)
	return func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		h := handlers[i%len(handlers)]
		i++
		mu.Unlock()
		h(w, r)
	}
}
