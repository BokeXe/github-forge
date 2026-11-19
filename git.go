package main

// Git smart HTTP for the forge: protocol v2/v0 upload-pack, ls-refs, fetch and
// v0 receive-pack. Faithful Go port of the Node entry's git.ts.
//
// SHA-1 only, object-format sha1. Object bytes are stored opaquely and never
// rebuilt; packs the server emits carry plain (non-delta) objects, while packs
// it reads may carry OFS_DELTA and REF_DELTA.
//
// Auth: every git request needs `Authorization: Bearer <token>` where the
// token is the one NewApp passes in (MAKEFUN_SHOP_TOKEN). A missing or wrong
// token is 401 with a JSON message; a missing repo is 404 on info/refs (and
// on the POST services). Malformed input never crashes the server: it is
// answered 400 and the repo is untouched.
//
// HandleGit returns true when the request was a git route (and it owns the
// response), false when it was not, so app.go can fall through.
//
// Concurrency (per CONTRACT.md): request bodies are read before locking;
// clone/fetch pack building runs under store.Mu.RLock so concurrent clones
// build in parallel; push mutations run under store.Mu.Lock. Response bytes
// are fully built under the lock and written after unlocking. The per-repo
// response cache lives in Repo.PackCache; cache inserts take the write lock
// briefly after the (parallel) build, so no map is ever written under RLock.

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode"
)

const gitZeroOID = "0000000000000000000000000000000000000000"
const gitPktLineMax = 65520
const gitPktLineMaxPayload = gitPktLineMax - 4
const gitAgent = "makefun-forge/1.0"

var (
	gitHex40Re     = regexp.MustCompile(`^[0-9a-f]{40}$`)
	gitNameRe      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)
	gitHex4Re      = regexp.MustCompile(`^[0-9a-fA-F]{4}$`)
	gitRouteRe     = regexp.MustCompile(`^/([^/]+)/([^/]+)\.git/(info/refs|git-upload-pack|git-receive-pack)$`)
	gitV2VersionRe = regexp.MustCompile(`(?:^|;)\s*version=2\s*(?:;|$)`)
)

