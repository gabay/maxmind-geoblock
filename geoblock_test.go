package maxmind_geoblock_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	geoblock "github.com/gabay/maxmind-geoblock"
)

const testDB = "testdata/GeoIP2-Country-Test.mmdb"

// IPs from the MaxMind test database.
const (
	ipGB       = "81.2.69.142"     // GB / EU
	ipUS       = "216.160.83.56"   // US / NA
	ipSE       = "89.160.20.112"   // SE / EU
	ipBT       = "67.43.156.1"     // BT / AS
	ipJP       = "2001:218::1"     // JP / AS
	ipEUOnly   = "2a02:d500::1"    // continent EU, no country
	ipNoGeo    = "214.1.1.1"       // record without country/continent
	ipLoopback = "127.0.0.1"       // Loopback IP
	ipLocal    = "172.17.1.2"      // Local IP
	ipUnknown  = "100.64.1.2"      // not in the database
	ipAnyEU    = "2a02:d500::ffff" // continent EU, no country
)

func newConfig() *geoblock.Config {
	cfg := geoblock.CreateConfig()
	cfg.DBPath = testDB
	cfg.DBReloadInterval = "0"

	return cfg
}

type response struct {
	code int
	body string
}

// serve builds the middleware from cfg and sends one request with the given
// remote address and headers through it.
func serve(t *testing.T, cfg *geoblock.Config, remoteAddr string, headers map[string]string) response {
	t.Helper()

	handler := newHandler(t, cfg)

	return send(handler, remoteAddr, headers)
}

func newHandler(t *testing.T, cfg *geoblock.Config) http.Handler {
	t.Helper()

	next := http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		rw.WriteHeader(http.StatusOK)
		_, _ = rw.Write([]byte("next"))
	})

	handler, err := geoblock.New(context.Background(), next, cfg, "test")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	return handler
}

