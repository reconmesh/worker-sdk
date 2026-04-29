package dns

import (
	"context"
	"errors"
	"net"
	"sort"
	"strings"
	"time"
)

// LocalBackend resolves via the Go stdlib net.Resolver. Used when:
//
//   - DNS_SERVICE_URL is unset (dev / single-binary deployments)
//   - the HTTP backend can't reach the dns-service (graceful fallback)
//   - unit tests want a real resolver without mocking gRPC
//
// It does NOT cache by itself - callers wrap it with cachingResolver
// if they want LRU. The default constructor (Default(), New()) does
// that wrapping for you.
type LocalBackend struct {
	// Resolver is the underlying *net.Resolver. nil → net.DefaultResolver.
	Resolver *net.Resolver
	// Timeout per individual lookup. 0 → 5s.
	Timeout time.Duration
}

func (b *LocalBackend) resolver() *net.Resolver {
	if b.Resolver != nil {
		return b.Resolver
	}
	return net.DefaultResolver
}

func (b *LocalBackend) timeout() time.Duration {
	if b.Timeout > 0 {
		return b.Timeout
	}
	return 5 * time.Second
}

func (b *LocalBackend) lookupCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, b.timeout())
}

// Resolve issues an A + AAAA query in parallel via the stdlib resolver.
// Stdlib does this internally on LookupIP - we route through that and
// preserve the error → typed error mapping the rest of the package
// expects.
func (b *LocalBackend) Resolve(ctx context.Context, host string) ([]net.IP, error) {
	ctx, cancel := b.lookupCtx(ctx)
	defer cancel()
	ips, err := b.resolver().LookupIP(ctx, "ip", host)
	if err != nil {
		return nil, mapErr(err)
	}
	return ips, nil
}

// ResolveAll fans out the relevant per-type lookups in parallel. Stdlib
// has no single "give me everything" call; we run five goroutines.
// Errors per type are folded into empty fields rather than aborting
// the whole call - partial answers are useful (e.g. an MX-only host).
func (b *LocalBackend) ResolveAll(ctx context.Context, host string) (*Records, error) {
	ctx, cancel := b.lookupCtx(ctx)
	defer cancel()

	now := time.Now().UTC()
	rec := &Records{
		Host:       host,
		QueriedAt:  now,
		// Stdlib doesn't give us TTLs back. We default to 5min so
		// callers behind the LRU don't keep stale answers forever
		// while not being so eager that we spam re-resolution.
		ValidUntil: now.Add(5 * time.Minute),
	}

	type slot struct {
		fn  func()
	}
	tasks := []slot{
		{fn: func() {
			ips, err := b.resolver().LookupIP(ctx, "ip", host)
			if errors.Is(err, &net.DNSError{}) {
				var dnsErr *net.DNSError
				if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
					rec.NXDomain = true
				}
				return
			}
			if err != nil {
				return
			}
			for _, ip := range ips {
				if v4 := ip.To4(); v4 != nil {
					rec.A = append(rec.A, v4.String())
				} else {
					rec.AAAA = append(rec.AAAA, ip.String())
				}
			}
		}},
		{fn: func() {
			c, _ := b.resolver().LookupCNAME(ctx, host)
			c = strings.TrimSuffix(c, ".")
			if c != "" && c != host {
				rec.CNAME = c
			}
		}},
		{fn: func() {
			mx, _ := b.resolver().LookupMX(ctx, host)
			out := make([]MX, 0, len(mx))
			for _, m := range mx {
				out = append(out, MX{
					Priority: int(m.Pref),
					Host:     strings.TrimSuffix(m.Host, "."),
				})
			}
			sort.Slice(out, func(i, j int) bool { return out[i].Priority < out[j].Priority })
			rec.MX = out
		}},
		{fn: func() {
			ns, _ := b.resolver().LookupNS(ctx, host)
			out := make([]string, 0, len(ns))
			for _, n := range ns {
				out = append(out, strings.TrimSuffix(n.Host, "."))
			}
			sort.Strings(out)
			rec.NS = out
		}},
		{fn: func() {
			txt, _ := b.resolver().LookupTXT(ctx, host)
			rec.TXT = txt
		}},
	}

	// Run sequentially first - stdlib lookups are I/O bound but the
	// system resolver is shared, parallel calls don't typically
	// speed things up linearly. If profiling later shows it matters,
	// switch to errgroup; for now we keep things readable.
	for _, t := range tasks {
		t.fn()
	}
	return rec, nil
}

func (b *LocalBackend) LookupCNAME(ctx context.Context, host string) (string, error) {
	ctx, cancel := b.lookupCtx(ctx)
	defer cancel()
	c, err := b.resolver().LookupCNAME(ctx, host)
	if err != nil {
		return "", mapErr(err)
	}
	c = strings.TrimSuffix(c, ".")
	if c == host {
		return "", nil
	}
	return c, nil
}

func (b *LocalBackend) LookupMX(ctx context.Context, host string) ([]MX, error) {
	ctx, cancel := b.lookupCtx(ctx)
	defer cancel()
	mx, err := b.resolver().LookupMX(ctx, host)
	if err != nil {
		return nil, mapErr(err)
	}
	out := make([]MX, 0, len(mx))
	for _, m := range mx {
		out = append(out, MX{
			Priority: int(m.Pref),
			Host:     strings.TrimSuffix(m.Host, "."),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Priority < out[j].Priority })
	return out, nil
}

func (b *LocalBackend) LookupTXT(ctx context.Context, host string) ([]string, error) {
	ctx, cancel := b.lookupCtx(ctx)
	defer cancel()
	out, err := b.resolver().LookupTXT(ctx, host)
	if err != nil {
		return nil, mapErr(err)
	}
	return out, nil
}

func (b *LocalBackend) LookupNS(ctx context.Context, host string) ([]string, error) {
	ctx, cancel := b.lookupCtx(ctx)
	defer cancel()
	ns, err := b.resolver().LookupNS(ctx, host)
	if err != nil {
		return nil, mapErr(err)
	}
	out := make([]string, 0, len(ns))
	for _, n := range ns {
		out = append(out, strings.TrimSuffix(n.Host, "."))
	}
	sort.Strings(out)
	return out, nil
}

// mapErr translates stdlib resolver errors into the package's typed
// errors. Any net.DNSError with IsNotFound becomes ErrNXDomain;
// timeout-flagged ones become ErrTimeout; everything else passes
// through verbatim so callers retain detail.
func mapErr(err error) error {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		if dnsErr.IsNotFound {
			return ErrNXDomain
		}
		if dnsErr.IsTimeout {
			return ErrTimeout
		}
	}
	return err
}
