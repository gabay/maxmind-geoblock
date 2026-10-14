// Package mmdb is a small, dependency-free reader for the MaxMind DB file
// format (https://maxmind.github.io/MaxMind-DB/).
//
// It is purpose-built for geoblocking: besides a generic decoder (used for
// metadata and debugging), it offers LookupCountry, a fast path that extracts
// only the country ISO code and the continent code of a record and stops
// decoding as soon as both are known.
//
// The whole database is read into memory (no mmap, no unsafe, no syscall), so
// the package can run both natively and inside Traefik's Yaegi interpreter.
package mmdb

import (
	"bytes"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"sync"
	"sync/atomic"
)

const (
	dataSectionSeparatorSize = 16
	metadataStartMarker      = "\xab\xcd\xefMaxMind.com"
	metadataMaxSize          = 128 * 1024

	// maxCachedRecords bounds the decoded-record memo; it is reset when
	// full (a country database has far fewer distinct records than that).
	maxCachedRecords = 16384

	// prefixBits is the number of leading address bits whose tree walk is
	// memoized (one entry per /16 prefix, for IPv4 and IPv6 separately).
	prefixBits = 16
)

// Errors returned by the reader.
var (
	ErrInvalidDatabase = errors.New("mmdb: invalid database")
	ErrIPv6InIPv4DB    = errors.New("mmdb: cannot look up an IPv6 address in an IPv4-only database")
)

// Metadata describes the database, see the "Database Metadata" section of the
// MaxMind DB specification.
type Metadata struct {
	NodeCount                uint32
	RecordSize               uint16
	IPVersion                uint16
	DatabaseType             string
	BuildEpoch               uint64
	Languages                []string
	Description              map[string]string
	BinaryFormatMajorVersion uint16
	BinaryFormatMinorVersion uint16
}

// Country is the subset of a GeoIP2/GeoLite2 Country (or City) record that is
// relevant for geoblocking. Fields are empty when absent from the record.
type Country struct {
	// ISOCode is the two-letter ISO 3166-1 code of the country where MaxMind
	// believes the IP address is located (the "country" field of the record).
	ISOCode string
	// ContinentCode is the two-letter continent code (AF, AN, AS, EU, NA, OC, SA).
	ContinentCode string
}

// Known reports whether the record carried any location information.
func (c Country) Known() bool {
	return c.ISOCode != "" || c.ContinentCode != ""
}

// Reader is an in-memory MaxMind DB reader. It is safe for concurrent use.
type Reader struct {
	meta Metadata

	tree []byte // search tree
	data []byte // data section (pointer offsets are relative to it)

	nodeCount  int
	recordSize int
	nodeSize   int // bytes per node
	ipv4Start  int // node to start IPv4 lookups from (IPv6 trees only)

	mu      sync.RWMutex
	records map[int]Country // decoded records by data section offset

	// prefix4 and prefix6 memoize, per /16 prefix, the node reached after
	// walking the first 16 bits of an address (0 = not computed yet). They
	// halve the IPv4 tree walk, which matters under an interpreter.
	prefix4 []uint32
	prefix6 []uint32
}

// Open reads the database file at path into memory and parses it.
func Open(path string) (*Reader, error) {
	buf, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	r, err := FromBytes(buf)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}

	return r, nil
}

// FromBytes parses a database held in memory. The reader keeps a reference to
// buf, which must not be modified afterwards.
func FromBytes(buf []byte) (*Reader, error) {
	metaStart := findMetadataStart(buf)
	if metaStart < 0 {
		return nil, fmt.Errorf("%w: metadata section not found", ErrInvalidDatabase)
	}

	metaSection := buf[metaStart+len(metadataStartMarker):]

	raw, _, err := decodeValue(metaSection, 0, 0)
	if err != nil {
		return nil, fmt.Errorf("%w: cannot decode metadata: %w", ErrInvalidDatabase, err)
	}

	meta, err := parseMetadata(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidDatabase, err)
	}

	nodeSize := int(meta.RecordSize) / 4
	treeSize := int(meta.NodeCount) * nodeSize
	dataStart := treeSize + dataSectionSeparatorSize

	if dataStart > metaStart {
		return nil, fmt.Errorf("%w: search tree exceeds file size", ErrInvalidDatabase)
	}

	r := &Reader{
		meta:       meta,
		tree:       buf[:treeSize],
		data:       buf[dataStart:metaStart],
		nodeCount:  int(meta.NodeCount),
		recordSize: int(meta.RecordSize),
		nodeSize:   nodeSize,
		records:    make(map[int]Country, 64),
		prefix4:    make([]uint32, 1<<prefixBits),
		prefix6:    make([]uint32, 1<<prefixBits),
	}

	if meta.IPVersion == 6 {
		// IPv4 addresses live under ::/96 in an IPv6 tree: follow 96 zero bits
		// once and remember where we end up.
		node := 0
		for i := 0; i < 96 && node < r.nodeCount; i++ {
			node = r.readRecord(node, 0)
		}
		r.ipv4Start = node
	}

	return r, nil
}