// Manual equivalents of the regexps above for the hot paths (every git
// request, every pkt-line header, every object id). Semantics identical.
func gitIsHex40(value string) bool {
	if len(value) != 40 {
		return false
	}
	for i := 0; i < 40; i++ {
		c := value[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func gitIsObjectID(value string) bool { return gitIsHex40(value) }

// gitParseHex4 parses exactly 4 hex digits; ok is false on any non-hex byte.
func gitParseHex4(b []byte) (int, bool) {
	if len(b) < 4 {
		return 0, false
	}
	v := 0
	for i := 0; i < 4; i++ {
		c := b[i]
		var d int
		switch {
		case c >= '0' && c <= '9':
			d = int(c - '0')
		case c >= 'a' && c <= 'f':
			d = int(c-'a') + 10
		case c >= 'A' && c <= 'F':
			d = int(c-'A') + 10
		default:
			return 0, false
		}
		v = v*16 + d
	}
	return v, true
}

const gitHexDigits = "0123456789abcdef"

func gitAppendHex4(out []byte, v int) []byte {
	return append(out, gitHexDigits[(v>>12)&0xf], gitHexDigits[(v>>8)&0xf], gitHexDigits[(v>>4)&0xf], gitHexDigits[v&0xf])
}

func gitIsHex4(s string) bool {
	if len(s) != 4 {
		return false
	}
	for i := 0; i < 4; i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

func gitIsName(s string) bool {
	if len(s) == 0 || len(s) > 100 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		alnum := c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9'
		if i == 0 {
			if !alnum {
				return false
			}
		} else if !alnum && c != '.' && c != '_' && c != '-' {
			return false
		}
	}
	return true
}

// gitMatchRoute is gitRouteRe without the regexp engine: it returns
// owner, repo, service for /{owner}/{repo}.git/{service}.
func gitMatchRoute(path string) (owner, repo, service string, ok bool) {
	if len(path) < 2 || path[0] != '/' {
		return "", "", "", false
	}
	rest := path[1:]
	slash := strings.IndexByte(rest, '/')
	if slash <= 0 {
		return "", "", "", false
	}
	owner = rest[:slash]
	rest = rest[slash+1:]
	slash = strings.IndexByte(rest, '/')
	if slash <= 0 {
		return "", "", "", false
	}
	repoPart := rest[:slash]
	service = rest[slash+1:]
	if !strings.HasSuffix(repoPart, ".git") {
		return "", "", "", false
	}
	repo = repoPart[:len(repoPart)-4]
	if repo == "" {
		return "", "", "", false
	}
	switch service {
	case "info/refs", "git-upload-pack", "git-receive-pack":
		return owner, repo, service, true
	}
	return "", "", "", false
}

// gitHeaderWantsV2 is gitV2VersionRe: a `version=2` token bounded by
// start/semicolon (with optional space) and semicolon/end.
func gitHeaderWantsV2(header string) bool {
	for i := 0; i+9 <= len(header); i++ {
		if header[i:i+9] != "version=2" {
			continue
		}
		// Left boundary: start, or ';' followed by optional spaces.
		leftOK := false
		if i == 0 {
			leftOK = true
		} else {
			j := i - 1
			for j >= 0 && (header[j] == ' ' || header[j] == '\t') {
				j--
			}
			if j >= 0 && header[j] == ';' {
				leftOK = true
			}
		}
		if !leftOK {
			continue
		}
		// Right boundary: end, or optional spaces then ';'.
		j := i + 9
		for j < len(header) && (header[j] == ' ' || header[j] == '\t') {
			j++
		}
		if j >= len(header) || header[j] == ';' {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------------------------
// object ids, deltas, packs
// ---------------------------------------------------------------------------------------------

// gitApplyDelta applies a git delta (copy/insert instruction stream) to a
// base object's bytes.
func gitApplyDelta(base, delta []byte) ([]byte, error) {
	offset := 0
	readSize := func() (uint64, error) {
		var value uint64
		var shift uint
		for {
			if offset >= len(delta) {
				return 0, fmt.Errorf("delta size varint is truncated")
			}
			b := delta[offset]
			offset++
			value += uint64(b&0x7f) << shift
			shift += 7
			if b&0x80 == 0 {
				break
			}
			if shift > 63 {
				return 0, fmt.Errorf("delta size varint is too long")
			}
		}
		return value, nil
	}
	sourceSize, err := readSize()
	if err != nil {
		return nil, err
	}
	targetSize, err := readSize()
	if err != nil {
		return nil, err
	}
	if sourceSize != uint64(len(base)) {
		return nil, fmt.Errorf("delta expects a %d-byte base, got %d", sourceSize, len(base))
	}
	// TS allocates targetSize bytes up front; refuse absurd sizes instead of
	// attempting the allocation (valid packs never come close).
	if targetSize > 512<<20 {
		return nil, fmt.Errorf("delta target size %d is too large", targetSize)
	}
	out := make([]byte, targetSize)
	var written uint64
	for offset < len(delta) {
		op := delta[offset]
		offset++
		if op&0x80 != 0 {
			var copyOffset, copySize uint64
			var shift uint
			for bit := 0; bit < 4; bit++ {
				if op&(1<<bit) != 0 {
					if offset >= len(delta) {
						return nil, fmt.Errorf("delta copy offset is truncated")
					}
					copyOffset += uint64(delta[offset]) << shift
					offset++
				}
				shift += 8
			}
			shift = 0
			for bit := 4; bit < 7; bit++ {
				if op&(1<<bit) != 0 {
					if offset >= len(delta) {
						return nil, fmt.Errorf("delta copy size is truncated")
					}
					copySize += uint64(delta[offset]) << shift
					offset++
				}
				shift += 8
			}
			if copySize == 0 {
				copySize = 0x10000
			}
			if copyOffset+copySize > uint64(len(base)) {
				return nil, fmt.Errorf("delta copy is out of the base")
			}
			if written+copySize > targetSize {
				return nil, fmt.Errorf("delta writes past its target size")
			}
			copy(out[written:written+copySize], base[copyOffset:copyOffset+copySize])
			written += copySize
		} else if op != 0 {
			literal := uint64(op & 0x7f)
			if uint64(offset)+literal > uint64(len(delta)) {
				return nil, fmt.Errorf("delta insert is truncated")
			}
			if written+literal > targetSize {
				return nil, fmt.Errorf("delta writes past its target size")
			}
			copy(out[written:written+literal], delta[offset:offset+int(literal)])
			offset += int(literal)
			written += literal
		} else {
			return nil, fmt.Errorf("delta opcode 0 is invalid")
		}
	}
	if written != targetSize {
		return nil, fmt.Errorf("delta produced %d of %d bytes", written, targetSize)
	}
	return out, nil
}

var gitTypeCode = map[string]byte{"commit": 1, "tree": 2, "blob": 3, "tag": 4}
var gitCodeType = map[byte]string{1: "commit", 2: "tree", 3: "blob", 4: "tag"}

func gitWritePackObjectHeader(objType string, size int) []byte {
	code := gitTypeCode[objType]
	var out []byte
	remaining := size
	first := (int(code) << 4) | (remaining & 0x0f)
	remaining /= 16
	for remaining > 0 {
		out = append(out, byte(first|0x80))
		first = remaining & 0x7f
		remaining /= 128
	}
	out = append(out, byte(first))
	return out
}

// gitWritePack builds a packfile (version 2) from plain objects, no deltas.
// The trailer is SHA-1 over everything before it. Objects are deflated with
// zlib at BestSpeed through a pooled writer: pack bytes are not compared
// byte-for-byte anywhere (clients inflate and verify object hashes), so the
// compression level is free to favour speed. Pooling matters: a fresh
// flate writer allocates ~100 KB of window/dictionary state per object.
var gitZlibWriterPool = sync.Pool{
	New: func() any {
		w, _ := zlib.NewWriterLevel(io.Discard, zlib.BestSpeed)
		return w
	},
}

func gitWritePack(objects []*GitObject) []byte {
	size := 12 + 20
	for _, object := range objects {
		size += len(object.Data) + 16
	}
	var body bytes.Buffer
	body.Grow(size)
	header := make([]byte, 12)
	copy(header, "PACK")
	binary.BigEndian.PutUint32(header[4:], 2)
	binary.BigEndian.PutUint32(header[8:], uint32(len(objects)))
	body.Write(header)
	zw := gitZlibWriterPool.Get().(*zlib.Writer)
	for _, object := range objects {
		body.Write(gitWritePackObjectHeader(object.Type, len(object.Data)))
		zw.Reset(&body)
		zw.Write(object.Data)
		zw.Close()
	}
	gitZlibWriterPool.Put(zw)
	trailer := sha1.Sum(body.Bytes())
	body.Write(trailer[:])
	return body.Bytes()
}

type gitPackEntry struct {
	Offset   int
	Type     string
	Size     uint64
	Data     []byte
	OID      string
	Delta    string // "", "ofs" or "ref"
	BaseOID  string
	HasDelta bool
}

type gitPackReadResult struct {
	Version         uint32
	DeclaredCount   uint32
	Objects         map[string]*GitObject
	Entries         []gitPackEntry
	Trailer         string
	ComputedTrailer string
	ChecksumOk      bool
	DeltaCount      int
	ByteLength      int
}

func gitReadVarintSize(buffer []byte, start int) (size uint64, next int, first byte, err error) {
	if start >= len(buffer) {
		return 0, 0, 0, fmt.Errorf("pack entry has no size header")
	}
	b := buffer[start]
	first = b
	size = uint64(b & 0x0f)
	var shift uint = 4
	offset := start + 1
	for b&0x80 != 0 {
		if offset >= len(buffer) {
			return 0, 0, 0, fmt.Errorf("pack entry size varint is truncated")
		}
		b = buffer[offset]
		offset++
		size += uint64(b&0x7f) << shift
		shift += 7
		if shift > 63 {
			return 0, 0, 0, fmt.Errorf("pack entry size varint is too long")
		}
	}
	return size, offset, first, nil
}

func gitReadOfsDeltaBase(buffer []byte, start int) (baseOffset uint64, next int, err error) {
	if start >= len(buffer) {
		return 0, 0, fmt.Errorf("ofs-delta has no base offset")
	}
	b := buffer[start]
	offset := start + 1
	value := uint64(b & 0x7f)
	for b&0x80 != 0 {
		if offset >= len(buffer) {
			return 0, 0, fmt.Errorf("ofs-delta base offset is truncated")
		}
		b = buffer[offset]
		offset++
		value = ((value + 1) * 128) + uint64(b&0x7f)
	}
	return value, offset, nil
}

// gitCountingByteReader counts the bytes a zlib stream consumes. Because it
// implements io.ByteReader, Go's flate/zlib readers do not read past the end
// of the stream, so the count is exactly the stream's byte length (the Go
// equivalent of Node inflateSync's engine.bytesWritten).
type gitCountingByteReader struct {
	r *bytes.Reader
	n int64
}

func (c *gitCountingByteReader) ReadByte() (byte, error) {
	b, err := c.r.ReadByte()
	if err == nil {
		c.n++
	}
	return b, err
}

func (c *gitCountingByteReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// Pooled zlib readers: a fresh reader allocates a 32 KiB window plus
// dictionary state per object inflated (the push workload inflates 3
// objects per push, 200 pushes a round). Reset re-reads the zlib header
// from the new counting reader, so the consumed-byte count stays exact.
var gitEmptyZlibStream = func() []byte {
	var b bytes.Buffer
	w := zlib.NewWriter(&b)
	w.Close()
	return b.Bytes()
}()

var gitZlibReaderPool = sync.Pool{
	New: func() any {
		r, err := zlib.NewReader(bytes.NewReader(gitEmptyZlibStream))
		if err != nil {
			panic(err)
		}
		return r
	},
}

func gitInflateAt(buffer []byte, offset int) (data []byte, next int, err error) {
	return gitInflateAtSize(buffer, offset, -1)
}

// gitInflateAtSize is gitInflateAt with the expected inflated size known
// (pack entries declare it): the output buffer is allocated exactly once
// instead of growing through io.ReadAll.
func gitInflateAtSize(buffer []byte, offset int, size int64) (data []byte, next int, err error) {
	counter := &gitCountingByteReader{r: bytes.NewReader(buffer[offset:])}
	zr := gitZlibReaderPool.Get().(io.ReadCloser)
	resetErr := zr.(zlib.Resetter).Reset(counter, nil)
	if resetErr != nil {
		gitZlibReaderPool.Put(zr)
		return nil, 0, fmt.Errorf("zlib stream at offset %d did not inflate: %s", offset, resetErr.Error())
	}
	var zerr error
	if size >= 0 && size <= 512<<20 {
		data = make([]byte, size)
		_, zerr = io.ReadFull(zr, data)
		if zerr == nil {
			// The stream must end exactly here.
			var extra [1]byte
			n, rerr := zr.Read(extra[:])
			if n != 0 || (rerr != nil && rerr != io.EOF) {
				zerr = fmt.Errorf("zlib stream has trailing data")
			}
		}
	} else {
		data, zerr = io.ReadAll(zr)
	}
	zr.Close()
	gitZlibReaderPool.Put(zr)
	if zerr != nil {
		return nil, 0, fmt.Errorf("zlib stream at offset %d did not inflate: %s", offset, zerr.Error())
	}
	if counter.n <= 0 {
		return nil, 0, fmt.Errorf("zlib stream at offset %d consumed no bytes", offset)
	}
	return data, offset + int(counter.n), nil
}

type gitRawEntry struct {
	offset       int
	typeCode     byte
	size         uint64
	data         []byte
	baseOffset   int
	baseOID      string
	resolvedType string
	resolved     []byte
	oid          string
}

// gitParsePack reads a packfile (version 2 or 3) and resolves every object,
// including OFS_DELTA and REF_DELTA. resolveBase supplies a base object a
// thin pack leaves out. When light is true and the pack carries no deltas,
// each entry's type is its own and the delta machinery is skipped (the
// receive-pack fast path, as in git.ts).
func gitParsePack(pack []byte, resolveBase func(oid string) *GitObject, light bool) (*gitPackReadResult, error) {
	if len(pack) < 32 {
		return nil, fmt.Errorf("pack is %d bytes, too short", len(pack))
	}
	if string(pack[:4]) != "PACK" {
		return nil, fmt.Errorf("pack does not start with PACK")
	}
	version := binary.BigEndian.Uint32(pack[4:])
	if version != 2 && version != 3 {
		return nil, fmt.Errorf("unsupported pack version %d", version)
	}
	declaredCount := binary.BigEndian.Uint32(pack[8:])
	trailerStart := len(pack) - 20
	trailer := hex.EncodeToString(pack[trailerStart:])
	packBody := pack[:trailerStart]
	computed := sha1.Sum(packBody)
	computedTrailer := hex.EncodeToString(computed[:])

	var raw []*gitRawEntry
	byOffset := make(map[int]*gitRawEntry)
	offset := 12
	for index := uint32(0); index < declaredCount; index++ {
		entryOffset := offset
		size, next, first, err := gitReadVarintSize(pack, offset)
		if err != nil {
			return nil, err
		}
		offset = next
		typeCode := (first >> 4) & 0x07
		entry := &gitRawEntry{offset: entryOffset, typeCode: typeCode, size: size}
		if typeCode == 6 {
			baseOff, n, err := gitReadOfsDeltaBase(pack, offset)
			if err != nil {
				return nil, err
			}
			offset = n
			if baseOff <= 0 || baseOff > uint64(entryOffset) {
				return nil, fmt.Errorf("ofs-delta at %d names an invalid base offset %d", entryOffset, baseOff)
			}
			entry.baseOffset = entryOffset - int(baseOff)
		} else if typeCode == 7 {
			if offset+20 > trailerStart {
				return nil, fmt.Errorf("ref-delta base id is truncated")
			}
			entry.baseOID = hex.EncodeToString(pack[offset : offset+20])
			offset += 20
		} else if _, ok := gitCodeType[typeCode]; !ok {
			return nil, fmt.Errorf("pack entry %d has unknown type code %d", index, typeCode)
		}
		inflated, n, err := gitInflateAtSize(packBody, offset, int64(size))
		if err != nil {
			return nil, err
		}
		if uint64(len(inflated)) != size {
			return nil, fmt.Errorf("pack entry at %d declares %d bytes but inflated to %d", entryOffset, size, len(inflated))
		}
		entry.data = inflated
		offset = n
		raw = append(raw, entry)
		byOffset[entryOffset] = entry
	}
	if offset != trailerStart {
		return nil, fmt.Errorf("pack has %d stray bytes before its trailer", trailerStart-offset)
	}

	// Lean path (receive-pack's only need: the objects + the checksum).
	if light {
		hasDelta := false
		for _, entry := range raw {
			if entry.typeCode == 6 || entry.typeCode == 7 {
				hasDelta = true
				break
			}
		}
		if !hasDelta {
			lightObjects := make(map[string]*GitObject)
			for _, entry := range raw {
				objType := gitCodeType[entry.typeCode]
				oid := HashObject(objType, entry.data)
				if _, ok := lightObjects[oid]; !ok {
					lightObjects[oid] = &GitObject{Type: objType, Data: entry.data}
				}
			}
			return &gitPackReadResult{
				Version:         version,
				DeclaredCount:   declaredCount,
				Objects:         lightObjects,
				Entries:         nil,
				Trailer:         trailer,
				ComputedTrailer: computedTrailer,
				ChecksumOk:      trailer == computedTrailer,
				DeltaCount:      0,
				ByteLength:      len(pack),
			}, nil
		}
	}

	var entries []gitPackEntry
	objects := make(map[string]*GitObject)
	resolving := make(map[int]bool)
	resolvedByOID := make(map[string]*gitRawEntry)

	var resolve func(entry *gitRawEntry) (objType string, data []byte, oid string, err error)
	resolve = func(entry *gitRawEntry) (string, []byte, string, error) {
		if entry.resolved != nil && entry.resolvedType != "" && entry.oid != "" {
			return entry.resolvedType, entry.resolved, entry.oid, nil
		}
		if resolving[entry.offset] {
			return "", nil, "", fmt.Errorf("delta cycle at pack offset %d", entry.offset)
		}
		resolving[entry.offset] = true
		var objType string
		var data []byte
		if entry.typeCode == 6 {
			baseEntry := byOffset[entry.baseOffset]
			if baseEntry == nil {
				return "", nil, "", fmt.Errorf("ofs-delta base at offset %d is not in the pack", entry.baseOffset)
			}
			baseType, baseData, _, err := resolve(baseEntry)
			if err != nil {
				return "", nil, "", err
			}
			objType = baseType
			data, err = gitApplyDelta(baseData, entry.data)
			if err != nil {
				return "", nil, "", err
			}
		} else if entry.typeCode == 7 {
			wanted := entry.baseOID
			if baseEntry, ok := resolvedByOID[wanted]; ok {
				baseType, baseData, _, err := resolve(baseEntry)
				if err != nil {
					return "", nil, "", err
				}
				objType = baseType
				data, err = gitApplyDelta(baseData, entry.data)
				if err != nil {
					return "", nil, "", err
				}
			} else {
				var external *GitObject
				if resolveBase != nil {
					external = resolveBase(wanted)
				}
				if external == nil {
					return "", nil, "", fmt.Errorf("ref-delta base %s is neither in the pack nor supplied", wanted)
				}
				objType = external.Type
				var err error
				data, err = gitApplyDelta(external.Data, entry.data)
				if err != nil {
					return "", nil, "", err
				}
			}
		} else {
			objType = gitCodeType[entry.typeCode]
			data = entry.data
		}
		oid := HashObject(objType, data)
		entry.resolved = data
		entry.resolvedType = objType
		entry.oid = oid
		resolvedByOID[oid] = entry
		delete(resolving, entry.offset)
		return objType, data, oid, nil
	}

	deltaCount := 0
	for _, entry := range raw {
		objType, data, oid, err := resolve(entry)
		if err != nil {
			return nil, err
		}
		if entry.typeCode == 6 || entry.typeCode == 7 {
			deltaCount++
		}
		if _, ok := objects[oid]; !ok {
			objects[oid] = &GitObject{Type: objType, Data: data}
		}
		pe := gitPackEntry{Offset: entry.offset, Type: objType, Size: entry.size, Data: entry.data, OID: oid}
		if entry.typeCode == 6 {
			pe.HasDelta = true
			pe.Delta = "ofs"
			if base := byOffset[entry.baseOffset]; base != nil {
				pe.BaseOID = base.oid
			}
		} else if entry.typeCode == 7 {
			pe.HasDelta = true
			pe.Delta = "ref"
			pe.BaseOID = entry.baseOID
		}
		entries = append(entries, pe)
	}

	return &gitPackReadResult{
		Version:         version,
		DeclaredCount:   declaredCount,
		Objects:         objects,
		Entries:         entries,
		Trailer:         trailer,
		ComputedTrailer: computedTrailer,
		ChecksumOk:      trailer == computedTrailer,
		DeltaCount:      deltaCount,
		ByteLength:      len(pack),
	}, nil
}

// ---------------------------------------------------------------------------------------------
// object graph: parsing and reachability
// ---------------------------------------------------------------------------------------------

type gitTreeEntry struct {
	Mode string
	Name string
	OID  string
}

func gitParseTree(data []byte) ([]gitTreeEntry, error) {
	var entries []gitTreeEntry
	offset := 0
	for offset < len(data) {
		space := bytes.IndexByte(data[offset:], 0x20)
		if space < 0 {
			return nil, fmt.Errorf("tree entry has no mode separator")
		}
		space += offset
		nul := bytes.IndexByte(data[space+1:], 0x00)
		if nul < 0 {
			return nil, fmt.Errorf("tree entry has no name terminator")
		}
		nul += space + 1
		if nul+21 > len(data) {
			return nil, fmt.Errorf("tree entry has a truncated object id")
		}
		entries = append(entries, gitTreeEntry{
			Mode: string(data[offset:space]),
			Name: string(data[space+1 : nul]),
			OID:  hex.EncodeToString(data[nul+1 : nul+21]),
		})
		offset = nul + 21
	}
	return entries, nil
}

type gitCommitInfo struct {
	Tree    string
	Parents []string
}

func gitParseCommit(data []byte) (gitCommitInfo, error) {
	blank := bytes.Index(data, []byte("\n\n"))
	headerEnd := len(data)
	if blank >= 0 {
		headerEnd = blank
	}
	var info gitCommitInfo
	for _, line := range strings.Split(string(data[:headerEnd]), "\n") {
		if strings.HasPrefix(line, "tree ") {
			info.Tree = strings.TrimSpace(line[5:])
		} else if strings.HasPrefix(line, "parent ") {
			info.Parents = append(info.Parents, strings.TrimSpace(line[7:]))
		}
	}
	if !gitIsObjectID(info.Tree) {
		return info, fmt.Errorf("commit has no valid tree header")
	}
	return info, nil
}

func gitParseTag(data []byte) (object string, objType string, err error) {
	blank := bytes.Index(data, []byte("\n\n"))
	headerEnd := len(data)
	if blank >= 0 {
		headerEnd = blank
	}
	for _, line := range strings.Split(string(data[:headerEnd]), "\n") {
		if strings.HasPrefix(line, "object ") {
			object = strings.TrimSpace(line[7:])
		} else if strings.HasPrefix(line, "type ") {
			objType = strings.TrimSpace(line[5:])
		}
	}
	if !gitIsObjectID(object) {
		return "", "", fmt.Errorf("tag has no valid object header")
	}
	return object, objType, nil
}

type gitObjectLookup func(oid string) *GitObject

// gitCollectReachable returns every object reachable from roots: commit ->
// tree + parents, tree -> entries (gitlinks skipped), tag -> its target.
// cut names commits whose parents are not followed (a shallow clone's
// boundary commits).
func gitCollectReachable(lookup gitObjectLookup, roots []string, allowMissing bool, cut map[string]bool) (map[string]bool, error) {
	seen := make(map[string]bool)
	var queue []string
	for _, root := range roots {
		if gitIsObjectID(root) {
			queue = append(queue, root)
		}
	}
	for len(queue) > 0 {
		oid := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if seen[oid] {
			continue
		}
		object := lookup(oid)
		if object == nil {
			if allowMissing {
				continue
			}
			return nil, fmt.Errorf("object %s is missing from the store", oid)
		}
		seen[oid] = true
		switch object.Type {
		case "commit":
			commit, err := gitParseCommit(object.Data)
			if err != nil {
				return nil, err
			}
			queue = append(queue, commit.Tree)
			if !cut[oid] {
				queue = append(queue, commit.Parents...)
			}
		case "tag":
			target, _, err := gitParseTag(object.Data)
			if err != nil {
				return nil, err
			}
			queue = append(queue, target)
		case "tree":
			entries, err := gitParseTree(object.Data)
			if err != nil {
				return nil, err
			}
			for _, entry := range entries {
				if entry.Mode == "160000" {
					continue
				}
				queue = append(queue, entry.OID)
			}
		}
	}
	return seen, nil
}

// gitOidSet is an insertion-ordered set of oids (JS Set semantics).
type gitOidSet struct {
	m     map[string]bool
	order []string
}

func newGitOidSet() *gitOidSet { return &gitOidSet{m: make(map[string]bool)} }

func (s *gitOidSet) add(oid string) {
	if !s.m[oid] {
		s.m[oid] = true
		s.order = append(s.order, oid)
	}
}

func (s *gitOidSet) has(oid string) bool { return s.m[oid] }

// gitDistMap is an insertion-ordered oid -> distance map (JS Map semantics:
// re-setting an existing key keeps its original position).
type gitDistMap struct {
	m     map[string]int
	order []string
}

func newGitDistMap() *gitDistMap { return &gitDistMap{m: make(map[string]int)} }

func (d *gitDistMap) get(oid string) (int, bool) { v, ok := d.m[oid]; return v, ok }

func (d *gitDistMap) set(oid string, dist int) {
	if _, ok := d.m[oid]; !ok {
		d.order = append(d.order, oid)
	}
	d.m[oid] = dist
}

func (d *gitDistMap) has(oid string) bool { _, ok := d.m[oid]; return ok }

type gitDepthWalk struct {
	Included   *gitOidSet
	CommitDist *gitDistMap
	CommitInfo map[string]gitCommitInfo
}

// gitCollectWithDepth is reachability from roots with a commit-generation
// limit. depth counts commits: depth 1 includes only the wanted commits
// themselves (plus their trees), depth 2 adds their parents, and so on; no
// limit when depth is nil. Tag hops do not consume depth.
func gitCollectWithDepth(lookup gitObjectLookup, roots []string, depth *int) (*gitDepthWalk, error) {
	included := newGitOidSet()
	commitDist := newGitDistMap()
	commitInfo := make(map[string]gitCommitInfo)
	seenNonCommit := make(map[string]bool)
	type queueItem struct {
		oid  string
		dist int
	}
	var queue []queueItem
	for _, root := range roots {
		if gitIsObjectID(root) {
			queue = append(queue, queueItem{root, 0})
		}
	}
	head := 0

	walkTree := func(oid string) error {
		stack := []string{oid}
		for len(stack) > 0 {
			current := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if seenNonCommit[current] || included.has(current) {
				continue
			}
			object := lookup(current)
			if object == nil {
				continue
			}
			seenNonCommit[current] = true
			included.add(current)
			if object.Type == "tree" {
				entries, err := gitParseTree(object.Data)
				if err != nil {
					return err
				}
				for _, entry := range entries {
					if entry.Mode == "160000" {
						continue
					}
					stack = append(stack, entry.OID)
				}
			} else if object.Type == "tag" {
				// A tree entry never names a tag in practice; stay safe.
				target, _, err := gitParseTag(object.Data)
				if err != nil {
					return err
				}
				queue = append(queue, queueItem{target, 0})
			}
		}
		return nil
	}

	for head < len(queue) {
		item := queue[head]
		head++
		oid, dist := item.oid, item.dist
		object := lookup(oid)
		if object == nil {
			continue
		}
		switch object.Type {
		case "commit":
			if known, ok := commitDist.get(oid); ok && known <= dist {
				continue
			}
			commitDist.set(oid, dist)
			included.add(oid)
			info, err := gitParseCommit(object.Data)
			if err != nil {
				continue
			}
			commitInfo[oid] = info
			if err := walkTree(info.Tree); err != nil {
				return nil, err
			}
			if depth == nil || dist+1 < *depth {
				for _, parent := range info.Parents {
					queue = append(queue, queueItem{parent, dist + 1})
				}
			}
		case "tag":
			if seenNonCommit[oid] {
				continue
			}
			seenNonCommit[oid] = true
			included.add(oid)
			target, _, err := gitParseTag(object.Data)
			if err != nil {
				return nil, err
			}
			queue = append(queue, queueItem{target, dist})
		case "tree":
			if err := walkTree(oid); err != nil {
				return nil, err
			}
		default:
			if seenNonCommit[oid] {
				continue
			}
			seenNonCommit[oid] = true
			included.add(oid)
		}
	}
	return &gitDepthWalk{Included: included, CommitDist: commitDist, CommitInfo: commitInfo}, nil
}

// gitResolveToCommit returns the commit an oid resolves to: itself for a
// commit, the tag target for a tag, else "".
func gitResolveToCommit(lookup gitObjectLookup, oid string) (string, error) {
	current := oid
	seen := make(map[string]bool)
	for steps := 0; steps < 16; steps++ {
		if seen[current] {
			return "", nil
		}
		seen[current] = true
		object := lookup(current)
		if object == nil {
			return "", nil
		}
		if object.Type == "commit" {
			return current, nil
		}
		if object.Type == "tag" {
			target, _, err := gitParseTag(object.Data)
			if err != nil {
				return "", err
			}
			current = target
			continue
		}
		return "", nil
	}
	return "", nil
}

// gitIsAncestor reports whether ancestor is reachable from descendant by
// following commit parents.
func gitIsAncestor(lookup gitObjectLookup, ancestor, descendant string) bool {
	if ancestor == descendant {
		return true
	}
	seen := make(map[string]bool)
	queue := []string{descendant}
	for len(queue) > 0 {
		oid := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if seen[oid] {
			continue
		}
		seen[oid] = true
		object := lookup(oid)
		if object == nil || object.Type != "commit" {
			continue
		}
		info, err := gitParseCommit(object.Data)
		if err != nil {
			continue
		}
		for _, parent := range info.Parents {
			if parent == ancestor {
				return true
			}
			queue = append(queue, parent)
		}
	}
	return false
}

// gitPeelToCommit returns the commit an annotated tag peels to, or "".
func gitPeelToCommit(lookup gitObjectLookup, oid string) (string, error) {
	return gitResolveToCommit(lookup, oid)
}

// ---------------------------------------------------------------------------------------------
// pkt-line codec
// ---------------------------------------------------------------------------------------------

type gitPacket struct {
	Kind    string // "data", "flush", "delim" or "response-end"
	Payload []byte
}

// gitPktItem is one item to encode: a data payload or a special packet.
type gitPktItem struct {
	data    []byte
	special string // "", "flush", "delim" or "response-end"
}

func gitPktData(data []byte) gitPktItem { return gitPktItem{data: data} }
func gitPktStr(s string) gitPktItem     { return gitPktItem{data: []byte(s)} }

var gitPktFlushItem = gitPktItem{special: "flush"}
var gitPktDelimItem = gitPktItem{special: "delim"}

func gitEncodePktLines(items []gitPktItem) ([]byte, error) {
	total := 0
	for _, item := range items {
		if item.special != "" {
			total += 4
			continue
		}
		if len(item.data) > gitPktLineMaxPayload {
			return nil, fmt.Errorf("pkt-line payload is %d bytes, over the maximum", len(item.data))
		}
		total += len(item.data) + 4
	}
	out := make([]byte, 0, total)
	for _, item := range items {
		switch item.special {
		case "flush":
			out = append(out, "0000"...)
		case "delim":
			out = append(out, "0001"...)
		case "response-end":
			out = append(out, "0002"...)
		default:
			out = gitAppendHex4(out, len(item.data)+4)
			out = append(out, item.data...)
		}
	}
	return out, nil
}

// ---------------------------------------------------------------------------------------------
// per-repo response cache
// ---------------------------------------------------------------------------------------------

// Cached whole-response buffers live in Repo.PackCache, keyed exactly as in
// git.ts (ref snapshot for advertisements/ls-refs; wants/haves/depth for
// fetches — git objects are content-addressed and immutable, so the same tip
// always packs to the same bytes). Bounded to 8 entries per repo. The
// snapshot is built from sorted refs so the key is deterministic in Go (TS
// used Map insertion order; the key is internal either way).

// gitCacheGet reads the cache. Caller must hold store.Mu (read or write).
func gitCacheGet(repo *Repo, key string) ([]byte, bool) {
	buf, ok := repo.PackCache[key]
	return buf, ok
}

// gitCachePut stores a response in the cache, evicting an arbitrary entry
// when full (TS evicted FIFO; eviction choice is not wire-visible since a
// rebuild produces identical bytes). It takes the write lock itself, so it
// must be called with no lock held.
func gitCachePut(store *Store, repo *Repo, key string, buf []byte) {
	store.Mu.Lock()
	defer store.Mu.Unlock()
	if repo.PackCache == nil {
		repo.PackCache = make(map[string][]byte)
	}
	if len(repo.PackCache) >= 8 {
		if _, exists := repo.PackCache[key]; !exists {
			for oldest := range repo.PackCache {
				delete(repo.PackCache, oldest)
				break
			}
		}
	}
	repo.PackCache[key] = buf
}

// gitDecodePacketsUntilFlush decodes pkt-lines from the start of buffer. It
// stops at the first flush packet and reports how many bytes were consumed,
// so a caller can treat the rest as raw data (receive-pack's pack). ok is
// false on any malformed header, over-long packet or truncation.
func gitDecodePacketsUntilFlush(buffer []byte) (ok bool, packets []gitPacket, consumed int) {
	offset := 0
	for offset < len(buffer) {
		if offset+4 > len(buffer) {
			return false, packets, offset
		}
		lengthInt, hexOk := gitParseHex4(buffer[offset : offset+4])
		if !hexOk {
			return false, packets, offset
		}
		length := int64(lengthInt)
		if length == 0 {
			return true, append(packets, gitPacket{Kind: "flush"}), offset + 4
		}
		if length == 1 {
			packets = append(packets, gitPacket{Kind: "delim"})
			offset += 4
			continue
		}
		if length == 2 {
			packets = append(packets, gitPacket{Kind: "response-end"})
			offset += 4
			continue
		}
		if length == 3 || length > gitPktLineMax {
			return false, packets, offset
		}
		if offset+int(length) > len(buffer) {
			return false, packets, offset
		}
		packets = append(packets, gitPacket{Kind: "data", Payload: buffer[offset+4 : offset+int(length)]})
		offset += int(length)
	}
	return false, packets, offset
}

// gitDecodePackets decodes a whole buffer of pkt-lines; ok is false when
// anything is malformed or trailing.
func gitDecodePackets(buffer []byte) (ok bool, packets []gitPacket) {
	offset := 0
	for offset < len(buffer) {
		if offset+4 > len(buffer) {
			return false, packets
		}
		lengthInt, hexOk := gitParseHex4(buffer[offset : offset+4])
		if !hexOk {
			return false, packets
		}
		length := int64(lengthInt)
		if length == 0 {
			packets = append(packets, gitPacket{Kind: "flush"})
			offset += 4
			continue
		}
		if length == 1 {
			packets = append(packets, gitPacket{Kind: "delim"})
			offset += 4
			continue
		}
		if length == 2 {
			packets = append(packets, gitPacket{Kind: "response-end"})
			offset += 4
			continue
		}
		if length == 3 || length > gitPktLineMax {
			return false, packets
		}
		if offset+int(length) > len(buffer) {
			return false, packets
		}
		packets = append(packets, gitPacket{Kind: "data", Payload: buffer[offset+4 : offset+int(length)]})
		offset += int(length)
	}
	return true, packets
}

func gitPacketLines(packets []gitPacket) []string {
	var lines []string
	for _, packet := range packets {
		if packet.Kind == "data" {
			lines = append(lines, string(packet.Payload))
		}
	}
	return lines
}

// gitSidebandPack splits pack bytes into sideband band-1 payloads (band byte
// + <= 32 KiB of pack data). These are pkt-line payloads: the caller adds
// the length header.
func gitSidebandPack(pack []byte) [][]byte {
	var parts [][]byte
	const chunkSize = 32768
	for offset := 0; offset < len(pack); offset += chunkSize {
		end := offset + chunkSize
		if end > len(pack) {
			end = len(pack)
		}
		chunk := pack[offset:end]
		framed := make([]byte, len(chunk)+1)
		framed[0] = 1
		copy(framed[1:], chunk)
		parts = append(parts, framed)
	}
	return parts
}

// ---------------------------------------------------------------------------------------------
// fetch planning
// ---------------------------------------------------------------------------------------------

type gitFetchPlan struct {
	Objects   []*GitObject
	Shallow   []string
	Unshallow []string
	Acks      []string
}

func gitPlanFetch(repo *Repo, wants, haves, shallowOld []string, depth *int) (*gitFetchPlan, error) {
	lookup := func(oid string) *GitObject { return GetObject(repo, oid) }
	walk, err := gitCollectWithDepth(lookup, wants, depth)
	if err != nil {
		return nil, err
	}
	// What the client already holds: reachable from its haves, but a commit
	// the client names as a shallow boundary contributes only itself and its
	// tree, never its parents.
	cut := make(map[string]bool, len(shallowOld))
	for _, oid := range shallowOld {
		cut[oid] = true
	}
	haveReachable, err := gitCollectReachable(lookup, haves, true, cut)
	if err != nil {
		return nil, err
	}
	var objects []*GitObject
	for _, oid := range walk.Included.order {
		if haveReachable[oid] {
			continue
		}
		if object := lookup(oid); object != nil {
			objects = append(objects, object)
		}
	}
	var acks []string
	for _, oid := range haves {
		if lookup(oid) != nil {
			acks = append(acks, oid)
		}
	}
	var shallow, unshallow []string
	if depth != nil {
		haveSet := make(map[string]bool, len(haves))
		for _, oid := range haves {
			haveSet[oid] = true
		}
		shallowSet := make(map[string]bool)
		for _, oid := range walk.CommitDist.order {
			dist, _ := walk.CommitDist.get(oid)
			if dist != *depth-1 {
				continue
			}
			info, ok := walk.CommitInfo[oid]
			if !ok || len(info.Parents) == 0 {
				continue
			}
			// A boundary commit whose parent the client already holds is
			// not shallow at all.
			parentHeld := false
			for _, parent := range info.Parents {
				if haveSet[parent] {
					parentHeld = true
					break
				}
			}
			if parentHeld {
				continue
			}
			shallow = append(shallow, oid)
			shallowSet[oid] = true
		}
		for _, boundary := range shallowOld {
			if !walk.CommitDist.has(boundary) || shallowSet[boundary] {
				continue
			}
			info, ok := walk.CommitInfo[boundary]
			if !ok {
				continue
			}
			for _, parent := range info.Parents {
				if walk.CommitDist.has(parent) {
					unshallow = append(unshallow, boundary)
					break
				}
			}
		}
	}
	return &gitFetchPlan{Objects: objects, Shallow: shallow, Unshallow: unshallow, Acks: acks}, nil
}

// ---------------------------------------------------------------------------------------------
// advertisements and ref listings
// ---------------------------------------------------------------------------------------------

type gitRefEntry struct {
	Name string
	OID  string
}

func gitSortedRefs(repo *Repo) []gitRefEntry {
	refs := make([]gitRefEntry, 0, len(repo.Refs))
	for name, oid := range repo.Refs {
		refs = append(refs, gitRefEntry{Name: name, OID: oid})
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].Name < refs[j].Name })
	return refs
}

func gitHeadOID(repo *Repo) string {
	if oid, ok := repo.Refs[repo.HeadTarget]; ok {
		return oid
	}
	return gitZeroOID
}

const gitUploadV0Caps = "agent=" + gitAgent + " object-format=sha1 symref=HEAD:refs/heads/main side-band-64k ofs-delta thin-pack multi_ack multi_ack_detailed shallow"
const gitReceiveV0Caps = "report-status side-band-64k delete-refs ofs-delta agent=" + gitAgent + " object-format=sha1 symref=HEAD:refs/heads/main"

// gitV0Advertisement builds a v0 ref advertisement: service preamble, then
// refs, caps after NUL on the first line only.
func gitV0Advertisement(repo *Repo, service string) ([]byte, error) {
	caps := gitUploadV0Caps
	if service != "git-upload-pack" {
		caps = gitReceiveV0Caps
	}
	lookup := func(oid string) *GitObject { return GetObject(repo, oid) }
	lines := []gitPktItem{gitPktStr("# service=" + service + "\n"), gitPktFlushItem}
	refs := gitSortedRefs(repo)
	if len(refs) == 0 {
		lines = append(lines, gitPktStr(gitZeroOID+" capabilities^{}\x00"+caps+"\n"))
	} else {
		lines = append(lines, gitPktStr(gitHeadOID(repo)+" HEAD\x00"+caps+"\n"))
		for _, ref := range refs {
			lines = append(lines, gitPktStr(ref.OID+" "+ref.Name+"\n"))
			if strings.HasPrefix(ref.Name, "refs/tags/") {
				if object := lookup(ref.OID); object != nil && object.Type == "tag" {
					peeled, err := gitPeelToCommit(lookup, ref.OID)
					if err != nil {
						return nil, err
					}
					if peeled != "" {
						lines = append(lines, gitPktStr(peeled+" "+ref.Name+"^{}\n"))
					}
				}
			}
		}
	}
	lines = append(lines, gitPktFlushItem)
	return gitEncodePktLines(lines)
}

// gitV2Advertisement is a v2 capability advertisement for upload-pack.
// Static: it names no repo state.
var gitV2AdvertisementBytes = func() []byte {
	buf, _ := gitEncodePktLines([]gitPktItem{
		gitPktStr("version 2\n"),
		gitPktStr("fetch=shallow filter\n"),
		gitPktStr("ls-refs=unborn\n"),
		gitPktStr("object-format=sha1\n"),
		gitPktStr("agent=" + gitAgent + "\n"),
		gitPktFlushItem,
	})
	return buf
}()

type gitListedRef struct {
	OID          string
	Name         string
	Peeled       string
	HasPeeled    bool
	SymrefTarget string
	HasSymref    bool
	Unborn       bool
}

// gitListRefsForLsRefs returns the refs ls-refs answers, in git's order
// (HEAD first, then sorted), already prefix-filtered.
func gitListRefsForLsRefs(repo *Repo, peel, symrefs bool, prefixes []string) ([]gitListedRef, error) {
	lookup := func(oid string) *GitObject { return GetObject(repo, oid) }
	keep := func(name string) bool {
		if len(prefixes) == 0 {
			return true
		}
		for _, prefix := range prefixes {
			if strings.HasPrefix(name, prefix) {
				return true
			}
		}
		return false
	}
	var out []gitListedRef
	head := gitHeadOID(repo)
	if keep("HEAD") {
		entry := gitListedRef{OID: head, Name: "HEAD"}
		if head == gitZeroOID {
			entry.Unborn = true
		}
		if symrefs {
			entry.SymrefTarget = repo.HeadTarget
			entry.HasSymref = true
		}
		out = append(out, entry)
	}
	for _, ref := range gitSortedRefs(repo) {
		if !keep(ref.Name) {
			continue
		}
		entry := gitListedRef{OID: ref.OID, Name: ref.Name}
		if peel && strings.HasPrefix(ref.Name, "refs/tags/") {
			if object := lookup(ref.OID); object != nil && object.Type == "tag" {
				peeled, err := gitPeelToCommit(lookup, ref.OID)
				if err != nil {
					return nil, err
				}
				if peeled != "" {
					entry.Peeled = peeled
					entry.HasPeeled = true
				}
			}
		}
		out = append(out, entry)
	}
	return out, nil
}

func gitLsRefsResponseLine(ref gitListedRef) string {
	if ref.Unborn {
		line := "unborn " + ref.Name
		if ref.HasSymref {
			line += " symref-target:" + ref.SymrefTarget
		}
		return line + "\n"
	}
	line := ref.OID + " " + ref.Name
	if ref.HasSymref {
		line += " symref-target:" + ref.SymrefTarget
	}
	if ref.HasPeeled {
		line += " peeled:" + ref.Peeled
	}
	return line + "\n"
}

// gitLsRefsResponse is the ls-refs answer for a repo and argument lines, as
// one buffer.
func gitLsRefsResponse(repo *Repo, lines []string) ([]byte, error) {
	peel := false
	symrefs := false
	var prefixes []string
	for _, raw := range lines {
		line := strings.TrimSuffix(raw, "\n")
		if line == "peel" {
			peel = true
		} else if line == "symrefs" {
			symrefs = true
		} else if strings.HasPrefix(line, "ref-prefix ") {
			prefixes = append(prefixes, line[len("ref-prefix "):])
		}
		// 'unborn', 'agent=...', 'object-format=...', 'command=ls-refs':
		// noted, no extra work — an unborn HEAD is listed whenever HEAD is
		// unborn, prefix rules permitting.
	}
	refs, err := gitListRefsForLsRefs(repo, peel, symrefs, prefixes)
	if err != nil {
		return nil, err
	}
	items := make([]gitPktItem, 0, len(refs)+1)
	for _, ref := range refs {
		items = append(items, gitPktStr(gitLsRefsResponseLine(ref)))
	}
	items = append(items, gitPktFlushItem)
	return gitEncodePktLines(items)
}

// ---------------------------------------------------------------------------------------------
// HTTP helpers
// ---------------------------------------------------------------------------------------------

type gitResponse struct {
	status      int
	contentType string
	body        []byte
}

func gitJSONResponse(status int, message string) gitResponse {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.Encode(map[string]string{"message": message})
	return gitResponse{status: status, contentType: "application/json; charset=utf-8", body: buf.Bytes()}
}

func gitBufferResponse(status int, contentType string, body []byte) gitResponse {
	return gitResponse{status: status, contentType: contentType, body: body}
}

// gitQueryParam reads one query parameter without building a map.
// Values are compared literally by callers (service names are plain
// ASCII), so no percent-decoding is needed here.
func gitQueryParam(rawQuery, key string) string {
	for len(rawQuery) > 0 {
		pair := rawQuery
		if i := strings.IndexByte(rawQuery, '&'); i >= 0 {
			pair = rawQuery[:i]
			rawQuery = rawQuery[i+1:]
		} else {
			rawQuery = ""
		}
		if eq := strings.IndexByte(pair, '='); eq >= 0 && pair[:eq] == key {
			return pair[eq+1:]
		}
	}
	return ""
}

func gitAuthorised(r *http.Request, token string) bool {
	if token == "" {
		return true // no token configured (local dev): do not lock the door on nothing
	}
	return r.Header.Get("Authorization") == "Bearer "+token
}

func gitWantsV2(r *http.Request) bool {
	return gitHeaderWantsV2(r.Header.Get("Git-Protocol"))
}

// ---------------------------------------------------------------------------------------------
// the handler
// ---------------------------------------------------------------------------------------------

// HandleGit serves one git smart-HTTP request. It returns true when the path
// was a git route (the response is then this function's business, even when
// it answers an error), false otherwise.
func HandleGit(w http.ResponseWriter, r *http.Request, store *Store, token string) bool {
	pathname := r.URL.EscapedPath()
	if pathname == "" {
		pathname = "/"
	}
	owner, repoName, service, isGit := gitMatchRoute(pathname)
	if !isGit {
		return false
	}
	resp := gitServe(r, store, token, owner, repoName, service)
	w.Header().Set("content-type", resp.contentType)
	w.Header().Set("content-length", strconv.Itoa(len(resp.body)))
	w.WriteHeader(resp.status)
	w.Write(resp.body)
	return true
}

func gitServe(r *http.Request, store *Store, token, owner, repoName, service string) (resp gitResponse) {
	// Malformed input and unexpected failures alike: never crash the server,
	// never leave a request hanging, and never half-apply a push (ref updates
	// happen only in the guarded receive-pack path below, after the pack
	// parsed).
	defer func() {
		if rec := recover(); rec != nil {
			resp = gitJSONResponse(500, fmt.Sprint(rec))
		}
	}()

	if !gitAuthorised(r, token) {
		return gitJSONResponse(401, "Unauthorized")
	}
	if !gitIsName(owner) || !gitIsName(repoName) {
		return gitJSONResponse(404, "Not Found")
	}
	method := r.Method
	if method == "" {
		method = "GET"
	}

	if service == "info/refs" {
		if method != "GET" {
			return gitJSONResponse(405, "Method Not Allowed")
		}
		queryService := gitQueryParam(r.URL.RawQuery, "service")
		if queryService != "git-upload-pack" && queryService != "git-receive-pack" {
			return gitJSONResponse(404, "Not Found")
		}
		wantsV2 := queryService == "git-upload-pack" && gitWantsV2(r)
		store.Mu.RLock()
		repo := store.GetRepo(owner, repoName)
		if repo == nil {
			store.Mu.RUnlock()
			return gitJSONResponse(404, "Not Found")
		}
		if wantsV2 {
			store.Mu.RUnlock()
			return gitBufferResponse(200, "application/x-"+queryService+"-advertisement", gitV2AdvertisementBytes)
		}
		cacheKey := "adv0:" + queryService + ":" + strconv.Itoa(repo.RefsVersion)
		if cached, ok := gitCacheGet(repo, cacheKey); ok {
			store.Mu.RUnlock()
			return gitBufferResponse(200, "application/x-"+queryService+"-advertisement", cached)
		}
		body, err := gitV0Advertisement(repo, queryService)
		store.Mu.RUnlock()
		if err != nil {
			return gitJSONResponse(500, err.Error())
		}
		gitCachePut(store, repo, cacheKey, body)
		return gitBufferResponse(200, "application/x-"+queryService+"-advertisement", body)
	}

	if method != "POST" {
		return gitJSONResponse(405, "Method Not Allowed")
	}
	store.Mu.RLock()
	repo := store.GetRepo(owner, repoName)
	store.Mu.RUnlock()
	if repo == nil {
		return gitJSONResponse(404, "Not Found")
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return gitJSONResponse(500, err.Error())
	}
	if service == "git-upload-pack" {
		return gitHandleUploadPack(r, store, repo, body)
	}
	return gitHandleReceivePack(store, repo, body)
}

// ---------------------------------------------------------------------------------------------
// upload-pack: ls-refs and fetch, v2 and v0
// ---------------------------------------------------------------------------------------------

type gitFetchArgs struct {
	Wants   []string
	Haves   []string
	Shallow []string
	Depth   *int
}

// gitParseJSInt mimics JS Number.parseInt(s, 10): leading integer prefix,
// NaN (ok=false) when there are no digits.
func gitParseJSInt(s string) (int, bool) {
	s = strings.TrimSpace(s)
	i := 0
	neg := false
	if i < len(s) && (s[i] == '+' || s[i] == '-') {
		neg = s[i] == '-'
		i++
	}
	start := i
	value := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		value = value*10 + int(s[i]-'0')
		i++
	}
	if i == start {
		return 0, false
	}
	if neg {
		value = -value
	}
	return value, true
}

func gitParseFetchArgs(lines []string) gitFetchArgs {
	var args gitFetchArgs
	for _, raw := range lines {
		line := strings.TrimSuffix(raw, "\n")
		if strings.HasPrefix(line, "want ") {
			oid := strings.Split(line[5:], " ")[0]
			if gitIsObjectID(oid) {
				args.Wants = append(args.Wants, oid)
			}
		} else if strings.HasPrefix(line, "have ") {
			oid := strings.TrimSpace(line[5:])
			if gitIsObjectID(oid) {
				args.Haves = append(args.Haves, oid)
			}
		} else if strings.HasPrefix(line, "shallow ") {
			oid := strings.TrimSpace(line[8:])
			if gitIsObjectID(oid) {
				args.Shallow = append(args.Shallow, oid)
			}
		} else if strings.HasPrefix(line, "deepen ") {
			if value, ok := gitParseJSInt(line[7:]); ok && value > 0 {
				v := value
				args.Depth = &v
			}
		}
		// deepen-since / deepen-not / filter / thin-pack / no-progress /
		// ofs-delta / agent / object-format / done: accepted and ignored.
	}
	return args
}

func gitFetchCacheKey(version string, args gitFetchArgs) string {
	depth := ""
	if args.Depth != nil {
		depth = strconv.Itoa(*args.Depth)
	}
	return "fetch:" + version + ":" + strings.Join(args.Wants, ",") + "|" +
		strings.Join(args.Haves, ",") + "|" + strings.Join(args.Shallow, ",") + "|" + depth
}

func gitHandleUploadPack(r *http.Request, store *Store, repo *Repo, body []byte) gitResponse {
	wantsV2 := gitWantsV2(r)
	ok, packets := gitDecodePackets(body)
	if !ok || len(packets) == 0 {
		return gitJSONResponse(400, "malformed pkt-line request")
	}
	lines := gitPacketLines(packets)
	first := ""
	if len(lines) > 0 {
		first = lines[0]
	}
	if strings.HasPrefix(first, "command=") {
		command := strings.TrimSpace(strings.TrimSuffix(first[len("command="):], "\n"))
		if command == "ls-refs" {
			store.Mu.RLock()
			cacheKey := "ls:" + strconv.Itoa(repo.RefsVersion) + ":" + strings.Join(lines, "|")
			if cached, hit := gitCacheGet(repo, cacheKey); hit {
				store.Mu.RUnlock()
				return gitBufferResponse(200, "application/x-git-upload-pack-result", cached)
			}
			buf, err := gitLsRefsResponse(repo, lines)
			store.Mu.RUnlock()
			if err != nil {
				return gitJSONResponse(500, err.Error())
			}
			gitCachePut(store, repo, cacheKey, buf)
			return gitBufferResponse(200, "application/x-git-upload-pack-result", buf)
		}
		if command == "fetch" {
			args := gitParseFetchArgs(lines)
			cacheKey := gitFetchCacheKey("2", args)
			store.Mu.RLock()
			if cached, hit := gitCacheGet(repo, cacheKey); hit {
				store.Mu.RUnlock()
				return gitBufferResponse(200, "application/x-git-upload-pack-result", cached)
			}
			plan, err := gitPlanFetch(repo, args.Wants, args.Haves, args.Shallow, args.Depth)
			var buf []byte
			if err == nil {
				buf, err = gitV2FetchResponse(plan)
			}
			store.Mu.RUnlock()
			if err != nil {
				return gitJSONResponse(500, err.Error())
			}
			gitCachePut(store, repo, cacheKey, buf)
			return gitBufferResponse(200, "application/x-git-upload-pack-result", buf)
		}
		return gitJSONResponse(400, "unknown command "+command)
	}
	if wantsV2 {
		// A v2 request without a command line is malformed.
		return gitJSONResponse(400, "malformed pkt-line request")
	}
	// v0 fetch: want/have/done lines in two flush-separated sections.
	args := gitParseFetchArgs(lines)
	cacheKey := gitFetchCacheKey("0", args)
	store.Mu.RLock()
	if cached, hit := gitCacheGet(repo, cacheKey); hit {
		store.Mu.RUnlock()
		return gitBufferResponse(200, "application/x-git-upload-pack-result", cached)
	}
	plan, err := gitPlanFetch(repo, args.Wants, args.Haves, args.Shallow, args.Depth)
	var buf []byte
	if err == nil {
		buf, err = gitV0FetchResponse(plan)
	}
	store.Mu.RUnlock()
	if err != nil {
		return gitJSONResponse(500, err.Error())
	}
	gitCachePut(store, repo, cacheKey, buf)
	return gitBufferResponse(200, "application/x-git-upload-pack-result", buf)
}

func gitV2FetchResponse(plan *gitFetchPlan) ([]byte, error) {
	items := []gitPktItem{gitPktStr("acknowledgments\n")}
	if len(plan.Acks) > 0 {
		for _, ack := range plan.Acks {
			items = append(items, gitPktStr("ACK "+ack+"\n"))
		}
		items = append(items, gitPktStr("ready\n"))
	} else {
		items = append(items, gitPktStr("NAK\n"))
	}
	items = append(items, gitPktDelimItem)
	if len(plan.Shallow) > 0 || len(plan.Unshallow) > 0 {
		items = append(items, gitPktStr("shallow-info\n"))
		for _, oid := range plan.Shallow {
			items = append(items, gitPktStr("shallow "+oid+"\n"))
		}
		for _, oid := range plan.Unshallow {
			items = append(items, gitPktStr("unshallow "+oid+"\n"))
		}
		items = append(items, gitPktDelimItem)
	}
	items = append(items, gitPktStr("packfile\n"))
	pack := gitWritePack(plan.Objects)
	for _, framed := range gitSidebandPack(pack) {
		items = append(items, gitPktData(framed))
	}
	items = append(items, gitPktFlushItem)
	return gitEncodePktLines(items)
}

func gitV0FetchResponse(plan *gitFetchPlan) ([]byte, error) {
	var items []gitPktItem
	if len(plan.Acks) > 0 {
		for _, ack := range plan.Acks {
			items = append(items, gitPktStr("ACK "+ack+"\n"))
		}
	} else {
		items = append(items, gitPktStr("NAK\n"))
	}
	for _, oid := range plan.Shallow {
		items = append(items, gitPktStr("shallow "+oid+"\n"))
	}
	for _, oid := range plan.Unshallow {
		items = append(items, gitPktStr("unshallow "+oid+"\n"))
	}
	pack := gitWritePack(plan.Objects)
	for _, framed := range gitSidebandPack(pack) {
		items = append(items, gitPktData(framed))
	}
	items = append(items, gitPktFlushItem)
	return gitEncodePktLines(items)
}

// ---------------------------------------------------------------------------------------------
// receive-pack (v0)
// ---------------------------------------------------------------------------------------------

type gitPushCommand struct {
	OldOID string
	NewOID string
	Ref    string
}

type gitRefReport struct {
	Ref    string
	OK     bool
	Reason string
}

func gitReceiveReport(unpack string, reports []gitRefReport) gitResponse {
	var items []gitPktItem
	pushLine := func(line string) {
		framed := make([]byte, len(line)+1)
		framed[0] = 1
		copy(framed[1:], line)
		items = append(items, gitPktData(framed))
	}
	pushLine("unpack " + unpack + "\n")
	for _, report := range reports {
		if report.OK {
			pushLine("ok " + report.Ref + "\n")
		} else {
			pushLine("ng " + report.Ref + " " + report.Reason + "\n")
		}
	}
	items = append(items, gitPktFlushItem)
	buf, err := gitEncodePktLines(items)
	if err != nil {
		return gitJSONResponse(500, err.Error())
	}
	return gitBufferResponse(200, "application/x-git-receive-pack-result", buf)
}

func gitValidRefName(ref string) bool {
	if !strings.HasPrefix(ref, "refs/") {
		return false
	}
	if len([]rune(ref)) > 200 {
		return false
	}
	for _, c := range ref {
		if unicode.IsSpace(c) || c < 0x20 || strings.ContainsRune("~^:?*[\\", c) {
			return false
		}
	}
	if strings.Contains(ref, "..") || strings.HasSuffix(ref, "/") || strings.HasSuffix(ref, ".lock") {
		return false
	}
	for _, segment := range strings.Split(ref, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
	}
	return true
}

func gitHandleReceivePack(store *Store, repo *Repo, body []byte) gitResponse {
	ok, packets, consumed := gitDecodePacketsUntilFlush(body)
	if !ok {
		return gitJSONResponse(400, "malformed pkt-line request")
	}
	var commands []gitPushCommand
	for _, packet := range packets {
		if packet.Kind != "data" {
			continue
		}
		text := strings.TrimSuffix(string(packet.Payload), "\n")
		commandText := text
		if nul := strings.IndexByte(text, 0); nul >= 0 {
			commandText = text[:nul]
		}
		parts := strings.Split(commandText, " ")
		if len(parts) != 3 {
			return gitJSONResponse(400, "malformed push command")
		}
		oldOID, newOID, ref := parts[0], parts[1], parts[2]
		if !gitIsObjectID(oldOID) || !gitIsObjectID(newOID) {
			return gitJSONResponse(400, "malformed push command")
		}
		commands = append(commands, gitPushCommand{OldOID: oldOID, NewOID: newOID, Ref: ref})
	}
	if len(commands) == 0 {
		return gitJSONResponse(400, "no push commands")
	}
	packBytes := body[consumed:]
	lookup := func(oid string) *GitObject { return GetObject(repo, oid) }

	// Unpack first: a bad pack refuses the whole push before any ref moves.
	// Parsing (inflate + delta resolution against repo objects) runs under
	// the read lock; storing objects and moving refs runs under the write
	// lock below.
	updates := false
	for _, command := range commands {
		if command.NewOID != gitZeroOID {
			updates = true
			break
		}
	}
	var parsed *gitPackReadResult
	if len(packBytes) > 0 {
		p, err := func() (*gitPackReadResult, error) {
			store.Mu.RLock()
			defer store.Mu.RUnlock()
			return gitParsePack(packBytes, lookup, true)
		}()
		if err != nil {
			return gitReceiveReport(err.Error(), nil)
		}
		if !p.ChecksumOk {
			return gitReceiveReport("pack trailer does not match its bytes", nil)
		}
		parsed = p
	}

	store.Mu.Lock()
	defer store.Mu.Unlock()

	if parsed != nil {
		// gitParsePack already recomputed every object's SHA-1; store by
		// that oid directly instead of re-hashing each object.
		for oid, object := range parsed.Objects {
			if _, exists := repo.Objects[oid]; !exists {
				repo.Objects[oid] = object
			}
		}
	} else if updates {
		// No pack at all: lenient — accept only when every new object is
		// already held.
		missing := false
		for _, command := range commands {
			if command.NewOID != gitZeroOID && lookup(command.NewOID) == nil {
				missing = true
				break
			}
		}
		if missing {
			return gitReceiveReport("unpack error: no pack supplied", nil)
		}
	}

	// Per-ref decisions, judged against the pre-push refs; ok refs move, ng
	// refs do not.
	var reports []gitRefReport
	for _, command := range commands {
		current, hasCurrent := repo.Refs[command.Ref]
		if !gitValidRefName(command.Ref) {
			reports = append(reports, gitRefReport{Ref: command.Ref, OK: false, Reason: "invalid ref name"})
			continue
		}
		if command.NewOID == gitZeroOID {
			if !hasCurrent {
				reports = append(reports, gitRefReport{Ref: command.Ref, OK: false, Reason: "no such ref"})
			} else if command.OldOID != current {
				reports = append(reports, gitRefReport{Ref: command.Ref, OK: false, Reason: "non-fast-forward"})
			} else {
				delete(repo.Refs, command.Ref)
				repo.RefsVersion++
				reports = append(reports, gitRefReport{Ref: command.Ref, OK: true})
			}
			continue
		}
		if command.OldOID == gitZeroOID {
			if hasCurrent {
				reports = append(reports, gitRefReport{Ref: command.Ref, OK: false, Reason: "already exists"})
			} else if lookup(command.NewOID) == nil {
				reports = append(reports, gitRefReport{Ref: command.Ref, OK: false, Reason: "object not found"})
			} else {
				repo.Refs[command.Ref] = command.NewOID
				repo.RefsVersion++
				reports = append(reports, gitRefReport{Ref: command.Ref, OK: true})
			}
			continue
		}
		if !hasCurrent {
			reports = append(reports, gitRefReport{Ref: command.Ref, OK: false, Reason: "no such ref"})
		} else if command.OldOID != current {
			reports = append(reports, gitRefReport{Ref: command.Ref, OK: false, Reason: "non-fast-forward"})
		} else if command.NewOID == current {
			reports = append(reports, gitRefReport{Ref: command.Ref, OK: true})
		} else if lookup(command.NewOID) == nil {
			reports = append(reports, gitRefReport{Ref: command.Ref, OK: false, Reason: "object not found"})
		} else if ff, err := gitIsFastForward(lookup, current, command.NewOID); err != nil {
			return gitJSONResponse(500, err.Error())
		} else if ff {
			repo.Refs[command.Ref] = command.NewOID
			repo.RefsVersion++
			reports = append(reports, gitRefReport{Ref: command.Ref, OK: true})
		} else {
			reports = append(reports, gitRefReport{Ref: command.Ref, OK: false, Reason: "non-fast-forward"})
		}
	}
	return gitReceiveReport("ok", reports)
}

// gitIsFastForward reports whether updating current to next is a
// fast-forward (or not a commit update at all).
func gitIsFastForward(lookup gitObjectLookup, current, next string) (bool, error) {
	currentCommit, err := gitResolveToCommit(lookup, current)
	if err != nil {
		return false, err
	}
	nextCommit, err := gitResolveToCommit(lookup, next)
	if err != nil {
		return false, err
	}
	if currentCommit == "" || nextCommit == "" {
		return true, nil
	}
	return gitIsAncestor(lookup, currentCommit, nextCommit), nil
}
