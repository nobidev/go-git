package http

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	transport "github.com/go-git/go-git/v6/plumbing/transport"
)

func TestCheckError_SuccessCodes(t *testing.T) {
	t.Parallel()
	for code := http.StatusOK; code < http.StatusMultipleChoices; code++ {
		assert.NoError(t, checkError(&http.Response{StatusCode: code}))
	}
}

func TestCheckError_Unauthorized(t *testing.T) {
	t.Parallel()
	req, _ := http.NewRequest("GET", "https://example.com/repo.git", nil)
	resp := &http.Response{
		Request:    req,
		StatusCode: http.StatusUnauthorized,
		Body:       io.NopCloser(strings.NewReader("auth needed")),
	}
	err := checkError(resp)
	require.Error(t, err)
	assert.True(t, errors.Is(err, transport.ErrAuthenticationRequired))
	var httpErr *Err
	assert.True(t, errors.As(err, &httpErr))
	assert.Equal(t, http.StatusUnauthorized, httpErr.StatusCode())
}

func TestCheckError_Forbidden(t *testing.T) {
	t.Parallel()
	req, _ := http.NewRequest("GET", "https://example.com/repo.git", nil)
	resp := &http.Response{
		Request:    req,
		StatusCode: http.StatusForbidden,
		Body:       io.NopCloser(strings.NewReader("forbidden")),
	}
	err := checkError(resp)
	require.Error(t, err)
	assert.True(t, errors.Is(err, transport.ErrAuthorizationFailed))
}

func TestCheckError_NotFound(t *testing.T) {
	t.Parallel()
	req, _ := http.NewRequest("GET", "https://example.com/repo.git", nil)
	resp := &http.Response{
		Request:    req,
		StatusCode: http.StatusNotFound,
		Body:       io.NopCloser(strings.NewReader("not found")),
	}
	err := checkError(resp)
	require.Error(t, err)
	assert.True(t, errors.Is(err, transport.ErrRepositoryNotFound))
}

func TestCheckError_Unknown(t *testing.T) {
	t.Parallel()
	req, _ := http.NewRequest("GET", "https://example.com/repo.git", nil)
	resp := &http.Response{
		Request:    req,
		StatusCode: http.StatusPaymentRequired,
		Body:       io.NopCloser(strings.NewReader("pay up")),
	}
	err := checkError(resp)
	require.Error(t, err)
	var httpErr *Err
	assert.True(t, errors.As(err, &httpErr))
	assert.Equal(t, http.StatusPaymentRequired, httpErr.StatusCode())
	assert.Equal(t, "pay up", httpErr.Reason)
}

func TestCheckError_WithReason(t *testing.T) {
	t.Parallel()
	req, _ := http.NewRequest("GET", "https://example.com/repo.git", nil)
	resp := &http.Response{
		Request:    req,
		StatusCode: http.StatusInternalServerError,
		Body:       io.NopCloser(strings.NewReader("server error details")),
	}
	err := checkError(resp)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "server error details")
}

func TestErr_ErrorRedactsCredentials(t *testing.T) {
	t.Parallel()
	req, _ := http.NewRequest("GET", "https://user:s3cr3t@example.com/repo.git/info/refs?service=git-upload-pack", nil)
	resp := &http.Response{
		Request:    req,
		StatusCode: http.StatusInternalServerError,
		Body:       io.NopCloser(strings.NewReader("boom")),
	}
	err := checkError(resp)
	require.Error(t, err)
	msg := err.Error()
	assert.NotContains(t, msg, "s3cr3t")
	assert.Contains(t, msg, "REDACTED")
	// the rest of the URL is still reported so the error stays useful
	assert.Contains(t, msg, "example.com/repo.git")
}

