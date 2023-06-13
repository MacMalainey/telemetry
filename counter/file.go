// Copyright 2023 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !compiler_bootstrap

package counter

import (
	"bytes"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"golang.org/x/telemetry"
	"golang.org/x/telemetry/internal/mmap"
)

// A file is a counter file.
type file struct {
	// Linked list of all known counters.
	// (Linked list insertion is easy to make lock-free,
	// and we don't want the initial counters incremented
	// by a program to cause significant contention.)
	counters atomic.Pointer[Counter] // head of list
	end      Counter                 // list ends at &end instead of nil

	mu         sync.Mutex
	namePrefix string
	err        error
	meta       string
	current    atomic.Pointer[mappedFile] // can be read without holding mu
}

var defaultFile file

// register ensures that the counter c is registered with the file.
func (f *file) register(c *Counter) {
	debugPrintf("register %s %p\n", c.name, c)

	// If counter is not registered with file, register it.
	// Doing this lazily avoids init-time work
	// as well as any execution cost at all for counters
	// that are not used in a given program.
	wroteNext := false
	for wroteNext || c.next.Load() == nil {
		head := f.counters.Load()
		next := head
		if next == nil {
			next = &f.end
		}
		debugPrintf("register %s next %p\n", c.name, next)
		if !wroteNext {
			if !c.next.CompareAndSwap(nil, next) {
				debugPrintf("register %s cas failed %p\n", c.name, c.next.Load())
				continue
			}
			wroteNext = true
		} else {
			c.next.Store(next)
		}
		if f.counters.CompareAndSwap(head, c) {
			debugPrintf("registered %s %p\n", c.name, f.counters.Load())
			return
		}
		debugPrintf("register %s cas2 failed %p %p\n", c.name, f.counters.Load(), head)
	}
}

// invalidateCounters marks as invalid all the pointers
// held by f's counters (because they point into m),
// and then closes prev.
//
// invalidateCounters cannot be called while holding f.mu,
// because a counter invalidation may call f.lookup.
func (f *file) invalidateCounters() {
	// Mark every counter as needing to refresh its count pointer.
	if head := f.counters.Load(); head != nil {
		for c := head; c != &f.end; c = c.next.Load() {
			c.invalidate()
		}
		for c := head; c != &f.end; c = c.next.Load() {
			c.refresh()
		}
	}
}

// lookup looks up the counter with the given name in the file,
// allocating it if needed, and returns a pointer to the atomic.Uint64
// containing the counter data.
// If the file has not been opened yet, lookup returns nil.
func (f *file) lookup(name string) counterPtr {
	current := f.current.Load()
	if current == nil {
		debugPrintf("lookup %s - no mapped file\n", name)
		return counterPtr{}
	}
	ptr := f.newCounter(name)
	if ptr == nil {
		return counterPtr{}
	}
	return counterPtr{current, ptr}
}

// ErrDisabled is the error returned when telemetry is disabled.
var ErrDisabled = errors.New("counter: disabled by GOTELEMETRY=off")

var (
	errNoBuildInfo = errors.New("counter: missing build info")
	errCorrupt     = errors.New("counter: corrupt counter file")
)

func (f *file) init(begin, end time.Time) {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		f.err = errNoBuildInfo
		return
	}
	if !telemetry.Enabled {
		f.err = ErrDisabled
		return
	}
	dir := telemetry.LocalDir

	if err := os.MkdirAll(dir, 0777); err != nil {
		f.err = err
		return
	}

	goVers := info.GoVersion
	if strings.Contains(goVers, "devel") || strings.Contains(goVers, "-") {
		goVers = "devel"
	}
	prog := info.Path
	if prog == "" {
		prog = strings.TrimSuffix(filepath.Base(os.Args[0]), ".exe")
	}
	prog = filepath.Base(prog)
	progVers := info.Main.Version
	if strings.Contains(progVers, "devel") || strings.Contains(progVers, "-") {
		progVers = "devel"
	}
	f.meta = fmt.Sprintf("TimeBegin: %s\nTimeEnd: %s\nProgram: %s\nVersion: %s\nGoVersion: %s\nGOOS: %s\nGOARCH: %s\n\n",
		begin.Format(time.RFC3339), end.Format(time.RFC3339),
		prog, progVers, goVers, runtime.GOOS, runtime.GOARCH)
	if len(f.meta) > maxMetaLen { // should be impossible for our use
		f.err = fmt.Errorf("metadata too long")
		return
	}
	if progVers != "" {
		progVers = "@" + progVers
	}
	prefix := fmt.Sprintf("%s%s-%s-%s-%s-", prog, progVers, goVers, runtime.GOOS, runtime.GOARCH)
	f.namePrefix = filepath.Join(dir, prefix)
}

