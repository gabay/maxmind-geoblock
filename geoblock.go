// Package maxmind_geoblock is a Traefik middleware plugin that allows or blocks
// requests based on the geographic location of the client IP address, as
// reported by a MaxMind GeoIP2/GeoLite2 Country database.
package maxmind_geoblock //nolint:revive,stylecheck // Traefik derives the package name from the module path.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gabay/maxmind-geoblock/mmdb"
)

const (
	defaultBlockStatusCode  = http.StatusForbidden
	defaultDBReloadInterval = "1m"
	defaultCacheSize        = 10000
)

// continentCodes are the two-letter continent codes used by MaxMind.
var continentCodes = map[string]bool{
	"AF": true, // Africa
	"AN": true, // Antarctica
	"AS": true, // Asia
	"EU": true, // Europe
	"NA": true, // North America
	"OC": true, // Oceania
	"SA": true, // South America
}

// Config is the plugin configuration.
type Config struct {
	// DBPath is the path to a MaxMind GeoIP2/GeoLite2 Country database
	// (.mmdb). City databases work as well. Required.
	DBPath string `json:"dbPath,omitempty"`

	// AllowedCountries only lets requests from these countries through
	// (two-letter ISO 3166-1 codes, e.g. "US"). Cannot be combined with any
	// other country or continent list.
	AllowedCountries []string `json:"allowedCountries,omitempty"`
	// DeniedCountries blocks requests from these countries. Can be combined
	// with AllowedContinents or DeniedContinents.
	DeniedCountries []string `json:"deniedCountries,omitempty"`

	// AllowedContinents only lets requests from these continents through
	// (AF, AN, AS, EU, NA, OC, SA). Mutually exclusive with DeniedContinents.
	AllowedContinents []string `json:"allowedContinents,omitempty"`
	// DeniedContinents blocks requests from these continents.
	DeniedContinents []string `json:"deniedContinents,omitempty"`

	// AllowedIPs lists IP addresses or CIDR subnets that bypass the geo
	// checks entirely (e.g. "127.0.0.1", "10.0.0.0/8").
	AllowedIPs []string `json:"allowedIPs,omitempty"`

	// AllowUnknown lets requests through when the client IP has no location
	// data (the lookup failed, the IP is not in the database or the record
	// has neither a country nor a continent). Defaults to false: such
	// requests are blocked.
	AllowUnknown bool `json:"allowUnknown,omitempty"`

	// BlockStatusCode is the HTTP status code sent for blocked requests.
	// Defaults to 403.
	BlockStatusCode int `json:"blockStatusCode,omitempty"`

	// TrustedForwarders lists IP addresses or CIDR subnets of proxies that
	// are trusted to set X-Forwarded-For. When the connecting peer is
	// trusted, the client IP is taken from X-Forwarded-For (the right-most
	// address that is not itself a trusted forwarder); otherwise the peer
	// address is used. Defaults to empty: X-Forwarded-For is never trusted.
	TrustedForwarders []string `json:"trustedForwarders,omitempty"`

	// Debug enables logging of the effective configuration, database
	// (re)loads and of every request decision. Defaults to false.
	Debug bool `json:"debug,omitempty"`

	// DBReloadInterval is how often the database file is checked for changes
	// (size or modification time) and reloaded, as a Go duration
	// (e.g. "30s", "5m"). "0" disables reloading. Defaults to "1m".
	DBReloadInterval string `json:"dbReloadInterval,omitempty"`

	// CacheSize is the maximum number of client IP addresses whose lookup
	// result is cached (the cache is flushed when full and whenever the
	// database is reloaded). 0 disables the cache. Defaults to 10000.
	CacheSize int `json:"cacheSize,omitempty"`
}

// CreateConfig creates the default plugin configuration.
func CreateConfig() *Config {
	return &Config{
		BlockStatusCode:  defaultBlockStatusCode,
		DBReloadInterval: defaultDBReloadInterval,
		CacheSize:        defaultCacheSize,
	}
}