func send(handler http.Handler, remoteAddr string, headers map[string]string) response {
	req := httptest.NewRequest(http.MethodGet, "http://example.com/path?q=1", nil)
	req.RemoteAddr = remoteAddr

	for k, v := range headers {
		req.Header.Set(k, v)
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	return response{code: rec.Code, body: rec.Body.String()}
}

func withPort(ip string) string {
	if strings.Contains(ip, ":") {
		return "[" + ip + "]:12345"
	}

	return ip + ":12345"
}

func TestDecisions(t *testing.T) {
	tests := []struct {
		name  string
		setup func(cfg *geoblock.Config)
		ip    string
		want  int
	}{
		// allowedCountries
		{name: "allowedCountries match", setup: func(c *geoblock.Config) { c.AllowedCountries = []string{"GB", "US"} }, ip: ipGB, want: 200},
		{name: "allowedCountries lower-case match", setup: func(c *geoblock.Config) { c.AllowedCountries = []string{" gb "} }, ip: ipGB, want: 200},
		{name: "allowedCountries no match", setup: func(c *geoblock.Config) { c.AllowedCountries = []string{"GB"} }, ip: ipUS, want: 403},
		{name: "allowedCountries ipv6 match", setup: func(c *geoblock.Config) { c.AllowedCountries = []string{"JP"} }, ip: ipJP, want: 200},
		{name: "allowedCountries continent-only record", setup: func(c *geoblock.Config) { c.AllowedCountries = []string{"GB"} }, ip: ipEUOnly, want: 403},
		{name: "allowedCountries loopback ip", setup: func(c *geoblock.Config) { c.AllowedCountries = []string{"GB"} }, ip: ipLoopback, want: 403},
		{name: "allowedCountries loopback ip allowLoopback", setup: func(c *geoblock.Config) { c.AllowedCountries = []string{"GB"}; c.AllowLoopback = true }, ip: ipLoopback, want: 200},
		{name: "allowedCountries local ip", setup: func(c *geoblock.Config) { c.AllowedCountries = []string{"GB"} }, ip: ipLocal, want: 403},
		{name: "allowedCountries local ip allowLocal", setup: func(c *geoblock.Config) { c.AllowedCountries = []string{"GB"}; c.AllowLocal = true }, ip: ipLocal, want: 200},
		{name: "allowedCountries unknown ip", setup: func(c *geoblock.Config) { c.AllowedCountries = []string{"GB"} }, ip: ipUnknown, want: 403},
		{name: "allowedCountries unknown ip allowUnknown", setup: func(c *geoblock.Config) { c.AllowedCountries = []string{"GB"}; c.AllowUnknown = true }, ip: ipUnknown, want: 200},
		{name: "allowedCountries record without geo", setup: func(c *geoblock.Config) { c.AllowedCountries = []string{"GB"} }, ip: ipNoGeo, want: 403},
		{name: "allowedCountries record without geo allowUnknown", setup: func(c *geoblock.Config) { c.AllowedCountries = []string{"GB"}; c.AllowUnknown = true }, ip: ipNoGeo, want: 200},

		// deniedCountries
		{name: "deniedCountries match", setup: func(c *geoblock.Config) { c.DeniedCountries = []string{"GB"} }, ip: ipGB, want: 403},
		{name: "deniedCountries no match", setup: func(c *geoblock.Config) { c.DeniedCountries = []string{"GB"} }, ip: ipUS, want: 200},
		{name: "deniedCountries continent-only record", setup: func(c *geoblock.Config) { c.DeniedCountries = []string{"GB"} }, ip: ipEUOnly, want: 200},
		{name: "deniedCountries loopback ip", setup: func(c *geoblock.Config) { c.DeniedCountries = []string{"GB"} }, ip: ipLoopback, want: 403},
		{name: "deniedCountries loopback ip allowLoopback", setup: func(c *geoblock.Config) { c.DeniedCountries = []string{"GB"}; c.AllowLoopback = true }, ip: ipLoopback, want: 200},
		{name: "deniedCountries local ip", setup: func(c *geoblock.Config) { c.DeniedCountries = []string{"GB"} }, ip: ipLocal, want: 403},
		{name: "deniedCountries local ip allowLocal", setup: func(c *geoblock.Config) { c.DeniedCountries = []string{"GB"}; c.AllowLocal = true }, ip: ipLocal, want: 200},
		{name: "deniedCountries unknown ip", setup: func(c *geoblock.Config) { c.DeniedCountries = []string{"GB"} }, ip: ipUnknown, want: 403},
		{name: "deniedCountries unknown ip allowUnknown", setup: func(c *geoblock.Config) { c.DeniedCountries = []string{"GB"}; c.AllowUnknown = true }, ip: ipUnknown, want: 200},

		// allowedContinents
		{name: "allowedContinents match", setup: func(c *geoblock.Config) { c.AllowedContinents = []string{"EU"} }, ip: ipGB, want: 200},
		{name: "allowedContinents no match", setup: func(c *geoblock.Config) { c.AllowedContinents = []string{"EU"} }, ip: ipUS, want: 403},
		{name: "allowedContinents continent-only record", setup: func(c *geoblock.Config) { c.AllowedContinents = []string{"EU"} }, ip: ipEUOnly, want: 200},
		{name: "allowedContinents loopback ip", setup: func(c *geoblock.Config) { c.AllowedContinents = []string{"EU"} }, ip: ipLoopback, want: 403},
		{name: "allowedContinents local ip", setup: func(c *geoblock.Config) { c.AllowedContinents = []string{"EU"} }, ip: ipLocal, want: 403},
		{name: "allowedContinents unknown ip", setup: func(c *geoblock.Config) { c.AllowedContinents = []string{"EU"} }, ip: ipUnknown, want: 403},

		// deniedContinents
		{name: "deniedContinents match", setup: func(c *geoblock.Config) { c.DeniedContinents = []string{"AS"} }, ip: ipJP, want: 403},
		{name: "deniedContinents no match", setup: func(c *geoblock.Config) { c.DeniedContinents = []string{"AS"} }, ip: ipGB, want: 200},

		// combinations: either filter denying is enough
		{name: "allow EU deny FR: GB", setup: func(c *geoblock.Config) { c.AllowedContinents = []string{"EU"}; c.DeniedCountries = []string{"FR"} }, ip: ipGB, want: 200},
		{name: "allow EU deny GB: GB", setup: func(c *geoblock.Config) { c.AllowedContinents = []string{"EU"}; c.DeniedCountries = []string{"GB"} }, ip: ipGB, want: 403},
		{name: "allow EU deny GB: US", setup: func(c *geoblock.Config) { c.AllowedContinents = []string{"EU"}; c.DeniedCountries = []string{"GB"} }, ip: ipUS, want: 403},
		{name: "allow EU deny GB: SE", setup: func(c *geoblock.Config) { c.AllowedContinents = []string{"EU"}; c.DeniedCountries = []string{"GB"} }, ip: ipSE, want: 200},
		{name: "deny AS deny GB: BT", setup: func(c *geoblock.Config) { c.DeniedContinents = []string{"AS"}; c.DeniedCountries = []string{"GB"} }, ip: ipBT, want: 403},
		{name: "deny AS deny GB: GB", setup: func(c *geoblock.Config) { c.DeniedContinents = []string{"AS"}; c.DeniedCountries = []string{"GB"} }, ip: ipGB, want: 403},
		{name: "deny AS deny GB: US", setup: func(c *geoblock.Config) { c.DeniedContinents = []string{"AS"}; c.DeniedCountries = []string{"GB"} }, ip: ipUS, want: 200},
		{name: "deny AS deny GB: EU-only record", setup: func(c *geoblock.Config) { c.DeniedContinents = []string{"AS"}; c.DeniedCountries = []string{"GB"} }, ip: ipAnyEU, want: 200},

		// allowedIPs bypass everything
		{name: "allowedIPs exact", setup: func(c *geoblock.Config) { c.AllowedCountries = []string{"US"}; c.AllowedIPs = []string{ipGB} }, ip: ipGB, want: 200},
		{name: "allowedIPs subnet", setup: func(c *geoblock.Config) { c.AllowedCountries = []string{"US"}; c.AllowedIPs = []string{"81.2.69.0/24"} }, ip: ipGB, want: 200},
		{name: "allowedIPs ipv6 subnet", setup: func(c *geoblock.Config) {
			c.AllowedCountries = []string{"US"}
			c.AllowedIPs = []string{"2001:218::/32"}
		}, ip: ipJP, want: 200},
		{name: "allowedIPs loopback ip", setup: func(c *geoblock.Config) { c.AllowedCountries = []string{"US"}; c.AllowedIPs = []string{"127.0.0.0/8"} }, ip: ipLoopback, want: 200},
		{name: "allowedIPs local ip", setup: func(c *geoblock.Config) { c.AllowedCountries = []string{"US"}; c.AllowedIPs = []string{"172.16.0.0/12"} }, ip: ipLocal, want: 200},
		{name: "allowedIPs unknown ip", setup: func(c *geoblock.Config) { c.AllowedCountries = []string{"US"}; c.AllowedIPs = []string{"100.64.0.0/10"} }, ip: ipUnknown, want: 200},
		{name: "allowedIPs no match", setup: func(c *geoblock.Config) {
			c.AllowedCountries = []string{"US"}
			c.AllowedIPs = []string{"100.64.0.0/10", "127.0.0.1"}
		}, ip: ipGB, want: 403},

		// status code
		{name: "custom status code", setup: func(c *geoblock.Config) { c.AllowedCountries = []string{"US"}; c.BlockStatusCode = 451 }, ip: ipGB, want: 451},
		{name: "status code 204", setup: func(c *geoblock.Config) { c.AllowedCountries = []string{"US"}; c.BlockStatusCode = 204 }, ip: ipGB, want: 204},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := newConfig()
			tc.setup(cfg)

			resp := serve(t, cfg, withPort(tc.ip), nil)
			if resp.code != tc.want {
				t.Fatalf("status = %d, want %d", resp.code, tc.want)
			}

			switch {
			case tc.want == 200 && resp.body != "next":
				t.Errorf("body = %q, want next handler output", resp.body)
			case tc.want >= 400 && resp.body != http.StatusText(tc.want):
				t.Errorf("body = %q, want %q", resp.body, http.StatusText(tc.want))
			case tc.want == 204 && resp.body != "":
				t.Errorf("body = %q, want empty", resp.body)
			}
		})
	}
}