// filename returns the name of the file to use for f,
// given the current time now.
// It also returns the time when that name will no longer be valid
// and a new filename should be computed.
func (f *file) filename(now time.Time) (name string, expire time.Time, err error) {
	year, month, day := now.Date()
	begin := time.Date(year, month, day, 0, 0, 0, 0, now.Location())
	incr := fileValidity()
	end := time.Date(year, month, day+incr, 0, 0, 0, 0, now.Location())
	if f.namePrefix == "" && f.err == nil {
		f.init(begin, end)
		debugPrintf("init: %#q, %v", f.namePrefix, f.err)
	}
	if f.err != nil {
		return "", time.Time{}, err
	}

	name = f.namePrefix + now.Format("2006-01-02") + "." + fileVersion + ".count"
	return name, end, nil
}

// fileValidity returns the number of days that a file is valid for.
// It is 7, except for new clients.
func fileValidity() int {
	dir := telemetry.UploadDir
	if c, err := os.ReadDir(dir); err == nil && len(c) > 0 {
		return 7
	}
	dir = telemetry.LocalDir
	if c, err := os.ReadDir(dir); err == nil && len(c) > 0 {
		return 7
	}
	return 8 + rand.Intn(7)
}

// rotate checks to see whether the file f needs to be rotated,
// meaning to start a new counter file with a different date in the name.
// rotate is also used to open the file initially, meaning f.current can be nil.
// In general rotate should be called just once for each file.
// rotate will arrange a timer to call itself again when necessary.
func (f *file) rotate() {
	expire, cleanup := f.rotate1()
	cleanup()
	if !expire.IsZero() {
		// TODO(rsc): Does this do the right thing for laptops closing?
		time.AfterFunc(time.Until(expire), f.rotate)
	}
}

func nop() {}

var counterTime = time.Now // changed for tests

func (f *file) rotate1() (expire time.Time, cleanup func()) {
	f.mu.Lock()
	defer f.mu.Unlock()

	name, expire, err := f.filename(counterTime())
	if err != nil {
		debugPrintf("rotate: %v\n", err)
		return time.Time{}, nop
	}
	if name == "" {
		return time.Time{}, nop
	}

	current := f.current.Load()
	if current != nil && name == current.f.Name() {
		return expire, nop
	}

	if current != nil {
		// TODO(pjw): are these log statements a good idea?
		log.Printf("closing %s", current.f.Name())
		if err := current.f.Close(); err != nil {
			log.Print(err)
		}
		if err := munmap(current.mapping); err != nil {
			log.Print(err)
		}
	}

	m, err := openMapped(name, f.meta, nil)
	if err != nil {
		debugPrintf("rotate: openMapped: %v\n", err)
		if current != nil {
			if v, _, _, _ := current.lookup("counter/rotate-error"); v != nil {
				v.Add(1)
			}
		}
		return expire, nop
	}

	debugPrintf("using %v", m.f.Name())
	f.current.Store(m)
	return expire, f.invalidateCounters
}

func (f *file) newCounter(name string) *atomic.Uint64 {
	v, cleanup := f.newCounter1(name)
	cleanup()
	return v
}

func (f *file) newCounter1(name string) (v *atomic.Uint64, cleanup func()) {
	f.mu.Lock()
	defer f.mu.Unlock()

	current := f.current.Load()
	if current == nil {
		return nil, nop
	}
	debugPrintf("newCounter %s in %s\n", name, current.f.Name())
	if v, _, _, _ := current.lookup(name); v != nil {
		return v, nop
	}
	v, newM, err := current.newCounter(name)
	if err != nil {
		debugPrintf("newCounter %s: %v\n", name, err)
		return nil, nop
	}

	cleanup = nop
	if newM != nil {
		f.current.Store(newM)
		cleanup = f.invalidateCounters
	}
	return v, cleanup
}

var mainCounter = New("counter/main")

func open() {
	debugPrintf("Open")
	mainCounter.Add(1)
	defaultFile.rotate()
}

// A mappedFile is a counter file mmapped into memory.
type mappedFile struct {
	meta      string
	hdrLen    uint32
	zero      [4]byte
	closeOnce sync.Once
	f         *os.File
	mapping   *mmap.Data
}