// GeoBlock is the geoblocking middleware.
type GeoBlock struct {
	next http.Handler
	name string

	allowedCountries  map[string]bool // nil when not configured
	deniedCountries   map[string]bool
	allowedContinents map[string]bool // nil when not configured
	deniedContinents  map[string]bool
	allowedIPs        []netip.Prefix
	trustedForwarders []netip.Prefix
	allowUnknown      bool
	blockStatusCode   int
	blockBody         []byte
	debug             bool
	cacheSize         int

	dbPath         string
	reloadInterval time.Duration
	db             atomic.Value // *dbState
}

// dbState is the currently loaded database together with the file attributes
// it was loaded from (used to detect changes) and a cache of lookup results
// that lives exactly as long as this database.
type dbState struct {
	reader  *mmdb.Reader // nil when no database could be loaded
	modTime time.Time
	size    int64

	mu    sync.RWMutex
	cache map[netip.Addr]mmdb.Country
}

// New creates a new GeoBlock middleware.
func New(ctx context.Context, next http.Handler, config *Config, name string) (http.Handler, error) {
	if config == nil {
		config = CreateConfig()
	}

	g := &GeoBlock{
		next:            next,
		name:            name,
		allowUnknown:    config.AllowUnknown,
		blockStatusCode: config.BlockStatusCode,
		debug:           config.Debug,
		dbPath:          strings.TrimSpace(config.DBPath),
	}

	if g.dbPath == "" {
		return nil, errors.New("dbPath is required")
	}

	var err error

	if g.allowedCountries, err = parseCountries(config.AllowedCountries); err != nil {
		return nil, fmt.Errorf("allowedCountries: %w", err)
	}

	if g.deniedCountries, err = parseCountries(config.DeniedCountries); err != nil {
		return nil, fmt.Errorf("deniedCountries: %w", err)
	}

	if g.allowedContinents, err = parseContinents(config.AllowedContinents); err != nil {
		return nil, fmt.Errorf("allowedContinents: %w", err)
	}

	if g.deniedContinents, err = parseContinents(config.DeniedContinents); err != nil {
		return nil, fmt.Errorf("deniedContinents: %w", err)
	}

	switch {
	case g.allowedCountries != nil && g.deniedCountries != nil:
		return nil, errors.New("allowedCountries and deniedCountries are mutually exclusive")
	case g.allowedContinents != nil && g.deniedContinents != nil:
		return nil, errors.New("allowedContinents and deniedContinents are mutually exclusive")
	case g.allowedCountries != nil && (g.allowedContinents != nil || g.deniedContinents != nil):
		return nil, errors.New("allowedCountries cannot be combined with allowedContinents or deniedContinents")
	case g.allowedCountries == nil && g.deniedCountries == nil && g.allowedContinents == nil && g.deniedContinents == nil:
		return nil, errors.New("at least one of allowedCountries, deniedCountries, allowedContinents or deniedContinents must be configured")
	}

	if g.allowedIPs, err = parsePrefixes(config.AllowedIPs); err != nil {
		return nil, fmt.Errorf("allowedIPs: %w", err)
	}

	if g.trustedForwarders, err = parsePrefixes(config.TrustedForwarders); err != nil {
		return nil, fmt.Errorf("trustedForwarders: %w", err)
	}

	if g.blockStatusCode == 0 {
		g.blockStatusCode = defaultBlockStatusCode
	}

	if g.blockStatusCode < 100 || g.blockStatusCode > 599 {
		return nil, fmt.Errorf("blockStatusCode: invalid HTTP status code %d", g.blockStatusCode)
	}

	if g.blockStatusCode >= 400 {
		g.blockBody = []byte(http.StatusText(g.blockStatusCode))
	}

	if g.reloadInterval, err = parseReloadInterval(config.DBReloadInterval); err != nil {
		return nil, fmt.Errorf("dbReloadInterval: %w", err)
	}

	if g.cacheSize = config.CacheSize; g.cacheSize < 0 {
		return nil, fmt.Errorf("cacheSize: must not be negative, got %d", g.cacheSize)
	}

	// A missing or broken database is not fatal: requests are treated as
	// having an unknown location (blocked unless allowUnknown is set) until
	// a valid database shows up at dbPath.
	g.db.Store(&dbState{})

	if err := g.loadDB(); err != nil {
		g.logError("cannot load database: " + err.Error() + "; all locations are unknown until it loads")
	}

	if g.reloadInterval > 0 {
		go g.watchDB(ctx)
	}

	if g.debug {
		g.logDebug("configuration: " + g.describe())
	}

	return g, nil
}

