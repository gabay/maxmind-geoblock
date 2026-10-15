// Package yaegibench benchmarks the maxmind-geoblock plugin the way Traefik
// runs it: loaded from a GOPATH tree into a Yaegi interpreter, with the
// configuration decoded by mapstructure and ServeHTTP invoked from native code.
//
// The natively compiled plugin is benchmarked with the same scenarios for
// comparison. Run with:
//
//	go test -bench . -benchmem ./...
package yaegibench

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/mitchellh/mapstructure"
	"github.com/traefik/yaegi/interp"
	"github.com/traefik/yaegi/stdlib"

	geoblock "github.com/gabay/maxmind-geoblock"
)

const (
	moduleName = "github.com/gabay/maxmind-geoblock"

	ipGB       = "81.2.69.142"
	ipJP       = "2001:218::1"
	ipLoopback = "127.0.0.1"
	ipLocal    = "172.17.1.2"
	ipUnknown  = "10.1.2.3"
)

// repoRoot is the plugin checkout (two levels up from this package).
func repoRoot(tb testing.TB) string {
	tb.Helper()

	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		tb.Fatal(err)
	}

	return root
}

func baseConfig(tb testing.TB) map[string]interface{} {
	tb.Helper()

	return map[string]interface{}{
		"dbPath":           filepath.Join(repoRoot(tb), "testdata", "GeoIP2-Country-Test.mmdb"),
		"dbReloadInterval": "0",
	}
}

type scenario struct {
	name       string
	config     map[string]interface{}
	remoteAddr string
	headers    map[string]string
	want       int
}

func scenarios(tb testing.TB) []scenario {
	tb.Helper()

	with := func(kv ...interface{}) map[string]interface{} {
		m := baseConfig(tb)
		for i := 0; i < len(kv); i += 2 {
			m[kv[i].(string)] = kv[i+1]
		}

		return m
	}

	return []scenario{
		{name: "allowed-country", config: with("allowedCountries", []string{"GB", "US", "DE"}), remoteAddr: ipGB + ":1234", want: 200},
		{name: "allowed-country-nocache", config: with("allowedCountries", []string{"GB", "US", "DE"}, "cacheSize", 0), remoteAddr: ipGB + ":1234", want: 200},
		{name: "blocked-country", config: with("allowedCountries", []string{"US"}), remoteAddr: ipGB + ":1234", want: 403},
		{name: "denied-country+allowed-continent", config: with("deniedCountries", []string{"FR", "RU"}, "allowedContinents", []string{"EU", "NA"}), remoteAddr: ipGB + ":1234", want: 200},
		{name: "ipv6", config: with("allowedCountries", []string{"JP"}), remoteAddr: "[" + ipJP + "]:1234", want: 200},
		{name: "ipv6-nocache", config: with("allowedCountries", []string{"JP"}, "cacheSize", 0), remoteAddr: "[" + ipJP + "]:1234", want: 200},
		{name: "loopback-ip", config: with(), remoteAddr: ipLoopback + ":1234", want: 403},
		{name: "loopback-ip-nocache", config: with("cacheSize", 0), remoteAddr: ipLoopback + ":1234", want: 403},
		{name: "local-ip", config: with(), remoteAddr: ipLocal + ":1234", want: 403},
		{name: "local-ip-nocache", config: with("cacheSize", 0), remoteAddr: ipLocal + ":1234", want: 403},
		{name: "unknown-ip", config: with(), remoteAddr: ipUnknown + ":1234", want: 403},
		{name: "unknown-ip-nocache", config: with("cacheSize", 0), remoteAddr: ipUnknown + ":1234", want: 403},
		{name: "allowed-ip-bypass", config: with("allowedCountries", []string{"US"}, "allowedIPs", []string{"10.0.0.0/8", ipGB}), remoteAddr: ipGB + ":1234", want: 200},
		{name: "trusted-forwarder", config: with("allowedCountries", []string{"GB"}, "trustedForwarders", []string{"10.0.0.0/8"}), remoteAddr: "10.0.0.1:1234", headers: map[string]string{"X-Forwarded-For": ipGB + ", 10.0.0.2"}, want: 200},
	}
}

var next = http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) { rw.WriteHeader(http.StatusOK) })

// newNativeHandler builds the middleware from the compiled package.
func newNativeHandler(tb testing.TB, config map[string]interface{}) http.Handler {
	tb.Helper()

	cfg := geoblock.CreateConfig()
	decodeConfig(tb, config, cfg)

	h, err := geoblock.New(context.Background(), next, cfg, "native")
	if err != nil {
		tb.Fatal(err)
	}

	return h
}

// yaegiPlugin mirrors Traefik's yaegiMiddlewareBuilder.
type yaegiPlugin struct {
	fnNew          reflect.Value
	fnCreateConfig reflect.Value
}