func TestRemoteAddrFormats(t *testing.T) {
	cfg := newConfig()
	cfg.AllowedCountries = []string{"GB", "JP"}

	tests := []struct {
		remoteAddr string
		want       int
	}{
		{remoteAddr: ipGB + ":443", want: 200},
		{remoteAddr: ipGB, want: 200},
		{remoteAddr: "[" + ipJP + "]:443", want: 200},
		{remoteAddr: ipJP, want: 200},
		{remoteAddr: "[::ffff:" + ipGB + "]:443", want: 200}, // IPv4-mapped
		{remoteAddr: ipUS + ":443", want: 403},
		{remoteAddr: "garbage", want: 403},
		{remoteAddr: "", want: 403},
	}

	for _, tc := range tests {
		t.Run(tc.remoteAddr, func(t *testing.T) {
			if resp := serve(t, cfg, tc.remoteAddr, nil); resp.code != tc.want {
				t.Errorf("status = %d, want %d", resp.code, tc.want)
			}
		})
	}

	// An undeterminable client IP counts as unknown.
	cfg.AllowUnknown = true

	if resp := serve(t, cfg, "garbage", nil); resp.code != 200 {
		t.Errorf("status = %d, want 200 with allowUnknown", resp.code)
	}
}

func TestTrustedForwarders(t *testing.T) {
	tests := []struct {
		name       string
		trusted    []string
		remoteAddr string
		xff        []string
		want       int
	}{
		{name: "no forwarders: header ignored", remoteAddr: ipUS, xff: []string{ipGB}, want: 403},
		{name: "untrusted peer: header ignored", trusted: []string{"10.0.0.0/8"}, remoteAddr: ipUS, xff: []string{ipGB}, want: 403},
		{name: "trusted peer: header used", trusted: []string{"10.0.0.0/8"}, remoteAddr: "10.0.0.1", xff: []string{ipGB}, want: 200},
		{name: "trusted peer exact ip: header used", trusted: []string{"10.0.0.1"}, remoteAddr: "10.0.0.1", xff: []string{ipGB}, want: 200},
		{name: "trusted peer: header blocked country", trusted: []string{"10.0.0.0/8"}, remoteAddr: "10.0.0.1", xff: []string{ipUS}, want: 403},
		{name: "trusted peer without header: peer used", trusted: []string{"10.0.0.0/8"}, remoteAddr: "10.0.0.1", want: 403},
		{name: "chain: rightmost untrusted wins", trusted: []string{"10.0.0.0/8"}, remoteAddr: "10.0.0.1", xff: []string{ipGB + ", " + ipUS + ", 10.0.0.2"}, want: 403},
		{name: "chain: spoofed entries ignored", trusted: []string{"10.0.0.0/8"}, remoteAddr: "10.0.0.1", xff: []string{ipUS + ", " + ipGB + ", 10.0.0.2"}, want: 200},
		{name: "chain: all trusted uses leftmost", trusted: []string{"10.0.0.0/8"}, remoteAddr: "10.0.0.1", xff: []string{"10.0.0.3, 10.0.0.2"}, want: 403},
		{name: "multiple headers", trusted: []string{"10.0.0.0/8"}, remoteAddr: "10.0.0.1", xff: []string{ipUS, ipGB}, want: 200},
		{name: "ipv6 forwarder", trusted: []string{"fd00::/8"}, remoteAddr: "[fd00::1]:1", xff: []string{ipGB}, want: 200},
		{name: "ipv6 client", trusted: []string{"10.0.0.0/8"}, remoteAddr: "10.0.0.1", xff: []string{ipJP}, want: 403},
		{name: "ipv4-mapped client", trusted: []string{"10.0.0.0/8"}, remoteAddr: "10.0.0.1", xff: []string{"::ffff:" + ipGB}, want: 200},
		{name: "garbage header from trusted peer: unknown", trusted: []string{"10.0.0.0/8"}, remoteAddr: "10.0.0.1", xff: []string{"not-an-ip"}, want: 403},
		{name: "empty header from trusted peer: peer used", trusted: []string{"10.0.0.0/8"}, remoteAddr: "10.0.0.1", xff: []string{" , "}, want: 403},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := newConfig()
			cfg.AllowedCountries = []string{"GB"}
			cfg.TrustedForwarders = tc.trusted

			handler := newHandler(t, cfg)

			req := httptest.NewRequest(http.MethodGet, "http://example.com/", nil)
			req.RemoteAddr = withPort(tc.remoteAddr)

			if strings.Contains(tc.remoteAddr, ":") || tc.remoteAddr == "" {
				req.RemoteAddr = tc.remoteAddr
			}

			for _, v := range tc.xff {
				req.Header.Add("X-Forwarded-For", v)
			}

			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d", rec.Code, tc.want)
			}
		})
	}
}