// exising should be nil the first time this is called for a file,
// and when remapping, should be the previous mappedFile.
func openMapped(name string, meta string, existing *mappedFile) (_ *mappedFile, err error) {
	hdr, err := mappedHeader(meta)
	if err != nil {
		return nil, err
	}

	f, err := os.OpenFile(name, os.O_RDWR|os.O_CREATE, 0666)
	if err != nil {
		return nil, err
	}
	// Note: using local variable m here, not return value,
	// so that reutrn nil, err does not set m = nil and break the code in the defer.
	m := &mappedFile{
		f:    f,
		meta: meta,
	}
	runtime.SetFinalizer(m, (*mappedFile).close)
	defer func() {
		if err != nil {
			m.close()
		}
	}()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}

	// Establish file header and initial data area if not already present.
	if info.Size() < minFileLen {
		if _, err := f.WriteAt(hdr, 0); err != nil {
			return nil, err
		}
		// Write zeros at the end of the file to extend it to minFileLen.
		if _, err := f.WriteAt(m.zero[:], int64(minFileLen-len(m.zero))); err != nil {
			return nil, err
		}
		info, err = f.Stat()
		if err != nil {
			return nil, err
		}
		if info.Size() < minFileLen {
			return nil, fmt.Errorf("counter: writing file did not extend it")
		}
	}

	// Map into memory.
	var mapping mmap.Data
	if existing != nil {
		mapping, err = memmap(f, existing.mapping)
	} else {
		mapping, err = memmap(f, nil)
	}
	if err != nil {
		return nil, err
	}
	m.mapping = &mapping
	if !bytes.HasPrefix(m.mapping.Data, hdr) {
		return nil, fmt.Errorf("counter: header mismatch")
	}
	m.hdrLen = uint32(len(hdr))

	return m, nil
}

const (
	fileVersion = "v1"
	hdrPrefix   = "# telemetry/counter file " + fileVersion + "\n"
	recordUnit  = 32
	maxMetaLen  = 512
	numHash     = 512 // 2kB for hash table
	maxNameLen  = 256
	limitOff    = 0
	hashOff     = 4
	pageSize    = 4096
	minFileLen  = 4096
)

func mappedHeader(meta string) ([]byte, error) {
	if len(meta) > maxMetaLen {
		return nil, fmt.Errorf("counter: metadata too large")
	}
	np := round(len(hdrPrefix), 4)
	n := round(np+4+len(meta), 32)
	hdr := make([]byte, n)
	copy(hdr, hdrPrefix)
	*(*uint32)(unsafe.Pointer(&hdr[np])) = uint32(n)
	copy(hdr[np+4:], meta)
	return hdr, nil
}

func (m *mappedFile) place(limit uint32, name string) (start, end uint32) {
	if limit == 0 {
		// first record in file
		limit = m.hdrLen + hashOff + 4*numHash
	}
	n := round(uint32(16+len(name)), recordUnit)
	start = round(limit, recordUnit) // should already be rounded but just in case
	if start/pageSize != (start+n)/pageSize {
		// bump start to next page
		start = round(limit, pageSize)
	}
	return start, start + n
}

var memmap = mmap.Mmap
var munmap = mmap.Munmap

func (m *mappedFile) close() {
	m.closeOnce.Do(func() {
		if m.mapping != nil {
			munmap(m.mapping)
			m.mapping = nil
		}
		if m.f != nil {
			m.f.Close()
			m.f = nil
		}
	})
}

// hash returns the hash code for name.
// The implementation is FNV-1a.
// This hash function is a fixed detail of the file format.
// It cannot be changed without also changing the file format version.
func hash(name string) uint32 {
	const (
		offset32 = 2166136261
		prime32  = 16777619
	)
	h := uint32(offset32)
	for i := 0; i < len(name); i++ {
		c := name[i]
		h = (h ^ uint32(c)) * prime32
	}
	return (h ^ (h >> 16)) % numHash
}

func (m *mappedFile) load32(off uint32) uint32 {
	if int64(off) >= int64(len(m.mapping.Data)) {
		return 0
	}
	return (*atomic.Uint32)(unsafe.Pointer(&m.mapping.Data[off])).Load()
}

func (m *mappedFile) cas32(off, old, new uint32) bool {
	if int64(off) >= int64(len(m.mapping.Data)) {
		panic("bad cas32") // return false would probably loop
	}
	return (*atomic.Uint32)(unsafe.Pointer(&m.mapping.Data[off])).CompareAndSwap(old, new)
}

func (m *mappedFile) entryAt(off uint32) (name []byte, next uint32, v *atomic.Uint64, ok bool) {
	if off < m.hdrLen+hashOff || int64(off)+16 > int64(len(m.mapping.Data)) {
		return nil, 0, nil, false
	}
	nameLen := m.load32(off+8) & 0x00ffffff
	if nameLen == 0 || int64(off)+16+int64(nameLen) > int64(len(m.mapping.Data)) {
		return nil, 0, nil, false
	}
	name = m.mapping.Data[off+16 : off+16+nameLen]
	next = m.load32(off + 12)
	v = (*atomic.Uint64)(unsafe.Pointer(&m.mapping.Data[off]))
	return name, next, v, true
}