// ServeHTTP implements http.Handler.
//
// The hot path is deliberately flat: under Yaegi a function call costs about
// 3µs, roughly ten times more than the work the callee would do here, so the
// client IP extraction, the allowlist check and the decision are inlined.
func (g *GeoBlock) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	// net/http always sets RemoteAddr to "ip:port"; parse that directly and
	// only fall back to the general parser when it does not look like it.
	var ip netip.Addr

	if ap, err := netip.ParseAddrPort(req.RemoteAddr); err == nil {
		ip = ap.Addr()

		if ip.Is6() {
			if ip.Is4In6() {
				ip = ip.Unmap()
			} else if ip.Zone() != "" {
				ip = ip.WithZone("")
			}
		}
	} else {
		ip = parseIP(req.RemoteAddr)
	}

	if len(g.trustedForwarders) > 0 {
		ip = g.forwardedIP(req, ip)
	}

	valid := ip.IsValid()

	if valid {
		for i := range g.allowedIPs {
			if g.allowedIPs[i].Contains(ip) {
				if g.debug {
					g.logDebug(ip.String() + " allowed: " + reasonAllowedIP + " " + req.Method + " " + req.Host + req.URL.RequestURI())
				}

				g.next.ServeHTTP(rw, req)

				return
			}
		}
	}

	var (
		country mmdb.Country
		err     error
	)

	state := g.db.Load().(*dbState)

	if valid && state.reader != nil {
		cached := false

		if g.cacheSize > 0 {
			state.mu.RLock()
			country, cached = state.cache[ip]
			state.mu.RUnlock()
		}

		if !cached {
			country, _, err = state.reader.LookupCountry(ip)

			if err == nil && g.cacheSize > 0 {
				state.mu.Lock()
				if len(state.cache) >= g.cacheSize {
					state.cache = make(map[netip.Addr]mmdb.Country, 64)
				}

				state.cache[ip] = country
				state.mu.Unlock()
			}
		}
	}

	// Decision. A request is unknown when the record has neither a country
	// nor a continent (or there was no record, no database or no usable IP).
	allowed := true
	reason := reasonAllowed

	switch {
	case country.ISOCode == "" && country.ContinentCode == "":
		if g.allowUnknown {
			reason = reasonUnknownAllowUnknown
		} else {
			allowed, reason = false, reasonUnknown
		}
	case g.allowedCountries != nil:
		if !g.allowedCountries[country.ISOCode] {
			allowed, reason = false, reasonCountryNotAllowed
		}
	case g.deniedCountries != nil && g.deniedCountries[country.ISOCode]:
		allowed, reason = false, reasonCountryDenied
	case g.allowedContinents != nil && !g.allowedContinents[country.ContinentCode]:
		allowed, reason = false, reasonContinentNotAllowed
	case g.deniedContinents != nil && g.deniedContinents[country.ContinentCode]:
		allowed, reason = false, reasonContinentDenied
	}

	if g.debug {
		g.logRequest(req, ip, country, allowed, reason, err, state.reader == nil)
	}

	if !allowed {
		if len(g.blockBody) > 0 {
			rw.Header().Set("Content-Type", "text/plain; charset=utf-8")
			rw.Header().Set("X-Content-Type-Options", "nosniff")
		}

		rw.WriteHeader(g.blockStatusCode)

		if len(g.blockBody) > 0 {
			_, _ = rw.Write(g.blockBody)
		}

		return
	}

	g.next.ServeHTTP(rw, req)
}

