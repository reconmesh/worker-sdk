package wtest

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestMockUpstream_GETHandler(t *testing.T) {
	srv := MockUpstream(t).
		OnGET("/api/v1/ping", RespondJSON(200, map[string]any{"ok": true})).
		Build()

	resp, err := http.Get(srv.URL() + "/api/v1/ping")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("status = %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["ok"] != true {
		t.Errorf("got = %v", got)
	}
}

func TestMockUpstream_RespondStatus(t *testing.T) {
	srv := MockUpstream(t).
		OnGET("/quota", RespondStatus(429)).
		Build()

	resp, err := http.Get(srv.URL() + "/quota")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 429 {
		t.Errorf("status = %d", resp.StatusCode)
	}
}

func TestMockUpstream_RespondString(t *testing.T) {
	srv := MockUpstream(t).
		OnGET("/page", RespondString(200, "<html>welcome</html>")).
		Build()
	resp, err := http.Get(srv.URL() + "/page")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "welcome") {
		t.Errorf("body = %q", body)
	}
}

func TestMockUpstream_RespondError(t *testing.T) {
	srv := MockUpstream(t).
		OnGET("/boom", RespondError("kaboom")).
		Build()
	resp, err := http.Get(srv.URL() + "/boom")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 500 {
		t.Errorf("status = %d", resp.StatusCode)
	}
}

func TestMockUpstream_RespondHeader(t *testing.T) {
	srv := MockUpstream(t).
		OnGET("/redir", RespondHeader(302, "Location", "/other")).
		Build()
	// We don't want the http client to follow redirects - inspect raw.
	cli := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := cli.Get(srv.URL() + "/redir")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 302 {
		t.Errorf("status = %d", resp.StatusCode)
	}
	if resp.Header.Get("Location") != "/other" {
		t.Errorf("loc = %q", resp.Header.Get("Location"))
	}
}

func TestMockUpstream_NoRouteIs404(t *testing.T) {
	srv := MockUpstream(t).Build()
	resp, err := http.Get(srv.URL() + "/nonexistent")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Errorf("status = %d", resp.StatusCode)
	}
}

func TestMockUpstream_OnAnyDefault(t *testing.T) {
	srv := MockUpstream(t).
		OnAny(RespondStatus(418)).
		Build()
	resp, err := http.Get(srv.URL() + "/whatever")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 418 {
		t.Errorf("status = %d", resp.StatusCode)
	}
}

func TestMockUpstream_TracksCalls(t *testing.T) {
	srv := MockUpstream(t).
		OnGET("/x", RespondStatus(200)).
		Build()
	for i := 0; i < 3; i++ {
		resp, _ := http.Get(srv.URL() + "/x?i=" + string(rune('0'+i)))
		resp.Body.Close()
	}
	if got := srv.CallCount(); got != 3 {
		t.Errorf("CallCount = %d, want 3", got)
	}
	calls := srv.Calls()
	if len(calls) != 3 {
		t.Errorf("Calls len = %d", len(calls))
	}
	if calls[0].Method != "GET" || calls[0].Path != "/x" {
		t.Errorf("first call = %+v", calls[0])
	}
}

func TestMockUpstream_RespondSequence(t *testing.T) {
	srv := MockUpstream(t).
		OnGET("/poll", RespondSequence(
			RespondStatus(429),
			RespondStatus(429),
			RespondStatus(200),
		)).
		Build()
	codes := []int{}
	for i := 0; i < 4; i++ {
		resp, _ := http.Get(srv.URL() + "/poll")
		codes = append(codes, resp.StatusCode)
		resp.Body.Close()
	}
	// Sequence: 429, 429, 200, then wraps to 429.
	want := []int{429, 429, 200, 429}
	for i, c := range want {
		if codes[i] != c {
			t.Errorf("call %d: got %d, want %d", i, codes[i], c)
		}
	}
}

func TestMockUpstream_PostBodyCaptured(t *testing.T) {
	srv := MockUpstream(t).
		OnPOST("/login", RespondStatus(200)).
		Build()
	resp, err := http.Post(srv.URL()+"/login", "application/x-www-form-urlencoded",
		strings.NewReader("username=admin&password=admin"))
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	defer resp.Body.Close()
	calls := srv.Calls()
	if len(calls) != 1 {
		t.Fatalf("calls = %d", len(calls))
	}
	if !strings.Contains(calls[0].Body, "username=admin") {
		t.Errorf("body not captured: %q", calls[0].Body)
	}
}

func TestMockUpstream_Reset(t *testing.T) {
	srv := MockUpstream(t).
		OnGET("/", RespondStatus(200)).
		Build()
	resp, _ := http.Get(srv.URL() + "/")
	resp.Body.Close()
	if srv.CallCount() != 1 {
		t.Errorf("pre-reset count = %d", srv.CallCount())
	}
	srv.Reset()
	if srv.CallCount() != 0 {
		t.Errorf("post-reset count = %d", srv.CallCount())
	}
}
