package mmdb

import (
	"errors"
	"math/big"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
)

const testdataDir = "../testdata"

func openTestDB(t *testing.T, name string) *Reader {
	t.Helper()

	r, err := Open(filepath.Join(testdataDir, name))
	if err != nil {
		t.Fatalf("Open(%s): %v", name, err)
	}

	return r
}

func TestMetadata(t *testing.T) {
	r := openTestDB(t, "GeoIP2-Country-Test.mmdb")
	m := r.Metadata()

	if m.DatabaseType != "GeoIP2-Country" {
		t.Errorf("DatabaseType = %q, want GeoIP2-Country", m.DatabaseType)
	}

	if m.IPVersion != 6 {
		t.Errorf("IPVersion = %d, want 6", m.IPVersion)
	}

	if m.RecordSize != 24 && m.RecordSize != 28 && m.RecordSize != 32 {
		t.Errorf("RecordSize = %d", m.RecordSize)
	}

	if m.NodeCount == 0 {
		t.Error("NodeCount = 0")
	}

	if m.BuildEpoch == 0 {
		t.Error("BuildEpoch = 0")
	}

	if m.BinaryFormatMajorVersion != 2 {
		t.Errorf("BinaryFormatMajorVersion = %d, want 2", m.BinaryFormatMajorVersion)
	}

	if len(m.Languages) == 0 {
		t.Error("Languages is empty")
	}

	if m.Description["en"] == "" {
		t.Errorf("Description = %v, want an English description", m.Description)
	}
}

func TestLookupCountry(t *testing.T) {
	tests := []struct {
		ip         string
		country    string
		continent  string
		found      bool
		geoIP2Only bool
	}{
		// Values come from MaxMind-DB/source-data/GeoIP2-Country-Test.json.
		{ip: "81.2.69.142", country: "GB", continent: "EU", found: true},
		{ip: "81.2.69.143", country: "GB", continent: "EU", found: true},
		{ip: "2.125.160.216", country: "GB", continent: "EU", found: true},
		{ip: "216.160.83.56", country: "US", continent: "NA", found: true},
		{ip: "89.160.20.112", country: "SE", continent: "EU", found: true},
		{ip: "67.43.156.1", country: "BT", continent: "AS", found: true},
		{ip: "202.196.224.9", country: "PH", continent: "AS", found: true},
		{ip: "2001:218::1", country: "JP", continent: "AS", found: true},
		{ip: "2001:220::1", country: "KR", continent: "AS", found: true},
		{ip: "2a02:d500::1", country: "", continent: "EU", found: true}, // continent only
		{ip: "::ffff:81.2.69.142", country: "GB", continent: "EU", found: true},
		{ip: "10.0.0.1", found: false},
		{ip: "127.0.0.1", found: false},
		{ip: "::1", found: false},
		{ip: "2001:db8::1", found: false},
		// Only present in the GeoIP2 fixture.
		{ip: "214.78.120.1", country: "US", continent: "NA", found: true, geoIP2Only: true},
		{ip: "214.1.1.1", country: "", continent: "", found: true, geoIP2Only: true}, // traits only
	}

	for _, name := range []string{"GeoIP2-Country-Test.mmdb", "GeoLite2-Country-Test.mmdb"} {
		r := openTestDB(t, name)

		for _, tc := range tests {
			if tc.geoIP2Only && name != "GeoIP2-Country-Test.mmdb" {
				continue
			}

			t.Run(name+"/"+tc.ip, func(t *testing.T) {
				c, found, err := r.LookupCountry(netip.MustParseAddr(tc.ip))
				if err != nil {
					t.Fatalf("LookupCountry: %v", err)
				}

				if found != tc.found {
					t.Fatalf("found = %v, want %v", found, tc.found)
				}

				if c.ISOCode != tc.country || c.ContinentCode != tc.continent {
					t.Errorf("got %+v, want country=%q continent=%q", c, tc.country, tc.continent)
				}

				if c.Known() != (tc.country != "" || tc.continent != "") {
					t.Errorf("Known() = %v", c.Known())
				}
			})
		}
	}
}

