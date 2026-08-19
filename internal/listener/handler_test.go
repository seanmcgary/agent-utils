package listener

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha1" //nolint:gosec // needed to construct the downgrade attack this test proves is closed, not to protect anything
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/go-github/v77/github"
	"github.com/google/uuid"
)

const testSecret = "test-webhook-secret"

// sha256Sig returns the header value GitHub would send for body signed with
// secret: "sha256=" followed by the lowercase hex HMAC-SHA256 digest.
func sha256Sig(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// sha1Hex returns the bare lowercase hex HMAC-SHA1 digest of body, with no
// "sha1=" prefix. Used to build the downgrade-attack signature by hand.
func sha1Hex(secret string, body []byte) string {
	mac := hmac.New(sha1.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// repoPayload returns the JSON body of a minimal webhook delivery carrying
// the given repository full_name.
func repoPayload(t *testing.T, fullName string) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"repository": map[string]any{"full_name": fullName},
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return body
}

// newServer builds a Server wired to tickCh: the fake Tick sends the repo
// name and returns. Tests synchronise on tickCh, never on a sleep or a
// counter, so they stay correct under -race.
func newServer(t *testing.T, tickCh chan<- string) *Server {
	t.Helper()
	s, err := New(&Server{
		Secret: testSecret,
		Port:   freePort(t),
		Tick: func(_ context.Context, repo string) {
			tickCh <- repo
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

// waitTick blocks for one value on ch, bounded by a deadline so a test that
// would otherwise hang forever fails instead. This is a safety bound, not a
// synchronisation sleep: the send on ch is what proves Tick ran.
func waitTick(t *testing.T, ch <-chan string) string {
	t.Helper()
	select {
	case repo := <-ch:
		return repo
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for Tick")
		return ""
	}
}

// assertNoTick fails if a value is already waiting on ch. It is only valid
// right after a request whose handling is entirely synchronous up to the
// point being tested (no goroutine could still be in flight), which every
// rejection path in Handler satisfies: Tick is dispatched only at step 10,
// after every check this package makes.
func assertNoTick(t *testing.T, ch <-chan string) {
	t.Helper()
	select {
	case repo := <-ch:
		t.Fatalf("Tick called unexpectedly with repo %q", repo)
	default:
	}
}

func doRequest(t *testing.T, url string, body []byte, headers map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(github.EventTypeHeader, "issues")
	req.Header.Set(github.DeliveryIDHeader, uuid.NewString())
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	return resp
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(b)
}

func TestValidSignatureAccepts(t *testing.T) {
	tickCh := make(chan string, 1)
	s := newServer(t, tickCh)
	ts := httptest.NewServer(s.Handler(context.Background()))
	defer ts.Close()

	body := repoPayload(t, "octo/hello")
	resp := doRequest(t, ts.URL+"/webhook", body, map[string]string{
		github.SHA256SignatureHeader: sha256Sig(testSecret, body),
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}
	if repo := waitTick(t, tickCh); repo != "octo/hello" {
		t.Errorf("Tick called with repo %q, want %q", repo, "octo/hello")
	}
}

// TestWrongSignatureRejects is the control for the no-leak requirement on
// the path that actually carries the attacker's signature into go-github's
// error text. ValidatePayloadFromBody's ValidateSignature returns errors
// like "signature is invalid" here, built from the caller's own signature
// argument; TestSHA256HeaderCarryingSHA1SignatureRejects never reaches the
// library at all, so it proves nothing about whether rejected() leaks a
// real library error. This test would fail if rejected ever changed to
// http.Error(w, err.Error(), status).
func TestWrongSignatureRejects(t *testing.T) {
	tickCh := make(chan string, 1)
	s := newServer(t, tickCh)
	ts := httptest.NewServer(s.Handler(context.Background()))
	defer ts.Close()

	body := repoPayload(t, "octo/hello")
	sig := sha256Sig("not-the-secret", body)
	resp := doRequest(t, ts.URL+"/webhook", body, map[string]string{
		github.SHA256SignatureHeader: sig,
	})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	assertNoTick(t, tickCh)

	respBody := readBody(t, resp)
	if strings.Contains(respBody, sig) {
		t.Errorf("rejection body leaks the signature: %q", respBody)
	}
	if strings.Contains(strings.ToLower(respBody), "sha1") {
		t.Errorf("rejection body leaks the algorithm name: %q", respBody)
	}
}

// TestInvalidHexSignatureRejects sends a signature with a correct "sha256="
// prefix but a payload that is not valid hex. go-github's messageMAC
// returns "error decoding signature %q", which interpolates the exact raw
// signature string given to it -- a second, distinct place a leak could
// enter besides ValidateSignature's own error text.
func TestInvalidHexSignatureRejects(t *testing.T) {
	tickCh := make(chan string, 1)
	s := newServer(t, tickCh)
	ts := httptest.NewServer(s.Handler(context.Background()))
	defer ts.Close()

	body := repoPayload(t, "octo/hello")
	sig := "sha256=not-valid-hex"
	resp := doRequest(t, ts.URL+"/webhook", body, map[string]string{
		github.SHA256SignatureHeader: sig,
	})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	assertNoTick(t, tickCh)

	respBody := readBody(t, resp)
	if strings.Contains(respBody, sig) {
		t.Errorf("rejection body leaks the signature: %q", respBody)
	}
	if strings.Contains(strings.ToLower(respBody), "sha1") {
		t.Errorf("rejection body leaks the algorithm name: %q", respBody)
	}
}

func TestMissingSignatureHeaderRejects(t *testing.T) {
	tickCh := make(chan string, 1)
	s := newServer(t, tickCh)
	ts := httptest.NewServer(s.Handler(context.Background()))
	defer ts.Close()

	body := repoPayload(t, "octo/hello")
	resp := doRequest(t, ts.URL+"/webhook", body, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	assertNoTick(t, tickCh)
}

// TestSHA256HeaderCarryingSHA1SignatureRejects is the test that proves the
// downgrade trap is closed. github.messageMAC picks the hash function from
// the signature string's OWN prefix, not from the header name, so a
// signature of "sha1=<hmac-sha1>" sent on the SHA-256 header would otherwise
// be verified with SHA-1 -- an attacker who can forge SHA-1 (or who read a
// leaked secret and just wants any valid encoding) could authenticate this
// way. Handler must reject before the signature ever reaches the library.
// The header-name case alone (TestMissingSignatureHeaderRejects) would pass
// against a still-vulnerable handler; this is the one that would not.
func TestSHA256HeaderCarryingSHA1SignatureRejects(t *testing.T) {
	tickCh := make(chan string, 1)
	s := newServer(t, tickCh)
	ts := httptest.NewServer(s.Handler(context.Background()))
	defer ts.Close()

	body := repoPayload(t, "octo/hello")
	sig := "sha1=" + sha1Hex(testSecret, body)
	resp := doRequest(t, ts.URL+"/webhook", body, map[string]string{
		github.SHA256SignatureHeader: sig,
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	assertNoTick(t, tickCh)

	respBody := readBody(t, resp)
	if strings.Contains(respBody, sig) {
		t.Errorf("rejection body leaks the signature: %q", respBody)
	}
	if strings.Contains(strings.ToLower(respBody), "sha1") {
		t.Errorf("rejection body leaks the algorithm name: %q", respBody)
	}
}

func TestSHA1HeaderAloneRejects(t *testing.T) {
	tickCh := make(chan string, 1)
	s := newServer(t, tickCh)
	ts := httptest.NewServer(s.Handler(context.Background()))
	defer ts.Close()

	body := repoPayload(t, "octo/hello")
	resp := doRequest(t, ts.URL+"/webhook", body, map[string]string{
		github.SHA1SignatureHeader: "sha1=" + sha1Hex(testSecret, body),
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	assertNoTick(t, tickCh)
}

func TestNewRefusesEmptySecret(t *testing.T) {
	_, err := New(&Server{
		Secret: "",
		Tick:   func(context.Context, string) {},
	})
	if err == nil {
		t.Fatal("New with an empty secret must return an error")
	}
}

func TestContentTypeWithCharsetAccepted(t *testing.T) {
	tickCh := make(chan string, 1)
	s := newServer(t, tickCh)
	ts := httptest.NewServer(s.Handler(context.Background()))
	defer ts.Close()

	body := repoPayload(t, "octo/hello")
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/webhook", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set(github.EventTypeHeader, "issues")
	req.Header.Set(github.DeliveryIDHeader, uuid.NewString())
	req.Header.Set(github.SHA256SignatureHeader, sha256Sig(testSecret, body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}
	waitTick(t, tickCh)
}

func TestFormEncodedContentTypeRejected(t *testing.T) {
	tickCh := make(chan string, 1)
	s := newServer(t, tickCh)
	ts := httptest.NewServer(s.Handler(context.Background()))
	defer ts.Close()

	body := repoPayload(t, "octo/hello")
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/webhook", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set(github.EventTypeHeader, "issues")
	req.Header.Set(github.DeliveryIDHeader, uuid.NewString())
	req.Header.Set(github.SHA256SignatureHeader, sha256Sig(testSecret, body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("status = %d, want 415", resp.StatusCode)
	}
	assertNoTick(t, tickCh)
}

func TestOversizedBodyRejectedAsTooLarge(t *testing.T) {
	tickCh := make(chan string, 1)
	s := newServer(t, tickCh)
	ts := httptest.NewServer(s.Handler(context.Background()))
	defer ts.Close()

	// 6 MiB, over the 5 MiB cap. Its content does not matter: the library
	// reads the whole body before it ever checks the signature, so this must
	// fail as a size error and not be conflated with an auth failure.
	big := bytes.Repeat([]byte("a"), 6<<20)
	resp := doRequest(t, ts.URL+"/webhook", big, map[string]string{
		github.SHA256SignatureHeader: "sha256=" + strings.Repeat("0", 64),
	})
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413, not 401", resp.StatusCode)
	}
	assertNoTick(t, tickCh)
}

func TestUnsubscribedEventGivesNoContent(t *testing.T) {
	tickCh := make(chan string, 1)
	s := newServer(t, tickCh)
	ts := httptest.NewServer(s.Handler(context.Background()))
	defer ts.Close()

	body := repoPayload(t, "octo/hello")
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/webhook", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(github.EventTypeHeader, "star") // not in ghub.HookEvents
	req.Header.Set(github.DeliveryIDHeader, uuid.NewString())
	req.Header.Set(github.SHA256SignatureHeader, sha256Sig(testSecret, body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
	assertNoTick(t, tickCh)
}

func TestRepeatedDeliveryIsDroppedOnSecondDelivery(t *testing.T) {
	tickCh := make(chan string, 2)
	s := newServer(t, tickCh)
	ts := httptest.NewServer(s.Handler(context.Background()))
	defer ts.Close()

	body := repoPayload(t, "octo/hello")
	deliveryID := uuid.NewString()
	headers := map[string]string{
		"Content-Type":               "application/json",
		github.EventTypeHeader:       "issues",
		github.DeliveryIDHeader:      deliveryID,
		github.SHA256SignatureHeader: sha256Sig(testSecret, body),
	}

	first := doRequestRaw(t, ts.URL+"/webhook", body, headers)
	if first.StatusCode != http.StatusAccepted {
		t.Fatalf("first delivery status = %d, want 202", first.StatusCode)
	}
	if repo := waitTick(t, tickCh); repo != "octo/hello" {
		t.Errorf("Tick called with repo %q, want %q", repo, "octo/hello")
	}

	second := doRequestRaw(t, ts.URL+"/webhook", body, headers)
	if second.StatusCode != http.StatusOK {
		t.Fatalf("second delivery status = %d, want 200", second.StatusCode)
	}
	// The dedup check runs synchronously and short-circuits before step 10
	// ever dispatches Tick, so no goroutine can still be in flight here: an
	// empty channel is conclusive, not a race against a pending send.
	assertNoTick(t, tickCh)
}

// doRequestRaw sends exactly the headers given, with no defaults filled in,
// so a repeated delivery can reuse the identical X-Github-Delivery value.
func doRequestRaw(t *testing.T, url string, body []byte, headers map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	return resp
}

func TestInvalidRepositoryFullNameRejects(t *testing.T) {
	cases := []string{
		"",
		"a/b/c",
		strings.Repeat("x", 500),
	}
	for _, fullName := range cases {
		fullName := fullName
		t.Run("", func(t *testing.T) {
			tickCh := make(chan string, 1)
			s := newServer(t, tickCh)
			ts := httptest.NewServer(s.Handler(context.Background()))
			defer ts.Close()

			body := repoPayload(t, fullName)
			resp := doRequest(t, ts.URL+"/webhook", body, map[string]string{
				github.SHA256SignatureHeader: sha256Sig(testSecret, body),
			})
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("full_name %q: status = %d, want 400", fullName, resp.StatusCode)
			}
			assertNoTick(t, tickCh)
		})
	}
}

func TestGetMethodRejectedAndHealthzOK(t *testing.T) {
	tickCh := make(chan string, 1)
	s := newServer(t, tickCh)
	ts := httptest.NewServer(s.Handler(context.Background()))
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/webhook")
	if err != nil {
		t.Fatalf("GET /webhook: %v", err)
	}
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET /webhook status = %d, want 405", resp.StatusCode)
	}

	healthResp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	if healthResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /healthz status = %d, want 200", healthResp.StatusCode)
	}
	if body := readBody(t, healthResp); body != "ok" {
		t.Fatalf("GET /healthz body = %q, want %q", body, "ok")
	}
}

// TestTickReceivesDaemonContextNotRequestContext proves Handler's ctx
// parameter, not r.Context(), is what reaches Tick. net/http cancels
// r.Context() the instant ServeHTTP returns, which happens right after step
// 10 writes the 202 -- well before this test's fake Tick is even allowed to
// run. If Handler used r.Context() instead of the daemon-scoped one, ctx.Err()
// below would come back non-nil.
func TestTickReceivesDaemonContextNotRequestContext(t *testing.T) {
	release := make(chan struct{})
	ctxErrCh := make(chan error, 1)
	s, err := New(&Server{
		Secret: testSecret,
		Port:   freePort(t),
		Tick: func(ctx context.Context, _ string) {
			<-release
			ctxErrCh <- ctx.Err()
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ts := httptest.NewServer(s.Handler(context.Background()))
	defer ts.Close()

	body := repoPayload(t, "octo/hello")
	resp := doRequest(t, ts.URL+"/webhook", body, map[string]string{
		github.SHA256SignatureHeader: sha256Sig(testSecret, body),
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}

	// Drain and close the body so the client has the complete response,
	// which happens only after ServeHTTP has returned on the server side --
	// and r.Context() is therefore already cancelled -- before Tick is
	// allowed to proceed past <-release.
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatalf("drain response body: %v", err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatalf("close response body: %v", err)
	}

	close(release)

	select {
	case err := <-ctxErrCh:
		if err != nil {
			t.Fatalf("Tick's ctx.Err() = %v, want nil (Handler must not pass r.Context() to Tick)", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for Tick")
	}
}

// TestFullPoolDropsWithoutSecondTick is the DoS-bound control: with
// MaxInFlight 1, a second delivery arriving while the one slot is held must
// be dropped with 202 rather than blocking or, worse, spawning a second
// unbounded goroutine. A regression to a blocking `s.sem <- struct{}{}`
// would hang this test out to its deadline instead of passing quickly.
func TestFullPoolDropsWithoutSecondTick(t *testing.T) {
	tickStarted := make(chan struct{}, 2)
	release := make(chan struct{})
	s, err := New(&Server{
		Secret:      testSecret,
		Port:        freePort(t),
		MaxInFlight: 1,
		Tick: func(context.Context, string) {
			tickStarted <- struct{}{}
			<-release
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ts := httptest.NewServer(s.Handler(context.Background()))
	defer ts.Close()

	body1 := repoPayload(t, "octo/one")
	resp1 := doRequest(t, ts.URL+"/webhook", body1, map[string]string{
		github.SHA256SignatureHeader: sha256Sig(testSecret, body1),
	})
	if resp1.StatusCode != http.StatusAccepted {
		t.Fatalf("first delivery status = %d, want 202", resp1.StatusCode)
	}

	// Wait for the first Tick to actually be RUNNING, holding the one
	// semaphore slot, before sending the second delivery. Without this the
	// second request could race an empty pool and prove nothing.
	select {
	case <-tickStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the first Tick to start")
	}

	body2 := repoPayload(t, "octo/two")
	resp2 := doRequest(t, ts.URL+"/webhook", body2, map[string]string{
		github.SHA256SignatureHeader: sha256Sig(testSecret, body2),
	})
	if resp2.StatusCode != http.StatusAccepted {
		t.Fatalf("second delivery status = %d, want 202 (dropped, not blocked)", resp2.StatusCode)
	}

	// The second request's handling is entirely synchronous -- the pool-full
	// branch never spawns a goroutine -- so by the time resp2 was received,
	// no second Tick call could still be in flight. This check is therefore
	// conclusive, not a race against a pending send.
	select {
	case <-tickStarted:
		t.Fatal("second delivery must not have called Tick")
	default:
	}

	close(release)
}