func TestConfigValidation(t *testing.T) {
	tests := []struct {
		name    string
		setup   func(cfg *geoblock.Config)
		wantErr string
	}{
		{name: "missing dbPath", setup: func(c *geoblock.Config) { c.DBPath = " "; c.AllowedCountries = []string{"US"} }, wantErr: "dbPath is required"},
		{name: "no filter", setup: func(_ *geoblock.Config) {}, wantErr: "at least one of"},
		{name: "allowed+denied countries", setup: func(c *geoblock.Config) { c.AllowedCountries = []string{"US"}; c.DeniedCountries = []string{"FR"} }, wantErr: "mutually exclusive"},
		{name: "allowed+denied continents", setup: func(c *geoblock.Config) { c.AllowedContinents = []string{"EU"}; c.DeniedContinents = []string{"AS"} }, wantErr: "mutually exclusive"},
		{name: "allowed countries + allowed continents", setup: func(c *geoblock.Config) { c.AllowedCountries = []string{"US"}; c.AllowedContinents = []string{"EU"} }, wantErr: "cannot be combined"},
		{name: "allowed countries + denied continents", setup: func(c *geoblock.Config) { c.AllowedCountries = []string{"US"}; c.DeniedContinents = []string{"EU"} }, wantErr: "cannot be combined"},
		{name: "bad country code", setup: func(c *geoblock.Config) { c.AllowedCountries = []string{"USA"} }, wantErr: "invalid country code"},
		{name: "empty country code", setup: func(c *geoblock.Config) { c.AllowedCountries = []string{""} }, wantErr: "empty code"},
		{name: "bad continent code", setup: func(c *geoblock.Config) { c.DeniedContinents = []string{"XX"} }, wantErr: "invalid continent code"},
		{name: "bad allowed ip", setup: func(c *geoblock.Config) { c.DeniedCountries = []string{"US"}; c.AllowedIPs = []string{"1.2.3.4/33"} }, wantErr: "allowedIPs"},
		{name: "bad trusted forwarder", setup: func(c *geoblock.Config) { c.DeniedCountries = []string{"US"}; c.TrustedForwarders = []string{"nope"} }, wantErr: "trustedForwarders"},
		{name: "bad status code", setup: func(c *geoblock.Config) { c.DeniedCountries = []string{"US"}; c.BlockStatusCode = 42 }, wantErr: "blockStatusCode"},
		{name: "bad reload interval", setup: func(c *geoblock.Config) { c.DeniedCountries = []string{"US"}; c.DBReloadInterval = "soon" }, wantErr: "dbReloadInterval"},
		{name: "negative reload interval", setup: func(c *geoblock.Config) { c.DeniedCountries = []string{"US"}; c.DBReloadInterval = "-1s" }, wantErr: "dbReloadInterval"},
		{name: "negative cache size", setup: func(c *geoblock.Config) { c.DeniedCountries = []string{"US"}; c.CacheSize = -1 }, wantErr: "cacheSize"},

		// Valid combinations.
		{name: "denied countries + allowed continents", setup: func(c *geoblock.Config) { c.DeniedCountries = []string{"FR"}; c.AllowedContinents = []string{"EU"} }},
		{name: "denied countries + denied continents", setup: func(c *geoblock.Config) { c.DeniedCountries = []string{"FR"}; c.DeniedContinents = []string{"AS"} }},
		{name: "continents only", setup: func(c *geoblock.Config) { c.AllowedContinents = []string{"EU", "NA"} }},
		{name: "defaults", setup: func(c *geoblock.Config) {
			c.AllowedCountries = []string{"US"}
			c.BlockStatusCode = 0
			c.DBReloadInterval = ""
		}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := newConfig()
			tc.setup(cfg)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			_, err := geoblock.New(ctx, http.NotFoundHandler(), cfg, "test")

			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("New: unexpected error %v", err)
			case tc.wantErr != "" && err == nil:
				t.Fatalf("New: expected error containing %q", tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Fatalf("New: error %q does not contain %q", err, tc.wantErr)
			}
		})
	}
}

