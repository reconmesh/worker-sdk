package dns

import (
	"net/http"
	"os"
	"time"
)

// Options configures New(). Every field is optional; defaults are
// chosen so a typical worker calls New(Options{}) and gets a
// resolver that goes through the system /etc/resolv.conf — which in
// production points at dns-service:53 via the docker-compose `dns:`
// directive. The shared cache works transparently; no code change
// needed in the worker.
//
// HTTPBackend mode (ServiceURL non-empty) stays around for two
// niche cases:
//   - dev laptops outside the docker network where /etc/resolv.conf
//     can't reach dns-service:53
//   - debug paths where seeing the JSON response is useful
type Options struct {
	// ServiceURL, when non-empty, switches to the HTTPBackend.
	// Empty value (default) uses the stdlib net.Resolver which
	// resolves via the system /etc/resolv.conf.
	ServiceURL string
	// HTTPClient overrides the default. Used by callers that want
	// keep-alive tuning, custom timeouts, or instrumentation.
	HTTPClient *http.Client
	// CallerTool is sent to dns-service as X-Caller-Tool. Defaults
	// to the WORKER_TOOL env var if set.
	CallerTool string
	// CacheTTL caps how long the in-process LRU keeps an entry. Set
	// to 0 to disable caching (rare; useful only for tests). Default
	// 5 min.
	CacheTTL time.Duration
	// CacheCapacity bounds the LRU. Default 4096 entries.
	CacheCapacity int
	// LocalResolverTimeout is the per-lookup timeout when falling
	// back to the system resolver. Default 5s.
	LocalResolverTimeout time.Duration
}

func newFromEnv() Resolver {
	return newWithOptions(Options{
		ServiceURL: os.Getenv("DNS_SERVICE_URL"),
		CallerTool: os.Getenv("WORKER_TOOL"),
	})
}

// newWithOptions assembles the resolver chain:
//
//   cachingResolver  ←  HTTPBackend(fallback=LocalBackend)   when ServiceURL set
//   cachingResolver  ←  LocalBackend                         otherwise
//
// One LRU at the top covers both paths; the local fallback shares it
// transparently.
func newWithOptions(opts Options) Resolver {
	local := &LocalBackend{Timeout: opts.LocalResolverTimeout}
	var inner Resolver = local
	if opts.ServiceURL != "" {
		inner = &HTTPBackend{
			BaseURL:    opts.ServiceURL,
			HTTPClient: opts.HTTPClient,
			CallerTool: opts.CallerTool,
			Fallback:   local,
		}
	}
	return newCachingResolver(inner, opts.CacheTTL, opts.CacheCapacity)
}