func (m *mappedFile) writeEntryAt(off uint32, name string) (next *atomic.Uint32, v *atomic.Uint64, ok bool) {
	if off < m.hdrLen+hashOff || int64(off)+16+int64(len(name)) > int64(len(m.mapping.Data)) {
		return nil, nil, false
	}
	copy(m.mapping.Data[off+16:], name)
	atomic.StoreUint32((*uint32)(unsafe.Pointer(&m.mapping.Data[off+8])), uint32(len(name))|0xff000000)
	next = (*atomic.Uint32)(unsafe.Pointer(&m.mapping.Data[off+12]))
	v = (*atomic.Uint64)(unsafe.Pointer(&m.mapping.Data[off]))
	return next, v, true
}

func (m *mappedFile) lookup(name string) (v *atomic.Uint64, headOff, head uint32, ok bool) {
	h := hash(name)
	headOff = m.hdrLen + hashOff + h*4
	head = m.load32(headOff)
	off := head
	for off != 0 {
		ename, next, v, ok := m.entryAt(off)
		if !ok {
			return nil, 0, 0, false
		}
		if string(ename) == name {
			return v, headOff, head, true
		}
		off = next
	}
	return nil, headOff, head, true
}

func (m *mappedFile) newCounter(name string) (v *atomic.Uint64, m1 *mappedFile, err error) {
	if len(name) > maxNameLen {
		return nil, nil, fmt.Errorf("counter name too long")
	}
	orig := m
	defer func() {
		if m != orig {
			if err != nil {
				m.close()
			} else {
				m1 = m
			}
		}
	}()

	v, headOff, head, ok := m.lookup(name)
	for !ok {
		// Lookup found an invalid pointer,
		// perhaps because the file has grown larger than the mapping.
		limit := m.load32(m.hdrLen + limitOff)
		if int64(limit) <= int64(len(m.mapping.Data)) {
			// Mapping doesn't need to grow, so lookup found actual corruption.
			debugPrintf("corrupt1\n")
			return nil, nil, errCorrupt
		}
		newM, err := openMapped(m.f.Name(), m.meta, m)
		m.f.Close() // PJW
		if err != nil {
			return nil, nil, err
		}
		if m != orig {
			m.close()
		}
		m = newM
		v, headOff, head, ok = m.lookup(name)
	}
	if v != nil {
		return v, nil, nil
	}

	// Reserve space for new record.
	// We are competing against other programs using the same file,
	// so we use a compare-and-swap on the allocation limit in the header.
	var start, end uint32
	for {
		// Determine where record should end, and grow file if needed.
		limit := m.load32(m.hdrLen + limitOff)
		start, end = m.place(limit, name)
		debugPrintf("place %s at %#x-%#x\n", name, start, end)
		if int64(end) > int64(len(m.mapping.Data)) {
			newM, err := m.extend(end)
			if err != nil {
				return nil, nil, err
			}
			if m != orig {
				m.close()
			}
			m = newM
			continue
		}

		// Attempt to reserve that space for our record.
		if m.cas32(m.hdrLen+limitOff, limit, end) {
			break
		}
	}

	// Write record.
	next, v, ok := m.writeEntryAt(start, name)
	if !ok {
		debugPrintf("corrupt2 %#x+%d vs %#x\n", start, len(name), len(m.mapping.Data))
		return nil, nil, errCorrupt // more likely our math is wrong
	}

	// Link record into hash chain, making sure not to introduce a duplicate.
	// We know name does not appear in the chain starting at head.
	for {
		next.Store(head)
		if m.cas32(headOff, head, start) {
			return v, nil, nil
		}

		// Check new elements in chain for duplicates.
		old := head
		head = m.load32(headOff)
		for off := head; off != old; {
			ename, enext, v, ok := m.entryAt(off)
			if !ok {
				return nil, nil, errCorrupt
			}
			if string(ename) == name {
				next.Store(^uint32(0)) // mark ours as dead
				return v, nil, nil
			}
			off = enext
		}
	}
}

func (m *mappedFile) extend(end uint32) (*mappedFile, error) {
	end = round(end, pageSize)
	info, err := m.f.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() < int64(end) {
		if _, err := m.f.WriteAt(m.zero[:], int64(end)-int64(len(m.zero))); err != nil {
			return nil, err
		}
	}
	newM, err := openMapped(m.f.Name(), m.meta, m)
	m.f.Close()
	return newM, err
}

// round returns x rounded up to the next multiple of unit,
// which must be a power of two.
func round[T int | uint32](x T, unit T) T {
	return (x + unit - 1) &^ (unit - 1)
}