func TestCache(t *testing.T) {
	for _, size := range []int{0, 1, 2, 10000} {
		cfg := newConfig()
		cfg.AllowedCountries = []string{"GB", "JP"}
		cfg.CacheSize = size

		handler := newHandler(t, cfg)

		// Cycle through more distinct IPs than the cache holds, twice, so
		// that hits, misses and flushes all happen.
		for round := 0; round < 2; round++ {
			for _, tc := range []struct {
				ip   string
				want int
			}{
				{ip: ipGB, want: 200},
				{ip: ipUS, want: 403},
				{ip: ipJP, want: 200},
				{ip: ipLoopback, want: 403},
				{ip: ipLocal, want: 403},
				{ip: ipUnknown, want: 403},
				{ip: ipGB, want: 200},
			} {
				if resp := send(handler, withPort(tc.ip), nil); resp.code != tc.want {
					t.Errorf("cacheSize=%d round %d %s: status = %d, want %d", size, round, tc.ip, resp.code, tc.want)
				}
			}
		}
	}
}

func TestMissingDatabaseIsNotFatal(t *testing.T) {
	cfg := newConfig()
	cfg.DBPath = filepath.Join(t.TempDir(), "missing.mmdb")
	cfg.AllowedCountries = []string{"GB"}

	if resp := serve(t, cfg, withPort(ipGB), nil); resp.code != 403 {
		t.Errorf("status = %d, want 403 without a database", resp.code)
	}

	cfg.AllowUnknown = true

	if resp := serve(t, cfg, withPort(ipGB), nil); resp.code != 200 {
		t.Errorf("status = %d, want 200 without a database and allowUnknown", resp.code)
	}

	cfg.AllowUnknown = false
	cfg.AllowedIPs = []string{ipGB}

	if resp := serve(t, cfg, withPort(ipGB), nil); resp.code != 200 {
		t.Errorf("status = %d, want 200 for allowedIPs without a database", resp.code)
	}
}

