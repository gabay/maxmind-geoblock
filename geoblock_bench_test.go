package maxmind_geoblock_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	geoblock "github.com/gabay/maxmind-geoblock"
)

// benchResponseWriter is a minimal ResponseWriter so that the benchmarks
// measure the middleware rather than httptest.ResponseRecorder.
type benchResponseWriter struct {
	header http.Header
	code   int
}

func (w *benchResponseWriter) Header() http.Header         { return w.header }
func (w *benchResponseWriter) Write(b []byte) (int, error) { return len(b), nil }
func (w *benchResponseWriter) WriteHeader(code int)        { w.code = code }

func newBenchHandler(b *testing.B, setup func(cfg *geoblock.Config)) http.Handler {
	b.Helper()

	cfg := geoblock.CreateConfig()
	cfg.DBPath = testDB
	cfg.DBReloadInterval = "0"
	setup(cfg)

	next := http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) { rw.WriteHeader(http.StatusOK) })

	handler, err := geoblock.New(context.Background(), next, cfg, "bench")
	if err != nil {
		b.Fatal(err)
	}

	return handler
}

func runBench(b *testing.B, handler http.Handler, remoteAddr string, headers map[string]string, want int) {
	b.Helper()

	req := httptest.NewRequest(http.MethodGet, "http://example.com/", nil)
	req.RemoteAddr = remoteAddr

	for k, v := range headers {
		req.Header.Set(k, v)
	}

	rw := &benchResponseWriter{header: http.Header{}}

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		rw.code = 0
		handler.ServeHTTP(rw, req)

		if rw.code != want {
			b.Fatalf("status = %d, want %d", rw.code, want)
		}
	}
}

func BenchmarkAllowedCountry(b *testing.B) {
	h := newBenchHandler(b, func(c *geoblock.Config) { c.AllowedCountries = []string{"GB", "US", "DE"} })
	runBench(b, h, ipGB+":1234", nil, 200)
}

func BenchmarkAllowedCountryNoCache(b *testing.B) {
	h := newBenchHandler(b, func(c *geoblock.Config) { c.AllowedCountries = []string{"GB", "US", "DE"}; c.CacheSize = 0 })
	runBench(b, h, ipGB+":1234", nil, 200)
}

func BenchmarkBlockedCountry(b *testing.B) {
	h := newBenchHandler(b, func(c *geoblock.Config) { c.AllowedCountries = []string{"US"} })
	runBench(b, h, ipGB+":1234", nil, 403)
}

func BenchmarkDeniedCountryAndContinent(b *testing.B) {
	h := newBenchHandler(b, func(c *geoblock.Config) {
		c.DeniedCountries = []string{"FR", "RU"}
		c.AllowedContinents = []string{"EU", "NA"}
	})
	runBench(b, h, ipGB+":1234", nil, 200)
}

func BenchmarkIPv6(b *testing.B) {
	h := newBenchHandler(b, func(c *geoblock.Config) { c.AllowedCountries = []string{"JP"} })
	runBench(b, h, "["+ipJP+"]:1234", nil, 200)
}

func BenchmarkIPv6NoCache(b *testing.B) {
	h := newBenchHandler(b, func(c *geoblock.Config) { c.AllowedCountries = []string{"JP"}; c.CacheSize = 0 })
	runBench(b, h, "["+ipJP+"]:1234", nil, 200)
}

func BenchmarkUnknownIP(b *testing.B) {
	h := newBenchHandler(b, func(c *geoblock.Config) { c.AllowedCountries = []string{"GB"} })
	runBench(b, h, ipUnknown+":1234", nil, 403)
}

func BenchmarkAllowedIP(b *testing.B) {
	h := newBenchHandler(b, func(c *geoblock.Config) {
		c.AllowedCountries = []string{"US"}
		c.AllowedIPs = []string{"10.0.0.0/8", "192.168.0.0/16", ipGB}
	})
	runBench(b, h, ipGB+":1234", nil, 200)
}

func BenchmarkTrustedForwarder(b *testing.B) {
	h := newBenchHandler(b, func(c *geoblock.Config) {
		c.AllowedCountries = []string{"GB"}
		c.TrustedForwarders = []string{"10.0.0.0/8"}
	})
	runBench(b, h, "10.0.0.1:1234", map[string]string{"X-Forwarded-For": ipGB + ", 10.0.0.2"}, 200)
}
