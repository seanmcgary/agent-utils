package ghub

import (
	"net/http"
	"strconv"

	"github.com/seanmcgary/agent-utils/internal/metrics"
)

// mRateLimitRemaining is the gauge every GitHub API response updates.
const mRateLimitRemaining = "github.rate_limit.remaining"

// rateTransport reports GitHub's rate-limit headers as a gauge, from every
// response this program's API client receives.
//
// A transport, not a call in each method: New and NewApp are the only two
// places an API client is built, so wrapping their transport covers every
// request without any method knowing. go-github parses the same headers into
// Response.Rate, but reading them there would mean touching each of the
// dozens of call sites in this package that discard the Response.
//
// The resource tag separates the budgets GitHub keeps apart -- core, search,
// graphql -- because a search budget at zero says nothing about whether the
// tick can still list issues. Nothing else is tagged: in token mode every
// response shares one budget, and in app mode each installation has its own,
// but naming an installation would put an account identifier in a tag.
type rateTransport struct {
	base http.RoundTripper
}

func (t rateTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	resp, err := base.RoundTrip(req)
	if err != nil || resp == nil {
		return resp, err
	}
	if v := resp.Header.Get("X-RateLimit-Remaining"); v != "" {
		if n, perr := strconv.Atoi(v); perr == nil {
			metrics.Default().Gauge(mRateLimitRemaining, float64(n),
				metrics.Tag("resource", resp.Header.Get("X-RateLimit-Resource")))
		}
	}
	return resp, nil
}

// withRateMetrics returns a copy of hc whose transport reports rate-limit
// headers. A nil hc is treated as a default client, which is what
// github.NewClient(nil) would have built.
func withRateMetrics(hc *http.Client) *http.Client {
	out := &http.Client{}
	if hc != nil {
		cp := *hc
		out = &cp
	}
	out.Transport = rateTransport{base: out.Transport}
	return out
}