// Metadata returns the database metadata.
func (r *Reader) Metadata() Metadata {
	return r.meta
}

// LookupCountry returns the country and continent codes of the record matching
// ip. found is false when the database has no record for ip (in which case the
// returned Country is empty). An error means the database is malformed or the
// address family is not supported by the database.
//
// This is the hot path of the plugin and it is written for Yaegi, where a
// function call costs about ten times more than the work done per tree level:
// the tree walk is inlined and unrolled per record size, the first 16 bits of
// every /16 prefix are walked only once, and decoded records are memoized by
// offset (a country database holds just a few thousand distinct records).
func (r *Reader) LookupCountry(ip netip.Addr) (country Country, found bool, err error) {
	// As16 yields the IPv4-mapped form for IPv4 addresses, whose last 32 bits
	// are the IPv4 address: a single loop serves both families.
	b := ip.As16()
	nodeCount := r.nodeCount

	var (
		node   int
		i      int
		memo   []uint32
		prefix int
	)

	if ip.Is4() || ip.Is4In6() {
		node = r.ipv4Start
		i = 96
		memo = r.prefix4
		prefix = int(b[12])<<8 | int(b[13])
	} else if r.meta.IPVersion == 4 {
		return Country{}, false, ErrIPv6InIPv4DB
	} else {
		memo = r.prefix6
		prefix = int(b[0])<<8 | int(b[1])
	}

	// Skip the first 16 bits when this prefix was walked before. A walk that
	// stops early (node >= nodeCount) is memoized as well: the remaining
	// bits cannot change its outcome, and the loops below do not run for it.
	if n := atomic.LoadUint32(&memo[prefix]); n != 0 {
		node = int(n)
		i += prefixBits
	} else {
		end := i + prefixBits

		for ; i < end && node < nodeCount; i++ {
			node = r.readRecord(node, int(b[i>>3]>>(7-i&7))&1)
		}

		atomic.StoreUint32(&memo[prefix], uint32(node))
	}

	t := r.tree

	switch r.recordSize {
	case 24:
		for ; i < 128 && node < nodeCount; i++ {
			base := node * 6
			if b[i>>3]>>(7-i&7)&1 != 0 {
				base += 3
			}

			node = int(t[base])<<16 | int(t[base+1])<<8 | int(t[base+2])
		}
	case 28:
		for ; i < 128 && node < nodeCount; i++ {
			base := node * 7
			if b[i>>3]>>(7-i&7)&1 != 0 {
				node = int(t[base+3]&0x0F)<<24 | int(t[base+4])<<16 | int(t[base+5])<<8 | int(t[base+6])
			} else {
				node = int(t[base+3]&0xF0)<<20 | int(t[base])<<16 | int(t[base+1])<<8 | int(t[base+2])
			}
		}
	default: // 32
		for ; i < 128 && node < nodeCount; i++ {
			base := node * 8
			if b[i>>3]>>(7-i&7)&1 != 0 {
				base += 4
			}

			node = int(t[base])<<24 | int(t[base+1])<<16 | int(t[base+2])<<8 | int(t[base+3])
		}
	}

	if node == nodeCount {
		return Country{}, false, nil
	}

	if node < nodeCount {
		return Country{}, false, fmt.Errorf("%w: search tree is deeper than the address", ErrInvalidDatabase)
	}

	off := node - nodeCount - dataSectionSeparatorSize
	if off >= len(r.data) {
		return Country{}, false, fmt.Errorf("%w: record offset out of range", ErrInvalidDatabase)
	}

	r.mu.RLock()
	country, ok := r.records[off]
	r.mu.RUnlock()

	if ok {
		return country, true, nil
	}

	country, err = decodeCountry(r.data, off)
	if err != nil {
		return Country{}, true, err
	}

	r.mu.Lock()
	if len(r.records) >= maxCachedRecords {
		r.records = make(map[int]Country, 64)
	}

	r.records[off] = country
	r.mu.Unlock()

	return country, true, nil
}

// Lookup returns the full, generically decoded record matching ip
// (map[string]interface{} for GeoIP2 databases), or nil if there is none.
// It is slow compared to LookupCountry and intended for debugging and tests.
func (r *Reader) Lookup(ip netip.Addr) (interface{}, error) {
	off, found, err := r.lookupOffset(ip)
	if err != nil || !found {
		return nil, err
	}

	v, _, err := decodeValue(r.data, off, 0)

	return v, err
}