func TestLookupMatchesGenericDecoder(t *testing.T) {
	r := openTestDB(t, "GeoIP2-Country-Test.mmdb")

	for _, ip := range []string{"81.2.69.142", "216.160.83.56", "202.196.224.9", "2001:218::1", "214.1.1.1", "2a02:d500::1"} {
		addr := netip.MustParseAddr(ip)

		c, _, err := r.LookupCountry(addr)
		if err != nil {
			t.Fatal(err)
		}

		raw, err := r.Lookup(addr)
		if err != nil {
			t.Fatal(err)
		}

		rec, ok := raw.(map[string]interface{})
		if !ok {
			t.Fatalf("Lookup(%s) = %T, want map", ip, raw)
		}

		wantCountry := asString(nested(rec, "country", "iso_code"))
		wantContinent := asString(nested(rec, "continent", "code"))

		if c.ISOCode != wantCountry || c.ContinentCode != wantContinent {
			t.Errorf("%s: fast path %+v, generic country=%q continent=%q", ip, c, wantCountry, wantContinent)
		}
	}
}

// asString is a Yaegi-safe `s, _ := v.(string)`: under Yaegi a failed comma-ok
// assertion can leave the destination with a stale value.
func asString(v interface{}) string {
	if s, ok := v.(string); ok {
		return s
	}

	return ""
}

func nested(m map[string]interface{}, keys ...string) interface{} {
	var v interface{} = m

	for _, k := range keys {
		mm, ok := v.(map[string]interface{})
		if !ok {
			return nil
		}

		v = mm[k]
	}

	return v
}

// The MaxMind-DB-test-* databases map networks to {"ip": "<network>"} records
// and exist for every record size, which exercises all tree layouts.
func TestRecordSizes(t *testing.T) {
	tests := []struct {
		db   string
		ip   string
		want string // expected value of the "ip" field; "" means not found
	}{
		{db: "MaxMind-DB-test-ipv4-24.mmdb", ip: "1.1.1.1", want: "1.1.1.1"},
		{db: "MaxMind-DB-test-ipv4-24.mmdb", ip: "1.1.1.3", want: "1.1.1.2"},
		{db: "MaxMind-DB-test-ipv4-24.mmdb", ip: "1.1.1.7", want: "1.1.1.4"},
		{db: "MaxMind-DB-test-ipv4-24.mmdb", ip: "1.1.1.32", want: "1.1.1.32"},
		{db: "MaxMind-DB-test-ipv4-24.mmdb", ip: "1.1.1.33", want: ""},
		{db: "MaxMind-DB-test-ipv4-24.mmdb", ip: "1.1.1.64", want: ""},
		{db: "MaxMind-DB-test-ipv4-24.mmdb", ip: "::ffff:1.1.1.1", want: "1.1.1.1"},
		{db: "MaxMind-DB-test-ipv4-28.mmdb", ip: "1.1.1.1", want: "1.1.1.1"},
		{db: "MaxMind-DB-test-ipv4-28.mmdb", ip: "1.1.1.19", want: "1.1.1.16"},
		{db: "MaxMind-DB-test-ipv4-28.mmdb", ip: "1.1.1.64", want: ""},
		{db: "MaxMind-DB-test-ipv4-32.mmdb", ip: "1.1.1.1", want: "1.1.1.1"},
		{db: "MaxMind-DB-test-ipv4-32.mmdb", ip: "1.1.1.9", want: "1.1.1.8"},
		{db: "MaxMind-DB-test-ipv4-32.mmdb", ip: "1.1.1.64", want: ""},
		{db: "MaxMind-DB-test-mixed-24.mmdb", ip: "1.1.1.1", want: "::1.1.1.1"},
		{db: "MaxMind-DB-test-mixed-24.mmdb", ip: "::1:ffff:ffff", want: "::1:ffff:ffff"},
		{db: "MaxMind-DB-test-mixed-24.mmdb", ip: "::2:0:1", want: "::2:0:0"},
		{db: "MaxMind-DB-test-mixed-24.mmdb", ip: "2001:db8::1", want: ""},
		{db: "MaxMind-DB-test-mixed-28.mmdb", ip: "1.1.1.17", want: "::1.1.1.16"},
		{db: "MaxMind-DB-test-mixed-28.mmdb", ip: "::2:0:41", want: "::2:0:40"},
		{db: "MaxMind-DB-test-mixed-32.mmdb", ip: "1.1.1.32", want: "::1.1.1.32"},
		{db: "MaxMind-DB-test-mixed-32.mmdb", ip: "::2:0:51", want: "::2:0:50"},
	}

	for _, tc := range tests {
		t.Run(tc.db+"/"+tc.ip, func(t *testing.T) {
			r := openTestDB(t, tc.db)

			raw, err := r.Lookup(netip.MustParseAddr(tc.ip))
			if err != nil {
				t.Fatalf("Lookup: %v", err)
			}

			if tc.want == "" {
				if raw != nil {
					t.Fatalf("Lookup = %v, want not found", raw)
				}

				return
			}

			rec, _ := raw.(map[string]interface{})
			if got := asString(rec["ip"]); got != tc.want {
				t.Errorf("ip = %q, want %q (record %v)", got, tc.want, raw)
			}

			// The fast path must not choke on records without country data.
			c, found, err := r.LookupCountry(netip.MustParseAddr(tc.ip))
			if err != nil || !found || c.Known() {
				t.Errorf("LookupCountry = %+v, %v, %v", c, found, err)
			}
		})
	}
}

