package mmdb

import (
	"net/netip"
	"path/filepath"
	"testing"
)

func openBenchDB(b *testing.B, name string) *Reader {
	b.Helper()

	r, err := Open(filepath.Join(testdataDir, name))
	if err != nil {
		b.Fatal(err)
	}

	return r
}

func BenchmarkLookupCountryIPv4(b *testing.B) {
	r := openBenchDB(b, "GeoIP2-Country-Test.mmdb")
	ip := netip.MustParseAddr("81.2.69.142")

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		c, found, err := r.LookupCountry(ip)
		if err != nil || !found || c.ISOCode != "GB" {
			b.Fatal(c, found, err)
		}
	}
}

func BenchmarkLookupCountryIPv6(b *testing.B) {
	r := openBenchDB(b, "GeoIP2-Country-Test.mmdb")
	ip := netip.MustParseAddr("2001:218::1")

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		c, found, err := r.LookupCountry(ip)
		if err != nil || !found || c.ISOCode != "JP" {
			b.Fatal(c, found, err)
		}
	}
}

func BenchmarkLookupCountryNotFound(b *testing.B) {
	r := openBenchDB(b, "GeoIP2-Country-Test.mmdb")
	ip := netip.MustParseAddr("10.1.2.3")

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		_, found, err := r.LookupCountry(ip)
		if err != nil || found {
			b.Fatal(found, err)
		}
	}
}

func BenchmarkLookupOffsetSimpleWalk(b *testing.B) {
	r := openBenchDB(b, "GeoIP2-Country-Test.mmdb")
	ip := netip.MustParseAddr("81.2.69.142")

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		_, found, err := r.lookupOffset(ip)
		if err != nil || !found {
			b.Fatal(found, err)
		}
	}
}

func BenchmarkLookupGeneric(b *testing.B) {
	r := openBenchDB(b, "GeoIP2-Country-Test.mmdb")
	ip := netip.MustParseAddr("81.2.69.142")

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		if _, err := r.Lookup(ip); err != nil {
			b.Fatal(err)
		}
	}
}