func copyFile(t *testing.T, src, dst string) {
	t.Helper()

	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}

	// Write to a temporary file and rename, like a well-behaved updater.
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := os.Rename(tmp, dst); err != nil {
		t.Fatal(err)
	}
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) bool {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}

		time.Sleep(20 * time.Millisecond)
	}

	return cond()
}

func TestDatabaseReload(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "country.mmdb")

	cfg := newConfig()
	cfg.DBPath = dbPath
	cfg.DBReloadInterval = "50ms"
	cfg.AllowedCountries = []string{"GB"}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	next := http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) { rw.WriteHeader(http.StatusOK) })

	handler, err := geoblock.New(ctx, next, cfg, "reload")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// 1. No database yet: everything unknown.
	if resp := send(handler, withPort(ipGB), nil); resp.code != 403 {
		t.Fatalf("status = %d, want 403 before the database exists", resp.code)
	}

	// 2. The database appears: it gets picked up.
	copyFile(t, testDB, dbPath)

	if !waitFor(t, 5*time.Second, func() bool { return send(handler, withPort(ipGB), nil).code == 200 }) {
		t.Fatal("database was not loaded after it appeared")
	}

	// 3. The database is replaced by one with different content: the test
	//    "mixed" database has no country data, so GB becomes unknown.
	//    Make sure the modification time differs even on coarse filesystems.
	time.Sleep(20 * time.Millisecond)
	copyFile(t, "testdata/MaxMind-DB-test-mixed-24.mmdb", dbPath)

	if !waitFor(t, 5*time.Second, func() bool { return send(handler, withPort(ipGB), nil).code == 403 }) {
		t.Fatal("database was not reloaded after it changed")
	}

	// 4. The database is replaced by garbage: the previous one is kept.
	time.Sleep(20 * time.Millisecond)

	if err := os.WriteFile(dbPath, []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}

	time.Sleep(200 * time.Millisecond)

	if resp := send(handler, withPort(ipGB), nil); resp.code != 403 {
		t.Fatalf("status = %d, want 403 (previous database kept)", resp.code)
	}

	// 5. A valid database comes back.
	copyFile(t, testDB, dbPath)

	if !waitFor(t, 5*time.Second, func() bool { return send(handler, withPort(ipGB), nil).code == 200 }) {
		t.Fatal("database was not reloaded after it was fixed")
	}

	// 6. Removing the database keeps the last good one.
	if err := os.Remove(dbPath); err != nil {
		t.Fatal(err)
	}

	time.Sleep(200 * time.Millisecond)

	if resp := send(handler, withPort(ipGB), nil); resp.code != 200 {
		t.Fatalf("status = %d, want 200 (last database kept after removal)", resp.code)
	}
}

