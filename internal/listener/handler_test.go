package listener

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha1" //nolint:gosec // needed to construct the downgrade attack this test proves is closed, not to protect anything
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-github/v77/github"
	"github.com/google/uuid"
)

const testSecret = "test-webhook-secret"

// fixedSecret is the Secret seam for a test that does not care about
// rotation: it hands back the same value on every call, the way a settings
// file nobody is editing does.
func fixedSecret(secret string) func() (string, error) {
	return func() (string, error) { return secret, nil }
}

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

// testIssueNumber is the subject of every fixture below that does not care
// which issue it names. It is not optional decoration: all five subscribed
// events carry a number, and a delivery without one is rejected, so a fixture
// that omitted it would exercise the rejection path instead of the case it was
// written for.
const testIssueNumber = 7

// repoPayload returns the JSON body of a minimal webhook delivery: the
// repository full_name and the issue number the event is about.
func repoPayload(t *testing.T, fullName string) []byte {
	t.Helper()
	return subjectPayload(t, fullName, "issue", testIssueNumber)
}

// subjectPayload returns a delivery whose subject ("issue" or "pull_request")
// carries number. number is any so a test can send what an attacker would: a
// string, a negative, a float.
func subjectPayload(t *testing.T, fullName, subject string, number any) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"repository": map[string]any{"full_name": fullName},
		subject:      map[string]any{"number": number},
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return body
}

// tickCall is one call to the Server's Tick seam: the repository and the issue
// number the delivery named.
type tickCall struct {
	repo       string
	number     int
	mergedInto string
}