func TestApplyRedirect(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		baseURL          string
		finalURL         string
		wantURL          string
		wantErr          string
		wantAuthRequired bool
		noRequest        bool
	}{
		{
			name:      "no redirect",
			baseURL:   "https://example.com/repo.git",
			wantURL:   "https://example.com/repo.git",
			noRequest: true,
		},
		{
			name:     "redirect updates host",
			baseURL:  "https://old.example.com/repo.git",
			finalURL: "https://new.example.com/repo.git/info/refs",
			wantURL:  "https://new.example.com/repo.git",
		},
		{
			name:     "same host and path is no-op",
			baseURL:  "https://example.com/repo.git",
			finalURL: "https://example.com/repo.git/info/refs",
			wantURL:  "https://example.com/repo.git",
		},
		{
			name:     "unsupported scheme",
			baseURL:  "https://example.com/repo.git",
			finalURL: "ftp://evil.com/repo.git/info/refs",
			wantErr:  "unsupported scheme",
		},
		{
			name:     "tail mismatch",
			baseURL:  "https://example.com/repo.git",
			finalURL: "https://evil.com/malicious-path",
			wantErr:  "does not end with",
		},
		{
			name:     "redirect updates scheme for http to https",
			baseURL:  "http://example.com/repo.git",
			finalURL: "https://example.com/repo.git/info/refs",
			wantURL:  "https://example.com/repo.git",
		},
		{
			name:     "redirect rejects scheme downgrade",
			baseURL:  "https://example.com/repo.git",
			finalURL: "http://example.com/repo.git/info/refs",
			wantErr:  "changes scheme",
		},
		{
			name:     "redirect updates path",
			baseURL:  "https://example.com/old-repo.git",
			finalURL: "https://example.com/new-repo.git/info/refs",
			wantURL:  "https://example.com/new-repo.git",
		},
		{
			name:     "redirect to bare repo path errors",
			baseURL:  "https://example.com/repo.git",
			finalURL: "https://example.com/repo.git",
			wantErr:  "does not end with",
		},
		{
			name:             "azure devops _signin redirect is auth required",
			baseURL:          "https://dev.azure.com/org/project/_git/repo",
			finalURL:         "https://dev.azure.com/org/_signin",
			wantErr:          "redirect to",
			wantAuthRequired: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			base, err := url.Parse(tt.baseURL)
			require.NoError(t, err)

			resp := &http.Response{}
			if !tt.noRequest {
				req, err := http.NewRequest("GET", tt.finalURL, nil)
				require.NoError(t, err)
				resp.Request = req
			}

			result, err := applyRedirect(resp, base)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				if tt.wantAuthRequired {
					assert.True(t, errors.Is(err, transport.ErrAuthenticationRequired),
						"expected error to wrap transport.ErrAuthenticationRequired")
				}
				return
			}

			require.NoError(t, err)
			want, err := url.Parse(tt.wantURL)
			require.NoError(t, err)
			assert.Equal(t, want, result)
		})
	}
}

func TestCredentialsMayFollow(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		from string
		to   string
		want bool
	}{
		{"identical", "https://example.test/a", "https://example.test/a", true},
		{"path differs only", "https://example.test/a", "https://example.test/b", true},
		{"explicit default port on the right", "https://example.test/a", "https://example.test:443/a", true},
		{"explicit default port on the left", "http://example.test:80/a", "http://example.test/a", true},
		{"leading zero port", "https://example.test/a", "https://example.test:0443/a", true},
		{"trailing root dot", "https://example.test/a", "https://example.test./a", true},
		{"uppercase host", "https://EXAMPLE.test/a", "https://example.test/a", true},
		{"unicode host, same spelling", "https://ẞexample.test/a", "https://ẞexample.test/a", true},
		{"http to https upgrade", "http://example.test/a", "https://example.test/a", true},
		{"ipv4 literal", "http://127.0.0.1:8080/a", "http://127.0.0.1:8080/a", true},
		{"ipv6 literal", "http://[::1]:8080/a", "http://[::1]:8080/a", true},
		{"host with underscore", "http://build_host:8080/a", "http://build_host:8080/a", true},

		{"https to http downgrade", "https://example.test/a", "http://example.test/a", false},
		{"subdomain", "https://example.test/a", "https://sub.example.test/a", false},
		{"parent domain", "https://sub.example.test/a", "https://example.test/a", false},
		{"different port", "https://example.test/a", "https://example.test:8443/a", false},
		{"unrelated host", "https://example.test/a", "https://evil.test/a", false},
		{"suffix but not subdomain", "https://example.test/a", "https://notexample.test/a", false},
		{"upgrade to a non-default https port", "http://example.test/a", "https://example.test:8443/a", false},

		// Hosts here are deliberately ASCII. Which internationalised hosts
		// IDNA maps together depends on its Unicode tables and on whether
		// the profile applies transitional processing, both of which change
		// between x/net releases, so a hard-coded expectation for such a
		// pair is not stable. TestCanonicalHostTracksDialTarget covers those
		// against net/http's own normalisation instead.
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			from, err := url.Parse(tt.from)
			require.NoError(t, err)
			to, err := url.Parse(tt.to)
			require.NoError(t, err)

			assert.Equal(t, tt.want, credentialsMayFollow(from, to))
		})
	}
}

func TestEffectivePort(t *testing.T) {
	t.Parallel()

	tests := []struct {
		rawURL string
		want   string
	}{
		{"http://example.test/a", "80"},
		{"https://example.test/a", "443"},
		{"http://example.test:8080/a", "8080"},
		{"https://example.test:0443/a", "443"},
		{"https://example.test:080/a", "80"},
		{"ftp://example.test/a", ""},
	}

	for _, tt := range tests {
		t.Run(tt.rawURL, func(t *testing.T) {
			t.Parallel()

			u, err := url.Parse(tt.rawURL)
			require.NoError(t, err)
			assert.Equal(t, tt.want, effectivePort(u))
		})
	}
}

