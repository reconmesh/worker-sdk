package wtest

import (
	"context"
	"testing"
	"time"
)

func TestNewRuntime_HasDefaults(t *testing.T) {
	rt := NewRuntime(t)
	if rt.HTTP == nil || rt.DNS == nil || rt.Cache == nil || rt.Ctx == nil {
		t.Fatalf("NewRuntime returned nil field: %+v", rt)
	}
	if _, ok := rt.Ctx.Deadline(); !ok {
		t.Errorf("ctx should have a deadline")
	}
}

func TestRuntime_HTTPURLReady(t *testing.T) {
	rt := NewRuntime(t)
	if rt.HTTP.URL() == "" {
		t.Errorf("HTTP.URL should be non-empty")
	}
}

func TestRuntime_CancelStopsCtx(t *testing.T) {
	rt := NewRuntime(t)
	rt.Cancel()
	select {
	case <-rt.Ctx.Done():
		// expected
	case <-time.After(time.Second):
		t.Errorf("ctx did not close after Cancel")
	}
}

func TestFakeDNS_AAndAAAA(t *testing.T) {
	d := NewFakeDNS()
	d.SetA("acme.com", "1.2.3.4", "5.6.7.8")
	d.SetAAAA("acme.com", "::1")

	a := d.LookupA("acme.com")
	if len(a) != 2 || a[0] != "1.2.3.4" {
		t.Errorf("A = %v", a)
	}
	aaaa := d.LookupAAAA("acme.com")
	if len(aaaa) != 1 || aaaa[0] != "::1" {
		t.Errorf("AAAA = %v", aaaa)
	}
	if d.LookupA("nope.example") != nil {
		t.Errorf("unset host should return nil")
	}
}

func TestFakeDNS_CNAMEAndCallCount(t *testing.T) {
	d := NewFakeDNS()
	d.SetCNAME("www.acme.com", "acme.com")
	if d.LookupCNAME("www.acme.com") != "acme.com" {
		t.Errorf("CNAME mismatch")
	}
	d.LookupA("www.acme.com")
	d.LookupAAAA("www.acme.com")
	if d.CallCount("www.acme.com") != 3 {
		t.Errorf("CallCount = %d", d.CallCount("www.acme.com"))
	}
}

func TestFakeCache_LookupUpsert(t *testing.T) {
	c := NewFakeCache()
	ctx := context.Background()

	if _, ok := c.Lookup(ctx, "k"); ok {
		t.Errorf("empty cache should miss")
	}
	c.Upsert(ctx, "k", []byte("v1"))
	if v, ok := c.Lookup(ctx, "k"); !ok || string(v) != "v1" {
		t.Errorf("lookup got (%q, %v)", v, ok)
	}
	hits, miss, size := c.Stats()
	if hits != 1 || miss != 1 || size != 1 {
		t.Errorf("stats = (%d, %d, %d)", hits, miss, size)
	}
}

func TestFakeCache_DefensiveCopy(t *testing.T) {
	// Mutating the input slice after Upsert must not affect the
	// stored value · catches the "alias the caller's buffer" bug.
	c := NewFakeCache()
	src := []byte("original")
	c.Upsert(context.Background(), "k", src)
	src[0] = 'X'
	v, _ := c.Lookup(context.Background(), "k")
	if string(v) != "original" {
		t.Errorf("Upsert should defensively copy, got %q", v)
	}
}
