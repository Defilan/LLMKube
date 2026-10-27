/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package machoplist embeds an Info.plist into a Go-built macOS executable as
// a __TEXT,__info_plist section, without relinking.
//
// macOS reads a bare executable's bundle identity (the name in the Local
// Network privacy prompt, the identifier codesign binds) from that section.
// Apple's linker writes it with -sectcreate, which Go reaches only through
// cgo and an external darwin linker; the release builds darwin binaries on
// Linux with CGO_ENABLED=0, so neither is available (golang/go#28832). This
// package post-processes the internal linker's output instead.
//
// The Go internal linker reserves 4 KiB for the Mach-O header and load
// commands and uses about half of it, so the first section starts well after
// the last load command. Inject appends a section_64 header to the __TEXT
// segment command, shifts the load commands that follow it down by 80 bytes
// into that padding, and writes the plist bytes at the end of the padding,
// just before the first section. Everything the linker placed in the file
// keeps its offset and contents; only the global section ordinals after
// __TEXT shift by one, so the symbol table's n_sect fields are renumbered.
//
// The result must be signed again: the edit invalidates the ad-hoc signature
// the Go linker puts on darwin/arm64 binaries, and macOS kills an arm64
// binary whose signature does not verify. codesign and rcodesign both bind
// the embedded plist when they sign (codesign -dv reports "Info.plist
// entries=N").
package machoplist

import (
	"bytes"
	"encoding/binary"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
)

const (
	magic64    = 0xfeedfacf // MH_MAGIC_64, little-endian
	cigam64    = 0xcffaedfe // MH_CIGAM_64, a big-endian 64-bit Mach-O
	magic32    = 0xfeedface // MH_MAGIC
	cigam32    = 0xcefaedfe // MH_CIGAM
	fatMagic   = 0xcafebabe // FAT_MAGIC, read big-endian
	fatMagic64 = 0xcafebabf // FAT_MAGIC_64, read big-endian

	cpuTypeX86_64 = 0x01000007
	cpuTypeARM64  = 0x0100000c
	mhExecute     = 0x2

	lcSegment64     = 0x19
	lcSymtab        = 0x2
	lcCodeSignature = 0x1d

	headerSize    = 32 // mach_header_64
	segCmdSize    = 72 // segment_command_64 without its sections
	sectHdrSize   = 80 // section_64
	nlistSize     = 16 // nlist_64
	codeSigCmdLen = 16 // linkedit_data_command

	sectionName = "__info_plist"
	segmentName = "__TEXT"
)

// Section types (flags & SECTION_TYPE) that occupy no file bytes.
var zeroFillTypes = map[uint32]bool{0x1: true, 0xc: true, 0x12: true}

var le = binary.LittleEndian

// segment is the part of an LC_SEGMENT_64 command Inject needs.
type segment struct {
	cmdOff   uint64 // file offset of the load command
	name     string
	vmaddr   uint64
	fileoff  uint64
	filesize uint64
	nsects   uint32
}

// layout is what Inject learns from walking the load commands.
type layout struct {
	sizeofcmds  uint32
	segments    []segment
	text        int    // index of __TEXT in segments
	firstSect   uint64 // lowest file offset of any section with file bytes
	symoff      uint32
	nsyms       uint32
	hasCodeSign bool
}

// Inject returns a copy of exe with plist embedded as a __TEXT,__info_plist
// section. exe must be a thin, little-endian 64-bit Mach-O executable for
// arm64 or x86_64 without an existing __info_plist section, with enough zeroed
// header padding for one more section header plus the plist; plist must be a
// well-formed XML property list. The input is never modified, and on error the
// returned slice is nil. The caller must re-sign the result.
func Inject(exe, plist []byte) ([]byte, error) {
	if err := validatePlist(plist); err != nil {
		return nil, err
	}
	if err := checkHeader(exe); err != nil {
		return nil, err
	}
	l, err := walkLoadCommands(exe)
	if err != nil {
		return nil, err
	}
	return inject(exe, plist, l)
}