// loadYaegiPlugin sets up a GOPATH with the plugin sources and imports it
// into a fresh interpreter, like Traefik does at startup.
func loadYaegiPlugin(tb testing.TB) *yaegiPlugin {
	tb.Helper()

	goPath := tb.(interface{ TempDir() string }).TempDir()
	pkgDir := filepath.Join(goPath, "src", moduleName)

	if err := os.MkdirAll(filepath.Dir(pkgDir), 0o750); err != nil {
		tb.Fatal(err)
	}

	if err := os.Symlink(repoRoot(tb), pkgDir); err != nil {
		tb.Fatal(err)
	}

	i := interp.New(interp.Options{GoPath: goPath, Env: os.Environ()})
	if err := i.Use(stdlib.Symbols); err != nil {
		tb.Fatal(err)
	}

	if _, err := i.Eval(fmt.Sprintf(`import %q`, moduleName)); err != nil {
		tb.Fatalf("import plugin: %v", err)
	}

	basePkg := strings.ReplaceAll(path.Base(moduleName), "-", "_")

	fnNew, err := i.Eval(basePkg + ".New")
	if err != nil {
		tb.Fatal(err)
	}

	fnCreateConfig, err := i.Eval(basePkg + ".CreateConfig")
	if err != nil {
		tb.Fatal(err)
	}

	return &yaegiPlugin{fnNew: fnNew, fnCreateConfig: fnCreateConfig}
}

func (p *yaegiPlugin) newHandler(tb testing.TB, config map[string]interface{}) http.Handler {
	tb.Helper()

	results := p.fnCreateConfig.Call(nil)
	vConfig := results[0]
	decodeConfig(tb, config, vConfig.Interface())

	args := []reflect.Value{reflect.ValueOf(context.Background()), reflect.ValueOf(next), vConfig, reflect.ValueOf("yaegi")}
	results = p.fnNew.Call(args)

	if len(results) > 1 && results[1].Interface() != nil {
		tb.Fatalf("New: %v", results[1].Interface())
	}

	h, ok := results[0].Interface().(http.Handler)
	if !ok {
		tb.Fatalf("New returned %T", results[0].Interface())
	}

	return h
}

// decodeConfig decodes the dynamic configuration like Traefik does.
func decodeConfig(tb testing.TB, config map[string]interface{}, target interface{}) {
	tb.Helper()

	decoder, err := mapstructure.NewDecoder(&mapstructure.DecoderConfig{
		DecodeHook:       mapstructure.StringToSliceHookFunc(","),
		WeaklyTypedInput: true,
		Result:           target,
	})
	if err != nil {
		tb.Fatal(err)
	}

	if err := decoder.Decode(config); err != nil {
		tb.Fatal(err)
	}
}

type nopResponseWriter struct {
	header http.Header
	code   int
}

func (w *nopResponseWriter) Header() http.Header         { return w.header }
func (w *nopResponseWriter) Write(b []byte) (int, error) { return len(b), nil }
func (w *nopResponseWriter) WriteHeader(code int)        { w.code = code }

func newRequest(s scenario) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "http://example.com/", nil)
	req.RemoteAddr = s.remoteAddr

	for k, v := range s.headers {
		req.Header.Set(k, v)
	}

	return req
}

func runScenario(b *testing.B, h http.Handler, s scenario) {
	b.Helper()

	req := newRequest(s)
	rw := &nopResponseWriter{header: http.Header{}}

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		rw.code = 0
		h.ServeHTTP(rw, req)

		if rw.code != s.want {
			b.Fatalf("status = %d, want %d", rw.code, s.want)
		}
	}
}

func runScenarioParallel(b *testing.B, h http.Handler, s scenario) {
	b.Helper()
	b.ReportAllocs()
	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		req := newRequest(s)
		rw := &nopResponseWriter{header: http.Header{}}

		for pb.Next() {
			rw.code = 0
			h.ServeHTTP(rw, req)

			if rw.code != s.want {
				b.Errorf("status = %d, want %d", rw.code, s.want)
			}
		}
	})
}

// TestYaegiMatchesNative checks that the interpreted plugin behaves like the
// compiled one for every scenario.
func TestYaegiMatchesNative(t *testing.T) {
	plugin := loadYaegiPlugin(t)

	for _, s := range scenarios(t) {
		t.Run(s.name, func(t *testing.T) {
			for name, h := range map[string]http.Handler{
				"native": newNativeHandler(t, s.config),
				"yaegi":  plugin.newHandler(t, s.config),
			} {
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, newRequest(s))

				if rec.Code != s.want {
					t.Errorf("%s: status = %d, want %d", name, rec.Code, s.want)
				}
			}
		})
	}
}

func BenchmarkNative(b *testing.B) {
	for _, s := range scenarios(b) {
		h := newNativeHandler(b, s.config)
		b.Run(s.name, func(b *testing.B) { runScenario(b, h, s) })
	}
}

func BenchmarkYaegi(b *testing.B) {
	plugin := loadYaegiPlugin(b)

	for _, s := range scenarios(b) {
		h := plugin.newHandler(b, s.config)
		b.Run(s.name, func(b *testing.B) { runScenario(b, h, s) })
	}
}

func BenchmarkNativeParallel(b *testing.B) {
	for _, s := range scenarios(b) {
		h := newNativeHandler(b, s.config)
		b.Run(s.name, func(b *testing.B) { runScenarioParallel(b, h, s) })
	}
}

func BenchmarkYaegiParallel(b *testing.B) {
	plugin := loadYaegiPlugin(b)

	for _, s := range scenarios(b) {
		h := plugin.newHandler(b, s.config)
		b.Run(s.name, func(b *testing.B) { runScenarioParallel(b, h, s) })
	}
}
