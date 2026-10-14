# MaxMind GeoBlock

[Traefik](https://traefik.io) middleware plugin that allows or blocks requests based on the
geographic location of the client IP address, looked up in a local
[MaxMind GeoIP2 / GeoLite2 Country](https://dev.maxmind.com/geoip/geolite2-free-geolocation-data)
database (`.mmdb`).

- Country allow/deny lists (ISO 3166-1 alpha-2 codes) and continent allow/deny lists, combinable.
- IP/CIDR allowlist that bypasses the geo checks.
- Correct client IP behind trusted proxies (`X-Forwarded-For`).
- Fail-closed by default: requests without location data are blocked unless `allowUnknown` is set.
- The database file is watched and reloaded automatically when it changes (e.g. after `geoipupdate`).
- No third-party dependencies, no `unsafe`/`syscall`: a small purpose-built `.mmdb` reader that
  runs both natively and inside Traefik's Yaegi interpreter, tuned for the latter (see
  [Performance](#performance)).

## Installation

Static configuration (published plugin):

```yaml
experimental:
  plugins:
    maxmind-geoblock:
      moduleName: github.com/gabay/maxmind-geoblock
      version: v0.1.0
```

Local mode (development): place the checkout at
`./plugins-local/src/github.com/gabay/maxmind-geoblock` relative to Traefik's working directory and
declare it under `experimental.localPlugins` instead.

The `.mmdb` file must be readable by the Traefik process (mount it into the container). Keep it
fresh with [`geoipupdate`](https://github.com/maxmind/geoipupdate); the plugin picks up the new file
on its own.

## Configuration

```yaml
http:
  middlewares:
    geoblock:
      plugin:
        maxmind-geoblock:
          dbPath: /var/lib/geoip/GeoLite2-Country.mmdb
          allowedContinents: [EU, NA]
          deniedCountries: [FR]
          allowedIPs:
            - 127.0.0.1
            - 10.0.0.0/8
          trustedForwarders:
            - 10.0.0.0/8
          allowUnknown: false
          blockStatusCode: 403
          debug: false

  routers:
    my-router:
      rule: Host(`example.com`)
      service: my-service
      middlewares: [geoblock]
```

| Option              | Type       | Default  | Description |
|---------------------|------------|----------|-------------|
| `dbPath`            | string     | —        | **Required.** Path to a MaxMind GeoIP2/GeoLite2 **Country** database (City databases work too, but are slower and much larger). |
| `allowedCountries`  | []string   | `[]`     | Only requests from these countries pass. Cannot be combined with any other country/continent list. |
| `deniedCountries`   | []string   | `[]`     | Requests from these countries are blocked. Can be combined with `allowedContinents` **or** `deniedContinents`. |
| `allowedContinents` | []string   | `[]`     | Only requests from these continents pass (`AF`, `AN`, `AS`, `EU`, `NA`, `OC`, `SA`). Mutually exclusive with `deniedContinents`. |
| `deniedContinents`  | []string   | `[]`     | Requests from these continents are blocked. |
| `allowedIPs`        | []string   | `[]`     | IPs or CIDRs that bypass all geo checks (`127.0.0.1`, `10.0.0.0/8`, `fd00::/8`, …). |
| `allowUnknown`      | bool       | `false`  | Allow requests whose client IP has no location data (not in the database, record without country and continent, lookup error, database not loaded, client IP undeterminable). By default such requests are blocked. |
| `blockStatusCode`   | int        | `403`    | HTTP status code of blocked responses. Codes ≥ 400 get the status text as plain-text body. |
| `trustedForwarders` | []string   | `[]`     | IPs or CIDRs of proxies trusted to set `X-Forwarded-For`. See [Client IP](#client-ip). |
| `debug`             | bool       | `false`  | Log the effective configuration, database (re)loads and every decision (Traefik `DEBUG` level). |
| `dbReloadInterval`  | duration   | `1m`     | How often the database file is checked (size + modification time) and reloaded when changed. `0` disables reloading. |
| `cacheSize`         | int        | `10000`  | Number of client IPs whose lookup result is cached. Flushed when full and whenever the database is reloaded. `0` disables the cache. |

Country codes are case-insensitive and validated for shape (two letters); continent codes are
validated against the list above. Lists may also be given as comma-separated strings (Traefik
splits them), which is handy with Docker labels.

### Filter combinations

At least one list must be configured. The valid combinations are:

| `allowedCountries` | `deniedCountries` | `allowedContinents` | `deniedContinents` | Example |
|:------------------:|:-----------------:|:-------------------:|:------------------:|---------|
| ✔                  |                   |                     |                    | only `US`, `CA` |
|                    | ✔                 |                     |                    | everything but `RU` |
|                    |                   | ✔                   |                    | only Europe |
|                    |                   |                     | ✔                  | everything but Asia |
|                    | ✔                 | ✔                   |                    | Europe except `FR` |
|                    | ✔                 |                     | ✔                  | everything but Asia and `FR` |

When several lists are configured, a request is blocked as soon as **any** of them rejects it.

### Decision logic

1. The client IP is determined (see below). If it matches `allowedIPs`, the request is allowed and
   nothing else is evaluated.
2. The IP is looked up in the database. The record's `country.iso_code` and `continent.code` are
   used (MaxMind's best estimate of where the IP is located, not the registration country).
3. If **neither** a country nor a continent is known, the request is *unknown*: allowed only when
   `allowUnknown` is `true`.
4. Otherwise the lists are applied: `allowedCountries` (must contain the country), then
   `deniedCountries`, `allowedContinents` (must contain the continent) and `deniedContinents`. A
   record that has a continent but no country (rare) fails `allowedCountries` and passes
   `deniedCountries`; a missing continent likewise fails `allowedContinents`.

A missing, unreadable or corrupt database is **not** a startup error: the plugin logs the problem,
treats every lookup as unknown (so, by default, blocks everything except `allowedIPs`) and keeps
retrying at every `dbReloadInterval`. When a reload fails, the previously loaded database stays in
use.

### Client IP

By default the connecting peer address (`RemoteAddr`) is the client IP and `X-Forwarded-For` is
ignored, so clients cannot spoof their location.

When `trustedForwarders` is set and the peer is one of them, `X-Forwarded-For` is walked from right
to left and the first address that is **not** a trusted forwarder is the client (if all entries are
trusted, the left-most is used; if a trusted proxy sends a malformed header, the client is
*unknown*). IPv4-mapped IPv6 addresses are normalized to IPv4 and zones are stripped.

> [!NOTE]
> Traefik only forwards `X-Forwarded-For` from clients it trusts itself
> (`entryPoints.<name>.forwardedHeaders.trustedIPs`). Configure both consistently.

## Logging

Yaegi plugins can only log through stdout (mapped to Traefik's `DEBUG` level) and stderr (`ERROR`
level). Database load failures are always reported on stderr. With `debug: true`, each request
produces a line such as

```
maxmind-geoblock[geoblock@file]: 81.2.69.142 country=GB continent=EU blocked: country in deniedCountries GET example.com/api
```

## Performance

Traefik runs plugins in [Yaegi](https://github.com/traefik/yaegi), a Go interpreter that is two to
three orders of magnitude slower than compiled Go, where a function call costs ~3µs. The plugin is
designed around that:

- The `.mmdb` reader decodes only `country.iso_code` and `continent.code` and stops as soon as both
  are known; the search-tree walk is unrolled per record size and call-free.
- Decoded records are memoized by offset, and the tree walk of each /16 prefix is memoized, so a
  cold lookup is roughly half a tree walk plus a map access.
- Lookup results are cached per client IP (`cacheSize`).
- The request path (`ServeHTTP`) is flat: client IP parsing, allowlist check and decision are inlined.

Measured on the MaxMind test database (`make bench-traefik`, which loads the plugin into an
embedded Yaegi exactly like Traefik does; single core, one request at a time):

| Scenario                      | Native Go | Yaegi (Traefik) |
|-------------------------------|----------:|----------------:|
| allowed country, cached IP    | ~80 ns    | ~12 µs          |
| allowed country, uncached IP  | ~140 ns   | ~33 µs          |
| IPv6, uncached IP             | ~165 ns   | ~35 µs          |
| IP not in database, uncached  | ~215 ns   | ~26 µs          |
| `allowedIPs` bypass           | ~60 ns    | ~11 µs          |
| behind trusted forwarder      | ~240 ns   | ~31 µs          |

For comparison, the first version of the plugin (a straightforward reader with one function per
decoding step) took ~430µs per request under Yaegi; generically decoding a whole country record
(what a general-purpose reader does) costs ~4µs natively and ~900µs under Yaegi. The remaining
~11µs floor is Yaegi's cost of entering `ServeHTTP` and calling into the standard library. Numbers
will differ on your hardware; run the benchmarks yourself:

```sh
make bench           # go test -bench, compiled
make yaegi-bench     # yaegi test -bench, interpreted (needs the yaegi CLI)
make bench-traefik   # plugin loaded into an embedded Yaegi, like Traefik; native side by side
```

## Development

```sh
make test            # unit tests (compiled)
make yaegi-test      # the same tests under Yaegi, from a GOPATH tree like Traefik uses
make vet             # go vet + gofmt
make lint            # golangci-lint
make bench-all       # all benchmarks
```

Install the Yaegi CLI with `go install github.com/traefik/yaegi/cmd/yaegi@v0.16.1` (the version
embedded in Traefik v3).

Layout:

- [`geoblock.go`](geoblock.go) – configuration, validation, request handling, database watching.
- [`clientip.go`](clientip.go) – client IP determination behind trusted forwarders.
- [`mmdb/`](mmdb/) – the MaxMind DB reader (format: <https://maxmind.github.io/MaxMind-DB/>).
- [`testdata/`](testdata/) – test databases from
  [maxmind/MaxMind-DB](https://github.com/maxmind/MaxMind-DB) (MIT/Apache-2.0, see
  `testdata/LICENSE-MAXMIND-TEST-DATA`).
- [`tools/yaegibench/`](tools/yaegibench/) – separate module benchmarking the plugin inside an
  embedded Yaegi interpreter; keeps the plugin itself dependency-free.

Yaegi gotchas that shaped the code: no generics or Go ≥ 1.23 features, no `unsafe`/`syscall`
(hence no mmap: the database is read into memory, ~6 MB for GeoLite2 Country), explicitly typed
shift operands, and no `v, _ := x.(T)` on a possibly-nil interface (the destination may keep a
stale value).

## License

[Apache 2.0](LICENSE). GeoLite2 data is distributed by MaxMind under its own
[license](https://www.maxmind.com/en/geolite2/eula).