// dialTarget returns the host net/http resolves rawURL to immediately before
// it opens a connection. It is captured from a real http.Client round trip
// whose dialer records the address and then refuses to connect, so it is
// net/http's own normalisation rather than a reimplementation of it.
func dialTarget(t *testing.T, rawURL string) string {
	t.Helper()

	var addr string
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(_ context.Context, _, a string) (net.Conn, error) {
			addr = a
			return nil, errors.New("dial refused by test")
		},
	}}

	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	require.NoError(t, err)
	resp, err := client.Do(req)
	require.Error(t, err, "the test dialer should have refused the connection")
	if resp != nil {
		_ = resp.Body.Close()
	}
	require.NotEmpty(t, addr, "net/http never dialled for %s", rawURL)

	host, _, err := net.SplitHostPort(addr)
	require.NoError(t, err)
	return host
}

// TestCanonicalHostTracksDialTarget pins the invariant the whole origin
// comparison rests on: canonicalHost must call two hosts the same origin
// exactly when net/http would open a connection to the same server.
//
// Expectations are derived from net/http at run time rather than hard-coded,
// because which internationalised hosts IDNA maps together depends on its
// Unicode tables and on transitional processing, and those change between
// x/net releases. A table of constants for such hosts passes on one release
// and fails on the next without anything being wrong — which is exactly what
// happened to an earlier version of this test. Deriving the expectation makes
// the property hold across releases: if a release starts mapping a pair
// differently, net/http and canonicalHost move together and the test still
// passes; if they ever disagree, it fails, which is the bug worth catching.
//
// net/http's normalisation is relaxed here by the two things DNS itself
// treats as equivalent — ASCII case and the trailing root dot — because those
// are the only liberties canonicalHost takes beyond it.
func TestCanonicalHostTracksDialTarget(t *testing.T) {
	t.Parallel()

	hosts := []string{
		// Spellings that must come out equal: ASCII case, the trailing root
		// dot, and punycode against the unicode form it encodes.
		"example.test",
		"EXAMPLE.test",
		"example.test.",
		"EXAMPLE.TEST.",
		"sub.example.test",
		"SUB.EXAMPLE.test",
		"xn--xample-20e.test",
		"XN--XAMPLE-20E.test",

		// Spellings whose relationship depends on the IDNA tables, which is
		// why nothing here is hard-coded.
		"ςxample.test",  // U+03C2 greek final sigma
		"σxample.test",  // U+03C3 greek small sigma
		"ẞexample.test", // U+1E9E capital sharp s
		"ßexample.test", // U+00DF small sharp s

		// Spellings that must come out different.
		"notexample.test",
		"evil.test",

		// Hosts IDNA rejects, exercising the fallback.
		"127.0.0.1",
		"[::1]",
		"build_host",
	}

	dialed := make(map[string]string, len(hosts))
	parsed := make(map[string]*url.URL, len(hosts))
	for _, h := range hosts {
		u, err := url.Parse("http://" + h + "/repo.git")
		require.NoError(t, err, "host %q", h)
		parsed[h] = u
		dialed[h] = dialTarget(t, u.String())
	}

	// dnsEquivalent reports whether net/http would reach the same server for
	// two hosts, ignoring the ASCII case and the root dot that DNS ignores.
	dnsEquivalent := func(a, b string) bool {
		return asciiLower(strings.TrimSuffix(a, ".")) == asciiLower(strings.TrimSuffix(b, "."))
	}

	var same, different, foldTraps int
	for i := range hosts {
		for j := i + 1; j < len(hosts); j++ {
			a, b := hosts[i], hosts[j]

			want := dnsEquivalent(dialed[a], dialed[b])
			got := canonicalHost(parsed[a]) == canonicalHost(parsed[b])
			assert.Equal(t, want, got,
				"canonicalHost disagreed with net/http: %q dials %q, %q dials %q",
				a, dialed[a], b, dialed[b])

			if want {
				same++
			} else {
				different++
			}

			// Pairs that strings.EqualFold would conflate but net/http keeps
			// apart are the ones that make comparing hostnames by case-folding
			// unsafe. Asserted only when the running x/net actually produces
			// such a pair, so a table change cannot fail this spuriously.
			if !want && strings.EqualFold(a, b) {
				foldTraps++
				assert.False(t, got,
					"case-folding would conflate %q and %q, but net/http dials %q and %q",
					a, b, dialed[a], dialed[b])
			}
		}
	}

	// Guard against the corpus degenerating into a one-sided test. Both counts
	// are guaranteed non-zero by pairs that need no IDNA at all
	// (example.test/EXAMPLE.test and example.test/evil.test).
	require.NotZero(t, same, "corpus exercised no same-origin pairs")
	require.NotZero(t, different, "corpus exercised no different-origin pairs")
	t.Logf("compared %d host pairs against net/http: %d same, %d different, %d case-fold traps",
		same+different, same, different, foldTraps)
}