func checkHeader(b []byte) error {
	if len(b) < headerSize {
		return fmt.Errorf("not a Mach-O file: %d bytes is shorter than a Mach-O header", len(b))
	}
	switch binary.BigEndian.Uint32(b) {
	case fatMagic, fatMagic64:
		return errors.New("universal (fat) binary is not supported: inject into each thin slice before lipo")
	}
	switch le.Uint32(b) {
	case magic64:
	case cigam64:
		return errors.New("big-endian 64-bit Mach-O is not supported")
	case magic32, cigam32:
		return errors.New("32-bit Mach-O is not supported: want a 64-bit arm64 or x86_64 executable")
	default:
		return fmt.Errorf("not a Mach-O file (magic %#08x)", le.Uint32(b))
	}
	if cpu := le.Uint32(b[4:]); cpu != cpuTypeARM64 && cpu != cpuTypeX86_64 {
		return fmt.Errorf("unsupported CPU type %#x: want arm64 or x86_64", cpu)
	}
	if ft := le.Uint32(b[12:]); ft != mhExecute {
		return fmt.Errorf("not an executable (Mach-O filetype %d, want MH_EXECUTE)", ft)
	}
	return nil
}

func walkLoadCommands(b []byte) (layout, error) {
	l := layout{text: -1, firstSect: ^uint64(0)}
	ncmds := le.Uint32(b[16:])
	l.sizeofcmds = le.Uint32(b[20:])
	end := uint64(headerSize) + uint64(l.sizeofcmds)
	if end > uint64(len(b)) {
		return l, fmt.Errorf("malformed Mach-O: load commands end at %d, past the %d-byte file", end, len(b))
	}
	off := uint64(headerSize)
	for i := uint32(0); i < ncmds; i++ {
		if off+8 > end {
			return l, fmt.Errorf("malformed Mach-O: load command %d starts past the load command area", i)
		}
		cmd, size := le.Uint32(b[off:]), uint64(le.Uint32(b[off+4:]))
		if size < 8 || off+size > end {
			return l, fmt.Errorf("malformed Mach-O: load command %d has size %d", i, size)
		}
		switch cmd {
		case lcSegment64:
			if err := l.addSegment(b, off, size); err != nil {
				return l, err
			}
		case lcSymtab:
			if size < 24 {
				return l, errors.New("malformed Mach-O: short LC_SYMTAB")
			}
			l.symoff, l.nsyms = le.Uint32(b[off+8:]), le.Uint32(b[off+12:])
		case lcCodeSignature:
			l.hasCodeSign = true
		}
		off += size
	}
	if l.text < 0 {
		return l, errors.New("no __TEXT segment")
	}
	if uint64(l.symoff)+uint64(l.nsyms)*nlistSize > uint64(len(b)) {
		return l, errors.New("malformed Mach-O: symbol table extends past the end of the file")
	}
	return l, nil
}

func (l *layout) addSegment(b []byte, off, size uint64) error {
	if size < segCmdSize {
		return errors.New("malformed Mach-O: short LC_SEGMENT_64")
	}
	s := segment{
		cmdOff:   off,
		name:     cString(b[off+8 : off+24]),
		vmaddr:   le.Uint64(b[off+24:]),
		fileoff:  le.Uint64(b[off+40:]),
		filesize: le.Uint64(b[off+48:]),
		nsects:   le.Uint32(b[off+64:]),
	}
	if segCmdSize+uint64(s.nsects)*sectHdrSize > size {
		return fmt.Errorf("malformed Mach-O: segment %s declares %d sections that do not fit its command", s.name, s.nsects)
	}
	for j := uint64(0); j < uint64(s.nsects); j++ {
		so := off + segCmdSize + j*sectHdrSize
		if cString(b[so:so+16]) == sectionName {
			return fmt.Errorf("binary already has an %s section (in %s); refusing to add a second", sectionName, cString(b[so+16:so+32]))
		}
		fileOff, flags := uint64(le.Uint32(b[so+48:])), le.Uint32(b[so+64:])
		if fileOff != 0 && !zeroFillTypes[flags&0xff] && fileOff < l.firstSect {
			l.firstSect = fileOff
		}
	}
	if s.name == segmentName {
		l.text = len(l.segments)
	}
	l.segments = append(l.segments, s)
	return nil
}