// lookupOffset is the straightforward search tree walk, used off the hot path
// (LookupCountry inlines an optimized version of it). It returns the offset of
// the record for ip in the data section.
func (r *Reader) lookupOffset(ip netip.Addr) (offset int, found bool, err error) {
	b := ip.As16()
	nodeCount := r.nodeCount
	node, i := 0, 0

	if ip.Is4() || ip.Is4In6() {
		node = r.ipv4Start
		i = 96
	} else if r.meta.IPVersion == 4 {
		return 0, false, ErrIPv6InIPv4DB
	}

	for ; i < 128 && node < nodeCount; i++ {
		node = r.readRecord(node, int(b[i>>3]>>(7-i&7))&1)
	}

	switch {
	case node == nodeCount:
		return 0, false, nil
	case node < nodeCount:
		return 0, false, fmt.Errorf("%w: search tree is deeper than the address", ErrInvalidDatabase)
	}

	offset = node - nodeCount - dataSectionSeparatorSize
	if offset >= len(r.data) {
		return 0, false, fmt.Errorf("%w: record offset out of range", ErrInvalidDatabase)
	}

	return offset, true, nil
}

// readRecord returns the left (bit == 0) or right (bit == 1) record of node.
// It is used off the hot path (load time, prefix memoization, lookupOffset);
// LookupCountry inlines the same logic.
func (r *Reader) readRecord(node, bit int) int {
	t := r.tree
	base := node * r.nodeSize

	switch r.recordSize {
	case 24:
		base += bit * 3

		return int(t[base])<<16 | int(t[base+1])<<8 | int(t[base+2])
	case 28:
		if bit == 0 {
			return int(t[base+3]&0xF0)<<20 | int(t[base])<<16 | int(t[base+1])<<8 | int(t[base+2])
		}

		return int(t[base+3]&0x0F)<<24 | int(t[base+4])<<16 | int(t[base+5])<<8 | int(t[base+6])
	default: // 32
		base += bit * 4

		return int(t[base])<<24 | int(t[base+1])<<16 | int(t[base+2])<<8 | int(t[base+3])
	}
}

func findMetadataStart(buf []byte) int {
	from := len(buf) - metadataMaxSize
	if from < 0 {
		from = 0
	}

	idx := bytes.LastIndex(buf[from:], []byte(metadataStartMarker))
	if idx < 0 {
		return -1
	}

	return from + idx
}

func parseMetadata(raw interface{}) (Metadata, error) {
	m, ok := raw.(map[string]interface{})
	if !ok {
		return Metadata{}, errors.New("metadata is not a map")
	}

	var meta Metadata

	nodeCount, ok := toUint64(m["node_count"])
	if !ok || nodeCount == 0 || nodeCount > 1<<31 {
		return Metadata{}, errors.New("invalid node_count")
	}

	meta.NodeCount = uint32(nodeCount)

	recordSize, ok := toUint64(m["record_size"])
	if !ok || (recordSize != 24 && recordSize != 28 && recordSize != 32) {
		return Metadata{}, fmt.Errorf("unsupported record_size %d", recordSize)
	}

	meta.RecordSize = uint16(recordSize)

	ipVersion, ok := toUint64(m["ip_version"])
	if !ok || (ipVersion != 4 && ipVersion != 6) {
		return Metadata{}, fmt.Errorf("unsupported ip_version %d", ipVersion)
	}

	meta.IPVersion = uint16(ipVersion)

	if major, ok := toUint64(m["binary_format_major_version"]); ok {
		meta.BinaryFormatMajorVersion = uint16(major)
	}

	if meta.BinaryFormatMajorVersion != 2 {
		return Metadata{}, fmt.Errorf("unsupported binary format major version %d", meta.BinaryFormatMajorVersion)
	}

	if minor, ok := toUint64(m["binary_format_minor_version"]); ok {
		meta.BinaryFormatMinorVersion = uint16(minor)
	}

	if epoch, ok := toUint64(m["build_epoch"]); ok {
		meta.BuildEpoch = epoch
	}

	meta.DatabaseType, _ = m["database_type"].(string)

	if langs, ok := m["languages"].([]interface{}); ok {
		for _, l := range langs {
			if s, ok := l.(string); ok {
				meta.Languages = append(meta.Languages, s)
			}
		}
	}

	if desc, ok := m["description"].(map[string]interface{}); ok {
		meta.Description = make(map[string]string, len(desc))

		for k, v := range desc {
			if s, ok := v.(string); ok {
				meta.Description[k] = s
			}
		}
	}

	return meta, nil
}

func toUint64(v interface{}) (uint64, bool) {
	switch n := v.(type) {
	case uint16:
		return uint64(n), true
	case uint32:
		return uint64(n), true
	case uint64:
		return n, true
	case int32:
		if n < 0 {
			return 0, false
		}

		return uint64(n), true
	default:
		return 0, false
	}
}