// TestPrefixMemo looks up addresses sharing a /16 prefix in different orders:
// the memoized partial walk must not leak one address's result into another.
func TestPrefixMemo(t *testing.T) {
	groups := [][]string{
		// Same /16, different outcomes (found GB / found GB / not found).
		{"81.2.69.142", "81.2.69.160", "81.2.1.1", "81.2.255.255"},
		// IPv4 prefix without any data: the memoized walk stops early.
		{"10.1.2.3", "10.200.1.1"},
		// IPv6: same /16 (2001::/16) with and without data.
		{"2001:218::1", "2001:218:ffff::1", "2001:219::1", "2001:db8::1"},
		// IPv4-mapped and plain forms share the IPv4 memo.
		{"::ffff:81.2.69.142", "81.2.69.142"},
	}

	fresh := openTestDB(t, "GeoIP2-Country-Test.mmdb")

	for _, group := range groups {
		for order := 0; order < len(group); order++ {
			r := openTestDB(t, "GeoIP2-Country-Test.mmdb")

			for k := 0; k < len(group); k++ {
				ip := netip.MustParseAddr(group[(order+k)%len(group)])

				want, wantFound, err := fresh.LookupCountry(ip)
				if err != nil {
					t.Fatal(err)
				}

				// Twice: the first call fills the memo, the second uses it.
				for pass := 0; pass < 2; pass++ {
					got, found, err := r.LookupCountry(ip)
					if err != nil {
						t.Fatal(err)
					}

					if got != want || found != wantFound {
						t.Errorf("%s (order %d, pass %d): got %+v/%v, want %+v/%v", ip, order, pass, got, found, want, wantFound)
					}
				}
			}
		}
	}
}

func TestIPv6InIPv4Database(t *testing.T) {
	r := openTestDB(t, "MaxMind-DB-test-ipv4-24.mmdb")

	if r.Metadata().IPVersion != 4 {
		t.Fatalf("IPVersion = %d, want 4", r.Metadata().IPVersion)
	}

	_, found, err := r.LookupCountry(netip.MustParseAddr("2001:db8::1"))
	if !errors.Is(err, ErrIPv6InIPv4DB) || found {
		t.Errorf("LookupCountry = found %v, err %v; want ErrIPv6InIPv4DB", found, err)
	}
}

func TestGenericDecoder(t *testing.T) {
	r := openTestDB(t, "MaxMind-DB-test-decoder.mmdb")

	raw, err := r.Lookup(netip.MustParseAddr("::1.1.1.0"))
	if err != nil {
		t.Fatal(err)
	}

	rec, ok := raw.(map[string]interface{})
	if !ok {
		t.Fatalf("Lookup = %T, want map", raw)
	}

	checks := map[string]interface{}{
		"utf8_string": "unicode! ☯ - ♫",
		"boolean":     true,
		"double":      42.123456,
		"float":       float32(1.1),
		"int32":       int32(-268435456),
		"uint16":      uint16(100),
		"uint32":      uint32(268435456),
		"uint64":      uint64(1152921504606846976),
	}

	for k, want := range checks {
		if got := rec[k]; got != want {
			t.Errorf("%s = %v (%T), want %v (%T)", k, got, got, want, want)
		}
	}

	if got, ok := rec["bytes"].([]byte); !ok || string(got) != "\x00\x00\x00\x2a" {
		t.Errorf("bytes = %x", got)
	}

	if arr, ok := rec["array"].([]interface{}); !ok || len(arr) != 3 || arr[0] != uint32(1) || arr[2] != uint32(3) {
		t.Errorf("array = %v", rec["array"])
	}

	if got := nested(rec, "map", "mapX", "utf8_stringX"); got != "hello" {
		t.Errorf("map.mapX.utf8_stringX = %v", got)
	}

	if got, ok := nested(rec, "map", "mapX", "arrayX").([]interface{}); !ok || len(got) != 3 {
		t.Errorf("map.mapX.arrayX = %v", got)
	}

	if u, ok := rec["uint128"].(*big.Int); !ok || u.String() != "1329227995784915872903807060280344576" {
		t.Errorf("uint128 = %v (%T)", rec["uint128"], rec["uint128"])
	}

	// The pointer-decoder database wraps every value behind pointers.
	rp := openTestDB(t, "MaxMind-DB-test-pointer-decoder.mmdb")

	raw, err = rp.Lookup(netip.MustParseAddr("::1.1.1.0"))
	if err != nil {
		t.Fatal(err)
	}

	rec, _ = raw.(map[string]interface{})
	if rec["utf8_string"] != "unicode! ☯ - ♫" || nested(rec, "map", "mapX", "utf8_stringX") != "hello" {
		t.Errorf("pointer-decoder record = %v", raw)
	}
}