// Decision reasons, exposed through debug logs.
const (
	reasonAllowed             = "no filter matched"
	reasonUnknown             = "unknown location"
	reasonUnknownAllowUnknown = "unknown location (allowUnknown)"
	reasonCountryNotAllowed   = "country not in allowedCountries"
	reasonCountryDenied       = "country in deniedCountries"
	reasonContinentNotAllowed = "continent not in allowedContinents"
	reasonContinentDenied     = "continent in deniedContinents"
	reasonAllowedIP           = "ip in allowedIPs"
	reasonDBUnavailable       = "database not loaded"
	reasonClientIPUnknown     = "client ip could not be determined"
)

// loadDB (re)loads the database from disk.
func (g *GeoBlock) loadDB() error {
	info, err := os.Stat(g.dbPath)
	if err != nil {
		return err
	}

	reader, err := mmdb.Open(g.dbPath)
	if err != nil {
		return err
	}

	g.db.Store(&dbState{
		reader:  reader,
		modTime: info.ModTime(),
		size:    info.Size(),
		cache:   make(map[netip.Addr]mmdb.Country, 64),
	})

	if g.debug {
		m := reader.Metadata()
		g.logDebug(fmt.Sprintf("loaded database %s: type=%s built=%s nodes=%d recordSize=%d ipVersion=%d",
			g.dbPath, m.DatabaseType, time.Unix(int64(m.BuildEpoch), 0).UTC().Format(time.RFC3339),
			m.NodeCount, m.RecordSize, m.IPVersion))
	}

	return nil
}

// watchDB periodically checks the database file and reloads it when its size
// or modification time changed, until ctx is done.
func (g *GeoBlock) watchDB(ctx context.Context) {
	ticker := time.NewTicker(g.reloadInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			g.checkDB()
		}
	}
}

// checkDB reloads the database if the file changed since it was loaded.
func (g *GeoBlock) checkDB() {
	state := g.db.Load().(*dbState)

	info, err := os.Stat(g.dbPath)
	if err != nil {
		if state.reader != nil || g.debug {
			g.logError("cannot stat database: " + err.Error())
		}

		return
	}

	if state.reader != nil && info.ModTime().Equal(state.modTime) && info.Size() == state.size {
		return
	}

	if err := g.loadDB(); err != nil {
		g.logError("cannot reload database, keeping the previous one: " + err.Error())
	}
}

// describe renders the effective configuration for debug logs.
func (g *GeoBlock) describe() string {
	var b strings.Builder

	b.WriteString("dbPath=" + g.dbPath)
	writeSet(&b, " allowedCountries=", g.allowedCountries)
	writeSet(&b, " deniedCountries=", g.deniedCountries)
	writeSet(&b, " allowedContinents=", g.allowedContinents)
	writeSet(&b, " deniedContinents=", g.deniedContinents)
	b.WriteString(" allowedIPs=" + prefixesString(g.allowedIPs))
	b.WriteString(" trustedForwarders=" + prefixesString(g.trustedForwarders))
	b.WriteString(" allowUnknown=" + strconv.FormatBool(g.allowUnknown))
	b.WriteString(" blockStatusCode=" + strconv.Itoa(g.blockStatusCode))
	b.WriteString(" dbReloadInterval=" + g.reloadInterval.String())
	b.WriteString(" cacheSize=" + strconv.Itoa(g.cacheSize))

	return b.String()
}

func (g *GeoBlock) logRequest(req *http.Request, ip netip.Addr, c mmdb.Country, allowed bool, reason string, err error, noDB bool) {
	var b strings.Builder

	if ip.IsValid() {
		b.WriteString(ip.String())
	} else {
		b.WriteString("<invalid ip " + req.RemoteAddr + ">")
	}

	b.WriteString(" country=" + orDash(c.ISOCode) + " continent=" + orDash(c.ContinentCode))

	if allowed {
		b.WriteString(" allowed: ")
	} else {
		b.WriteString(" blocked: ")
	}

	switch {
	case err != nil:
		b.WriteString(reason + " (lookup error: " + err.Error() + ")")
	case !ip.IsValid():
		b.WriteString(reason + " (" + reasonClientIPUnknown + ")")
	case !c.Known() && noDB:
		b.WriteString(reason + " (" + reasonDBUnavailable + ")")
	default:
		b.WriteString(reason)
	}

	b.WriteString(" " + req.Method + " " + req.Host + req.URL.RequestURI())

	g.logDebug(b.String())
}