// newServer builds a Server wired to tickCh: the fake Tick sends the repo
// name and returns. Tests synchronise on tickCh, never on a sleep or a
// counter, so they stay correct under -race.
func newServer(t *testing.T, tickCh chan<- tickCall) *Server {
	t.Helper()
	s, err := New(&Server{
		Secret: fixedSecret(testSecret),
		Port:   freePort(t),
		Tick: func(_ context.Context, d Delivery) {
			tickCh <- tickCall{repo: d.Repo, number: d.Number, mergedInto: d.MergedInto}
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
func waitTick(t *testing.T, ch <-chan tickCall) tickCall {
	t.Helper()
	select {
	case call := <-ch:
		return call
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for Tick")
		return tickCall{}
	}
}

// assertNoTick fails if a value is already waiting on ch. It is only valid
// right after a request whose handling is entirely synchronous up to the
// point being tested (no goroutine could still be in flight), which every
// rejection path in Handler satisfies: Tick is dispatched only at step 10,
// after every check this package makes.
func assertNoTick(t *testing.T, ch <-chan tickCall) {
	t.Helper()
	select {
	case call := <-ch:
		t.Fatalf("Tick called unexpectedly with %+v", call)
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
	tickCh := make(chan tickCall, 1)
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
	if got := waitTick(t, tickCh); got.repo != "octo/hello" {
		t.Errorf("Tick called with repo %q, want %q", got.repo, "octo/hello")
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
	tickCh := make(chan tickCall, 1)
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
	tickCh := make(chan tickCall, 1)
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
	tickCh := make(chan tickCall, 1)
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
	tickCh := make(chan tickCall, 1)
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
	tickCh := make(chan tickCall, 1)
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

// The one invariant this package's doc is written around: with an empty
// secret, github.ValidatePayloadFromBody computes the HMAC with a key the
// attacker also knows, so the endpoint accepts anything and starts a coding
// agent for it. settings.Load returns a zero value when its file is absent, so
// an empty secret is a state this program can reach.
//
// Port is set deliberately. Left at zero, New fails on the Port check first
// and this test passes with the secret refusal deleted -- which is exactly how
// it used to pass. The error text is asserted for the same reason.
func TestNewRefusesEmptySecret(t *testing.T) {
	_, err := New(&Server{
		Secret: fixedSecret(""),
		Port:   8080,
		Tick:   func(context.Context, Delivery) {},
	})
	if err == nil {
		t.Fatal("New with an empty secret must return an error")
	}
	if !strings.Contains(err.Error(), "empty webhook secret") {
		t.Fatalf("err = %v, want the refusal to name the empty webhook secret", err)
	}
}

// A Secret seam that cannot answer is not an excuse to serve unauthenticated:
// New refuses at start rather than letting every delivery discover it.
func TestNewRefusesAnUnreadableSecret(t *testing.T) {
	_, err := New(&Server{
		Secret: func() (string, error) { return "", errors.New("settings unreadable") },
		Port:   8080,
		Tick:   func(context.Context, Delivery) {},
	})
	if err == nil {
		t.Fatal("New with an unreadable secret must return an error")
	}
}

// Rotating the secret (`config webhook --rotate-secret`) must not require
// restarting the daemon. A secret read once at start would make GitHub sign
// with the new value while the daemon verified with the old, 401ing every
// delivery and filling the log with signature failures indistinguishable from
// an attack.
func TestSecretIsReReadPerRequest(t *testing.T) {
	tickCh := make(chan tickCall, 1)
	var mu sync.Mutex
	secret := testSecret
	s, err := New(&Server{
		Secret: func() (string, error) {
			mu.Lock()
			defer mu.Unlock()
			return secret, nil
		},
		Port: freePort(t),
		Tick: func(_ context.Context, d Delivery) {
			tickCh <- tickCall{repo: d.Repo, number: d.Number, mergedInto: d.MergedInto}
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ts := httptest.NewServer(s.Handler(context.Background()))
	defer ts.Close()

	// The operator rotates it, and GitHub starts signing with the new value.
	mu.Lock()
	secret = "rotated-webhook-secret"
	mu.Unlock()

	body := repoPayload(t, "octo/hello")
	resp := doRequest(t, ts.URL+"/webhook", body, map[string]string{
		github.SHA256SignatureHeader: sha256Sig("rotated-webhook-secret", body),
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: the rotated secret must be picked up without a restart", resp.StatusCode)
	}
	if got := waitTick(t, tickCh); got.repo != "octo/hello" {
		t.Fatalf("Tick repo = %q", got)
	}

	// And the old secret is genuinely no longer accepted.
	old := doRequest(t, ts.URL+"/webhook", body, map[string]string{
		github.SHA256SignatureHeader: sha256Sig(testSecret, body),
	})
	if old.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d for the superseded secret, want 401", old.StatusCode)
	}
}

// Every rejection below step 6 is written by an anonymous caller: slog goes to
// stdout as JSON and launchd appends that to an unrotated file in the
// operator's home directory. A few hundred requests a second of garbage would
// otherwise write gigabytes a day and KeepAlive would respawn the daemon
// against a full disk.
func TestRejectionLoggingIsRateLimitedPerStage(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	l := newThrottledLog(time.Minute)
	l.now = func() time.Time { return now }

	ok, suppressed := l.allow("method")
	if !ok || suppressed != 0 {
		t.Fatalf("first rejection: ok=%v suppressed=%d, want true/0", ok, suppressed)
	}
	for i := 0; i < 500; i++ {
		if ok, _ := l.allow("method"); ok {
			t.Fatalf("rejection %d was logged inside the interval", i)
		}
	}
	// A different stage is a different key: an operator must still see the
	// first of each kind.
	if ok, _ := l.allow("content type"); !ok {
		t.Fatal("a different stage was suppressed by another stage's interval")
	}

	// Once the interval passes, one line gets through and carries the count it
	// stands for -- a silently sampled log would understate an attack.
	now = now.Add(time.Minute)
	ok, suppressed = l.allow("method")
	if !ok {
		t.Fatal("nothing was logged after the interval passed")
	}
	if suppressed != 500 {
		t.Fatalf("suppressed = %d, want 500", suppressed)
	}
}

func TestContentTypeWithCharsetAccepted(t *testing.T) {
	tickCh := make(chan tickCall, 1)
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
	tickCh := make(chan tickCall, 1)
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
	tickCh := make(chan tickCall, 1)
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
	tickCh := make(chan tickCall, 1)
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
	tickCh := make(chan tickCall, 2)
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
	if got := waitTick(t, tickCh); got.repo != "octo/hello" {
		t.Errorf("Tick called with repo %q, want %q", got.repo, "octo/hello")
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
			tickCh := make(chan tickCall, 1)
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
	tickCh := make(chan tickCall, 1)
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
		Secret: fixedSecret(testSecret),
		Port:   freePort(t),
		Tick: func(ctx context.Context, _ Delivery) {
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
		Secret:      fixedSecret(testSecret),
		Port:        freePort(t),
		MaxInFlight: 1,
		Tick: func(context.Context, Delivery) {
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

// A delivery the pool drops must stay recoverable. GitHub's "Redeliver" sends
// the SAME guid, so an id cached for work that never ran answers every future
// attempt with "duplicate, skipping tick" and the work is lost outright: Wake
// fires only on retry deadlines, and the next delivery may be days away.
func TestAPoolDroppedDeliveryIsRecoverableByRedelivery(t *testing.T) {
	tickStarted := make(chan string, 2)
	release := make(chan struct{})
	s, err := New(&Server{
		Secret:      fixedSecret(testSecret),
		Port:        freePort(t),
		MaxInFlight: 1,
		Tick: func(_ context.Context, d Delivery) {
			tickStarted <- d.Repo
			<-release
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ts := httptest.NewServer(s.Handler(context.Background()))
	defer ts.Close()

	first := repoPayload(t, "octo/one")
	if resp := doRequest(t, ts.URL+"/webhook", first, map[string]string{
		github.SHA256SignatureHeader: sha256Sig(testSecret, first),
	}); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("first delivery status = %d, want 202", resp.StatusCode)
	}
	select {
	case <-tickStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the first Tick to start")
	}

	// The pool is full, so this one is dropped. Its guid is fixed, because
	// that is what "Redeliver" reuses.
	body := repoPayload(t, "octo/two")
	guid := uuid.NewString()
	headers := map[string]string{
		github.SHA256SignatureHeader: sha256Sig(testSecret, body),
		github.DeliveryIDHeader:      guid,
	}
	if resp := doRequest(t, ts.URL+"/webhook", body, headers); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("dropped delivery status = %d, want 202", resp.StatusCode)
	}

	// The slot frees up and the operator hits Redeliver. Drain is what proves
	// the pool goroutine has finished and given the slot back, so the second
	// delivery is not racing it.
	close(release)
	s.Drain()

	if resp := doRequest(t, ts.URL+"/webhook", body, headers); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("redelivery status = %d, want 202: a dropped delivery must not be cached as seen", resp.StatusCode)
	}
	select {
	case repo := <-tickStarted:
		if repo != "octo/two" {
			t.Fatalf("redelivery ticked %q, want octo/two", repo)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the redelivery of a dropped delivery did not tick; the work is unrecoverable")
	}
}

// TestDrainWaitsForInFlightTickStartedByThePool proves Server.Drain gives a
// real happens-before guarantee, not just an eventual one: it must not
// return while a Tick the pool goroutine started is still running, and it
// must return once that Tick finishes. A regression to Add(1) placed inside
// the spawned goroutine (rather than before it is spawned) would make this
// flaky rather than reliably fail, since the race is between Drain's Wait
// and the goroutine's own first instruction -- this test's release gate is
// what turns that race into something deterministic to assert against.
func TestDrainWaitsForInFlightTickStartedByThePool(t *testing.T) {
	tickStarted := make(chan struct{})
	release := make(chan struct{})
	s, err := New(&Server{
		Secret: fixedSecret(testSecret),
		Port:   freePort(t),
		Tick: func(context.Context, Delivery) {
			close(tickStarted)
			<-release
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

	select {
	case <-tickStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for Tick to start")
	}

	drainDone := make(chan struct{})
	go func() {
		s.Drain()
		close(drainDone)
	}()

	select {
	case <-drainDone:
		t.Fatal("Drain returned while Tick was still running")
	case <-time.After(100 * time.Millisecond):
	}

	close(release)

	select {
	case <-drainDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Drain did not return after Tick finished")
	}
}

// TestDrainReturnsImmediatelyWithNothingInFlight covers the common case: a
// caller shutting down a listener that has no in-flight delivery must not
// block at all.
func TestDrainReturnsImmediatelyWithNothingInFlight(t *testing.T) {
	s, err := New(&Server{
		Secret: fixedSecret(testSecret),
		Port:   freePort(t),
		Tick:   func(context.Context, Delivery) {},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	done := make(chan struct{})
	go func() {
		s.Drain()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Drain blocked with nothing in flight")
	}
}

// repoActionPayload returns the JSON body of a webhook delivery carrying a
// repository full_name and the "action" field the issue and pull request
// events carry (opened, labeled, closed...).
func repoActionPayload(t *testing.T, fullName, action string) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"action":     action,
		"repository": map[string]any{"full_name": fullName},
		"issue":      map[string]any{"number": testIssueNumber},
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return body
}

// An ACCEPTED delivery must log what it was. Every rejection and every skip
// already logs; success logged nothing, so the ticks a delivery causes
// appeared in the operator's log with no line explaining why. The report that
// prompted this: a fresh unlabelled issue was created, and seconds later a
// tend was dispatched for a DIFFERENT issue. That turned out to be a real bug
// -- a delivery now acts only on the issue it names -- and nothing in the log
// said a delivery had arrived at all, which is what made it unreadable.
func TestAnAcceptedDeliveryIsLogged(t *testing.T) {
	logs := captureLogs(t)
	tickCh := make(chan tickCall, 1)
	s := newServer(t, tickCh)
	ts := httptest.NewServer(s.Handler(context.Background()))
	defer ts.Close()

	body := repoActionPayload(t, "octo/hello", "opened")
	deliveryID := uuid.NewString()
	resp := doRequest(t, ts.URL+"/webhook", body, map[string]string{
		github.SHA256SignatureHeader: sha256Sig(testSecret, body),
		github.DeliveryIDHeader:      deliveryID,
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}
	waitTick(t, tickCh)

	out := logs.String()
	for _, want := range []string{
		"accepted delivery", "event=issues", "action=opened",
		"repo=octo/hello", "delivery=" + deliveryID,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the accepted delivery log does not carry %q:\n%s", want, out)
		}
	}
}

// The action is decoded out of the payload, so it is bounded and shape-checked
// before it reaches a log line for the same reason the delivery id and the
// repository full_name are: an unbounded string with an embedded newline in it
// would otherwise forge a whole log record in the operator's file.
func TestAnOutOfShapeActionIsNotLoggedVerbatim(t *testing.T) {
	logs := captureLogs(t)
	tickCh := make(chan tickCall, 1)
	s := newServer(t, tickCh)
	ts := httptest.NewServer(s.Handler(context.Background()))
	defer ts.Close()

	evil := "opened\nlevel=ERROR msg=\"the daemon is on fire\"" + strings.Repeat("a", 500)
	body := repoActionPayload(t, "octo/hello", evil)
	resp := doRequest(t, ts.URL+"/webhook", body, map[string]string{
		github.SHA256SignatureHeader: sha256Sig(testSecret, body),
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}
	waitTick(t, tickCh)

	out := logs.String()
	if strings.Contains(out, "the daemon is on fire") {
		t.Errorf("the raw action reached the log:\n%s", out)
	}
	if !strings.Contains(out, "action=<invalid>") {
		t.Errorf("an out-of-shape action must be reported as such:\n%s", out)
	}
}

// An event that carries no action at all (push, for one) must still log its
// accepted line, and must not report a missing field as an invalid one: an
// operator who sees "<invalid>" goes looking for an attack.
func TestAnAcceptedDeliveryWithNoActionIsStillLogged(t *testing.T) {
	logs := captureLogs(t)
	tickCh := make(chan tickCall, 1)
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
	waitTick(t, tickCh)

	out := logs.String()
	if !strings.Contains(out, "accepted delivery") {
		t.Errorf("no accepted line for an action-less event:\n%s", out)
	}
	if strings.Contains(out, "action=<invalid>") {
		t.Errorf("an absent action must not be reported as invalid:\n%s", out)
	}
}

// A delivery says WHICH issue changed, and that number is the whole point of
// the delivery: the tick it starts decides that issue and nothing else.
// Passing only the repository is what made an unlabelled test issue dispatch
// an agent for an unrelated one.
func TestAnIssueEventPassesItsNumberToTick(t *testing.T) {
	tickCh := make(chan tickCall, 1)
	s := newServer(t, tickCh)
	ts := httptest.NewServer(s.Handler(context.Background()))
	defer ts.Close()

	body := subjectPayload(t, "octo/hello", "issue", 51)
	resp := doRequest(t, ts.URL+"/webhook", body, map[string]string{
		github.SHA256SignatureHeader: sha256Sig(testSecret, body),
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}
	if got := waitTick(t, tickCh); got.repo != "octo/hello" || got.number != 51 {
		t.Fatalf("Tick called with %+v, want octo/hello #51", got)
	}
}

// Pull requests are also technically issues: they share the number space, and
// three of the five subscribed events carry pull_request.number rather than
// issue.number. Reading only one of the two fields would silently drop those
// deliveries.
func TestAPullRequestEventPassesItsNumberToTick(t *testing.T) {
	tickCh := make(chan tickCall, 1)
	s := newServer(t, tickCh)
	ts := httptest.NewServer(s.Handler(context.Background()))
	defer ts.Close()

	body := subjectPayload(t, "octo/hello", "pull_request", 108)
	resp := doRequest(t, ts.URL+"/webhook", body, map[string]string{
		github.SHA256SignatureHeader: sha256Sig(testSecret, body),
		github.EventTypeHeader:       "pull_request",
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}
	if got := waitTick(t, tickCh); got.number != 108 {
		t.Fatalf("Tick called with %+v, want #108", got)
	}
}

// The number is decoded out of an attacker-controlled payload, like the
// repository full_name, so it is validated before it is used or logged. A
// delivery carrying no usable number names nothing to act on: answering it
// 202 would accept work this daemon cannot do and hide the malformed delivery
// from the operator.
func TestADeliveryWithNoUsableNumberIsRejected(t *testing.T) {
	cases := []struct {
		name string
		body func(t *testing.T) []byte
	}{
		{"no subject at all", func(t *testing.T) []byte {
			t.Helper()
			body, err := json.Marshal(map[string]any{
				"repository": map[string]any{"full_name": "octo/hello"},
			})
			if err != nil {
				t.Fatalf("marshal payload: %v", err)
			}
			return body
		}},
		{"zero", func(t *testing.T) []byte {
			return subjectPayload(t, "octo/hello", "issue", 0)
		}},
		{"negative", func(t *testing.T) []byte {
			return subjectPayload(t, "octo/hello", "issue", -1)
		}},
		{"not a number", func(t *testing.T) []byte {
			return subjectPayload(t, "octo/hello", "issue", "51")
		}},
		{"wider than an int", func(t *testing.T) []byte {
			return subjectPayload(t, "octo/hello", "issue", json.RawMessage("99999999999999999999"))
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tickCh := make(chan tickCall, 1)
			s := newServer(t, tickCh)
			ts := httptest.NewServer(s.Handler(context.Background()))
			defer ts.Close()

			body := c.body(t)
			resp := doRequest(t, ts.URL+"/webhook", body, map[string]string{
				github.SHA256SignatureHeader: sha256Sig(testSecret, body),
			})
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", resp.StatusCode)
			}
			assertNoTick(t, tickCh)
		})
	}
}

// The accepted line has to name the issue, not just the repository: it is the
// only record tying a delivery to the one tick it caused, and "a delivery
// arrived for octo/hello" cannot be matched against what the human just did.
func TestAnAcceptedDeliveryLogsTheIssueItNames(t *testing.T) {
	logs := captureLogs(t)
	tickCh := make(chan tickCall, 1)
	s := newServer(t, tickCh)
	ts := httptest.NewServer(s.Handler(context.Background()))
	defer ts.Close()

	body := subjectPayload(t, "octo/hello", "issue", 51)
	resp := doRequest(t, ts.URL+"/webhook", body, map[string]string{
		github.SHA256SignatureHeader: sha256Sig(testSecret, body),
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}
	waitTick(t, tickCh)

	out := logs.String()
	if !strings.Contains(out, "number=51") {
		t.Errorf("the accepted delivery log does not name the issue:\n%s", out)
	}
	// The old line claimed a repository-wide reconcile, which is no longer
	// what a delivery does.
	if strings.Contains(out, "reconcil") {
		t.Errorf("an accepted delivery still claims a reconcile:\n%s", out)
	}
}

// The handler must report a merge, and must NOT report anything else as one.
// MergedInto is what arms a repository-wide sweep, so a false positive here is
// the regression Worker.RunIssue records, reintroduced at the front door.
func TestHandlerReportsOnlyAMergedPullRequestAsAMerge(t *testing.T) {
	cases := []struct {
		name    string
		event   string
		payload string
		want    string
	}{
		{
			name:    "a merged pull request",
			event:   "pull_request",
			payload: `{"action":"closed","repository":{"full_name":"o/r"},"pull_request":{"number":7,"merged":true,"base":{"ref":"master"}}}`,
			want:    "master",
		},
		{
			name:    "a closed but unmerged pull request",
			event:   "pull_request",
			payload: `{"action":"closed","repository":{"full_name":"o/r"},"pull_request":{"number":7,"merged":false,"base":{"ref":"master"}}}`,
			want:    "",
		},
		{
			// GitHub sends merged: true on later pull_request actions too.
			// Only the close is the moment the base branch moved.
			name:    "an edited pull request that was already merged",
			event:   "pull_request",
			payload: `{"action":"edited","repository":{"full_name":"o/r"},"pull_request":{"number":7,"merged":true,"base":{"ref":"master"}}}`,
			want:    "",
		},
		{
			// issue_comment carries a pull_request object too, and its action
			// is attacker-shaped text: with a closed action and a merged flag,
			// only the EVENT check separates this from a real merge.
			name:    "a comment delivery whose payload claims a closed merge",
			event:   "issue_comment",
			payload: `{"action":"closed","repository":{"full_name":"o/r"},"issue":{"number":7},"pull_request":{"number":7,"merged":true,"base":{"ref":"master"}}}`,
			want:    "",
		},
		{
			// pull_request_review carries a pull_request object as well, and
			// it is not a merge whatever that object says.
			name:    "a review on a merged pull request",
			event:   "pull_request_review",
			payload: `{"action":"submitted","repository":{"full_name":"o/r"},"pull_request":{"number":7,"merged":true,"base":{"ref":"master"}}}`,
			want:    "",
		},
		{
			name:    "an issue delivery",
			event:   "issues",
			payload: `{"action":"labeled","repository":{"full_name":"o/r"},"issue":{"number":7}}`,
			want:    "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tickCh := make(chan tickCall, 1)
			s := newServer(t, tickCh)
			srv := httptest.NewServer(s.Handler(context.Background()))
			t.Cleanup(srv.Close)

			body := []byte(tc.payload)
			resp := doRequest(t, srv.URL+"/webhook", body, map[string]string{
				github.EventTypeHeader:       tc.event,
				github.SHA256SignatureHeader: sha256Sig(testSecret, body),
			})
			defer resp.Body.Close()

			got := waitTick(t, tickCh)
			if got.mergedInto != tc.want {
				t.Errorf("mergedInto = %q, want %q", got.mergedInto, tc.want)
			}
			if got.number != 7 {
				t.Errorf("number = %d, want 7", got.number)
			}
		})
	}
}