func TestInvalidDatabases(t *testing.T) {
	if _, err := Open(filepath.Join(testdataDir, "does-not-exist.mmdb")); err == nil {
		t.Error("Open(missing) succeeded")
	}

	if _, err := FromBytes([]byte("definitely not a database")); !errors.Is(err, ErrInvalidDatabase) {
		t.Errorf("FromBytes(garbage) = %v, want ErrInvalidDatabase", err)
	}

	buf, err := os.ReadFile(filepath.Join(testdataDir, "GeoIP2-Country-Test.mmdb"))
	if err != nil {
		t.Fatal(err)
	}

	// Truncating the file drops the metadata.
	if _, err := FromBytes(buf[:len(buf)/2]); !errors.Is(err, ErrInvalidDatabase) {
		t.Errorf("FromBytes(truncated) = %v, want ErrInvalidDatabase", err)
	}

	// Keep the metadata but drop most of the tree/data: the tree no longer
	// fits in the file.
	start := findMetadataStart(buf)
	broken := append(append([]byte{}, buf[:1024]...), buf[start:]...)

	if _, err := FromBytes(broken); !errors.Is(err, ErrInvalidDatabase) {
		t.Errorf("FromBytes(broken) = %v, want ErrInvalidDatabase", err)
	}
}

func TestReadControlSizes(t *testing.T) {
	tests := []struct {
		in   []byte
		typ  int
		size int
		next int
	}{
		{in: []byte{0x40 | 5, 'h', 'e', 'l', 'l', 'o'}, typ: typeString, size: 5, next: 1},
		{in: []byte{0x40 | 29, 1}, typ: typeString, size: 30, next: 2},
		{in: []byte{0x40 | 30, 1, 2}, typ: typeString, size: 285 + 258, next: 3},
		{in: []byte{0x40 | 31, 1, 2, 3}, typ: typeString, size: 65821 + 66051, next: 4},
		{in: []byte{0xE0 | 1}, typ: typeMap, size: 1, next: 1},
		{in: []byte{0x00 | 3, 7}, typ: typeBool, size: 3, next: 2}, // extended type 14 - 7 = 7
		{in: []byte{0x20 | 0x00 | 1, 0x10, 0, 0, 0, 0, 0}, typ: typePointer, size: 0x110, next: 2},
		{in: []byte{0x20 | 0x08 | 1, 0x02, 0x03, 0, 0, 0, 0}, typ: typePointer, size: 0x10203 + 2048, next: 3},
	}

	for i, tc := range tests {
		// Pad so that pointer targets stay in range.
		data := append(append([]byte{}, tc.in...), make([]byte, 70000)...)

		typ, size, next, err := readControl(data, 0)
		if err != nil {
			t.Errorf("case %d: %v", i, err)

			continue
		}

		if typ != tc.typ || size != tc.size || next != tc.next {
			t.Errorf("case %d: got (%d, %d, %d), want (%d, %d, %d)", i, typ, size, next, tc.typ, tc.size, tc.next)
		}
	}

	if _, _, _, err := readControl([]byte{}, 0); err == nil {
		t.Error("readControl(empty) succeeded")
	}

	if _, _, _, err := readControl([]byte{0x40 | 29}, 0); err == nil {
		t.Error("readControl(truncated size) succeeded")
	}
}