func inject(b, plist []byte, l layout) ([]byte, error) {
	text := l.segments[l.text]
	if text.fileoff != 0 {
		return nil, fmt.Errorf("__TEXT starts at file offset %d, want 0", text.fileoff)
	}
	if l.firstSect == ^uint64(0) || l.firstSect > text.filesize || l.firstSect > uint64(len(b)) {
		return nil, errors.New("no section with file contents inside __TEXT to bound the header padding")
	}

	cmdsEnd := uint64(headerSize) + uint64(l.sizeofcmds)
	// codesign appends LC_CODE_SIGNATURE in the same padding when the binary
	// has none (the Go linker signs darwin/arm64 only), so keep room for it.
	need := cmdsEnd + sectHdrSize
	if !l.hasCodeSign {
		need += codeSigCmdLen
	}
	size := uint64(len(plist))
	if size > l.firstSect || (l.firstSect-size)&^15 < need {
		return nil, fmt.Errorf("not enough header padding: load commands end at %d and need %d more bytes, "+
			"the first section starts at %d, and the plist is %d bytes (at most %d fit)",
			cmdsEnd, need-cmdsEnd, l.firstSect, size, capacity(l.firstSect, need))
	}
	dataOff := (l.firstSect - size) &^ 15
	if !allZero(b[cmdsEnd:l.firstSect]) {
		return nil, fmt.Errorf("header padding is not zero between offsets %d and %d; refusing to overwrite it", cmdsEnd, l.firstSect)
	}

	out := bytes.Clone(b)

	// Insert the section header after __TEXT's last one, moving the load
	// commands that follow it into the padding.
	insertAt := text.cmdOff + segCmdSize + uint64(text.nsects)*sectHdrSize
	copy(out[insertAt+sectHdrSize:cmdsEnd+sectHdrSize], b[insertAt:cmdsEnd])
	hdr := out[insertAt : insertAt+sectHdrSize]
	clear(hdr)
	copy(hdr[0:16], sectionName)
	copy(hdr[16:32], segmentName)
	le.PutUint64(hdr[32:], text.vmaddr+dataOff) // addr
	le.PutUint64(hdr[40:], size)                // size
	// offset; align, relocs and flags (S_REGULAR) stay 0.
	le.PutUint32(hdr[48:], uint32(dataOff)) //nolint:gosec // G115: dataOff < firstSect, read from a uint32 field

	le.PutUint32(out[text.cmdOff+4:], le.Uint32(b[text.cmdOff+4:])+sectHdrSize) // cmdsize
	le.PutUint32(out[text.cmdOff+64:], text.nsects+1)                           // nsects
	le.PutUint32(out[20:], l.sizeofcmds+sectHdrSize)                            // sizeofcmds

	copy(out[dataOff:], plist)

	if err := renumberSymbols(out, l); err != nil {
		return nil, err
	}
	return out, nil
}

// renumberSymbols shifts n_sect for symbols defined in a section after the
// new one. Section ordinals are 1-based and global across segments, in load
// command order. The Go linker emits no stabs, so only N_SECT entries carry a
// section ordinal.
func renumberSymbols(out []byte, l layout) error {
	newOrdinal := uint32(1)
	for i := 0; i <= l.text; i++ {
		newOrdinal += l.segments[i].nsects
	}
	const nStab, nTypeMask, nSect = 0xe0, 0x0e, 0x0e
	for i := uint64(0); i < uint64(l.nsyms); i++ {
		e := uint64(l.symoff) + i*nlistSize
		ntype, nsect := out[e+4], out[e+5]
		if ntype&nStab != 0 || ntype&nTypeMask != nSect || uint32(nsect) < newOrdinal {
			continue
		}
		if nsect == 0xff {
			return errors.New("section ordinal overflow: the binary already has 255 sections")
		}
		out[e+5] = nsect + 1
	}
	return nil
}

// capacity reports the largest plist that fits, for the padding error: the
// plist starts 16-byte aligned at or after need and ends at firstSect.
func capacity(firstSect, need uint64) uint64 {
	start := (need + 15) &^ 15
	if firstSect < start {
		return 0
	}
	return firstSect - start
}

func validatePlist(p []byte) error {
	if !bytes.HasPrefix(p, []byte("<?xml")) {
		return errors.New("plist does not start with <?xml: want an XML property list")
	}
	d := xml.NewDecoder(bytes.NewReader(p))
	root := ""
	for {
		tok, err := d.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("plist is not well-formed XML: %w", err)
		}
		if se, ok := tok.(xml.StartElement); ok && root == "" {
			root = se.Name.Local
			if root != "plist" {
				return fmt.Errorf("plist root element is <%s>, want <plist>", root)
			}
		}
	}
	if root == "" {
		return errors.New("plist has no root element")
	}
	return nil
}

func allZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

func cString(b []byte) string {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	return string(b)
}