func TestDebugLogging(t *testing.T) {
	// Capture stdout, which is where Yaegi plugins log.
	orig := os.Stdout

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}

	os.Stdout = w

	defer func() { os.Stdout = orig }()

	cfg := newConfig()
	cfg.Debug = true
	cfg.AllowedCountries = []string{"GB"}
	cfg.AllowedIPs = []string{"1.2.3.4"}
	cfg.AllowLocal = true


	handler := newHandler(t, cfg)
	send(handler, withPort(ipGB), nil)
	send(handler, withPort(ipUS), nil)
	send(handler, withPort(ipLocal), nil)
	send(handler, withPort(ipUnknown), nil)
	send(handler, "1.2.3.4:1", nil)

	_ = w.Close()

	out := make([]byte, 64*1024)
	n, _ := r.Read(out)
	logs := string(out[:n])

	for _, want := range []string{
		"maxmind-geoblock[test]: configuration: dbPath=" + testDB + " allowedCountries=GB allowedIPs=[1.2.3.4/32] trustedForwarders=[] allowLoopback=false allowLocal=true allowUnknown=false blockStatusCode=403 dbReloadInterval=0s cacheSize=10000",
		"loaded database " + testDB + ": type=GeoIP2-Country",
		ipGB + " country=GB continent=EU allowed: no filter matched GET example.com/path?q=1",
		ipUS + " country=US continent=NA blocked: country not in allowedCountries",
		ipLocal + " allowed: ip is local",
		ipUnknown + " country=- continent=- blocked: unknown location",
		"1.2.3.4 allowed: ip in allowedIPs",
	} {
		if !strings.Contains(logs, want) {
			t.Errorf("debug log does not contain %q:\n%s", want, logs)
		}
	}
}

func TestNoDebugLogging(t *testing.T) {
	orig := os.Stdout

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}

	os.Stdout = w

	defer func() { os.Stdout = orig }()

	cfg := newConfig()
	cfg.AllowedCountries = []string{"GB"}

	handler := newHandler(t, cfg)
	send(handler, withPort(ipGB), nil)
	send(handler, withPort(ipUS), nil)

	_ = w.Close()

	out := make([]byte, 1024)
	if n, _ := r.Read(out); n != 0 {
		t.Errorf("unexpected output without debug: %q", out[:n])
	}
}