// Yaegi plugins can only log by writing to stdout (debug level in Traefik)
// and stderr (error level).

func (g *GeoBlock) logDebug(msg string) {
	_, _ = os.Stdout.WriteString("maxmind-geoblock[" + g.name + "]: " + msg + "\n")
}

func (g *GeoBlock) logError(msg string) {
	_, _ = os.Stderr.WriteString("maxmind-geoblock[" + g.name + "]: " + msg + "\n")
}

func parseCountries(codes []string) (map[string]bool, error) {
	set, err := parseCodes(codes)
	if err != nil {
		return nil, err
	}

	for code := range set {
		if len(code) != 2 || code[0] < 'A' || code[0] > 'Z' || code[1] < 'A' || code[1] > 'Z' {
			return nil, fmt.Errorf("invalid country code %q: expected a two-letter ISO 3166-1 code", code)
		}
	}

	return set, nil
}

func parseContinents(codes []string) (map[string]bool, error) {
	set, err := parseCodes(codes)
	if err != nil {
		return nil, err
	}

	for code := range set {
		if !continentCodes[code] {
			return nil, fmt.Errorf("invalid continent code %q: expected one of AF, AN, AS, EU, NA, OC, SA", code)
		}
	}

	return set, nil
}

// parseCodes normalizes a list of codes into an upper-case set. It returns nil
// for an empty list so that callers can tell "not configured" apart.
func parseCodes(codes []string) (map[string]bool, error) {
	set := make(map[string]bool, len(codes))

	for _, code := range codes {
		code = strings.ToUpper(strings.TrimSpace(code))
		if code == "" {
			return nil, errors.New("empty code")
		}

		set[code] = true
	}

	if len(set) == 0 {
		return nil, nil
	}

	return set, nil
}

// parsePrefixes parses IP addresses and CIDR subnets.
func parsePrefixes(values []string) ([]netip.Prefix, error) {
	prefixes := make([]netip.Prefix, 0, len(values))

	for _, v := range values {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}

		var (
			p   netip.Prefix
			err error
		)

		if strings.Contains(v, "/") {
			p, err = netip.ParsePrefix(v)
		} else {
			var ip netip.Addr

			ip, err = netip.ParseAddr(v)
			if err == nil {
				ip = ip.Unmap().WithZone("")
				p = netip.PrefixFrom(ip, ip.BitLen())
			}
		}

		if err != nil {
			return nil, fmt.Errorf("invalid IP or CIDR %q: %w", v, err)
		}

		if p.Addr().Is4In6() {
			// Store IPv4-mapped prefixes as plain IPv4 so that they match
			// unmapped client addresses.
			bits := p.Bits() - 96
			if bits < 0 {
				return nil, fmt.Errorf("invalid IPv4-mapped CIDR %q", v)
			}

			p = netip.PrefixFrom(p.Addr().Unmap(), bits)
		}

		prefixes = append(prefixes, p.Masked())
	}

	return prefixes, nil
}

func parseReloadInterval(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		s = defaultDBReloadInterval
	}

	if s == "0" {
		return 0, nil
	}

	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, err
	}

	if d < 0 {
		return 0, errors.New("must not be negative")
	}

	return d, nil
}

func writeSet(b *strings.Builder, label string, set map[string]bool) {
	if set == nil {
		return
	}

	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	b.WriteString(label + strings.Join(keys, ","))
}

func prefixesString(prefixes []netip.Prefix) string {
	if len(prefixes) == 0 {
		return "[]"
	}

	parts := make([]string, len(prefixes))
	for i, p := range prefixes {
		parts[i] = p.String()
	}

	return "[" + strings.Join(parts, ",") + "]"
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}

	return s
}
