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

package machoplist

import (
	"bytes"
	"debug/macho"
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// testPlist is shaped like the release templates: an XML declaration, the
// Apple DOCTYPE and a dict.
const testPlist = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>CFBundleIdentifier</key>
    <string>dev.llmkube.machoplist-test</string>
    <key>CFBundleName</key>
    <string>machoplist test</string>
    <key>CFBundleExecutable</key>
    <string>hello</string>
    <key>CFBundleInfoDictionaryVersion</key>
    <string>6.0</string>
    <key>CFBundleShortVersionString</key>
    <string>1.2.3</string>
    <key>CFBundleVersion</key>
    <string>1.2.3</string>
    <key>LSMinimumSystemVersion</key>
    <string>13.0</string>
</dict>
</plist>
`

// helloSource prints its version for --version so the darwin test can prove
// the injected, re-signed binary still executes.
const helloSource = `package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "--version" {
		fmt.Println("hello 1.2.3")
		return
	}
	fmt.Println("hello")
}
`

// target is one darwin binary flavor the tests build. stripped mirrors the
// release ldflags (-s -w); the unstripped build keeps a full symbol table, so
// the n_sect renumbering is exercised against real N_SECT symbols.
type target struct {
	arch     string
	stripped bool
}

func (tg target) String() string {
	if tg.stripped {
		return tg.arch + "-stripped"
	}
	return tg.arch + "-symbols"
}

var allTargets = []target{
	{arch: "arm64", stripped: true},
	{arch: "amd64", stripped: true},
	{arch: "arm64", stripped: false},
	{arch: "amd64", stripped: false},
}

var (
	buildRoot  string
	buildMu    sync.Mutex
	buildCache = map[target]string{}
)

func TestMain(m *testing.M) {
	code := m.Run()
	if buildRoot != "" {
		_ = os.RemoveAll(buildRoot)
	}
	os.Exit(code)
}

// darwinBinary builds the hello program for the target with the internal
// linker (CGO_ENABLED=0, as the release does) once per test binary and
// returns a fresh copy of its bytes.
func darwinBinary(t testing.TB, tg target) []byte {
	t.Helper()
	path := darwinBinaryPath(t, tg)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}

func darwinBinaryPath(t testing.TB, tg target) string {
	t.Helper()
	goTool, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go tool not available; cannot build a darwin test binary")
	}
	buildMu.Lock()
	defer buildMu.Unlock()
	if p, ok := buildCache[tg]; ok {
		return p
	}
	if buildRoot == "" {
		buildRoot, err = os.MkdirTemp("", "machoplist-test-")
		if err != nil {
			t.Fatalf("mkdtemp: %v", err)
		}
	}
	src := filepath.Join(buildRoot, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for name, body := range map[string]string{
		"go.mod":  "module hello\n\ngo 1.24\n",
		"main.go": helloSource,
	} {
		if err := os.WriteFile(filepath.Join(src, name), []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	out := filepath.Join(buildRoot, "hello-"+tg.String())
	args := []string{"build", "-trimpath", "-o", out}
	if tg.stripped {
		args = append(args, "-ldflags=-s -w")
	}
	args = append(args, ".")
	cmd := exec.Command(goTool, args...)
	cmd.Dir = src
	cmd.Env = append(os.Environ(), "GOOS=darwin", "GOARCH="+tg.arch, "CGO_ENABLED=0", "GOWORK=off", "GOFLAGS=")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build %s: %v\n%s", tg, err, b)
	}
	buildCache[tg] = out
	return out
}

func parse(t testing.TB, b []byte) *macho.File {
	t.Helper()
	f, err := macho.NewFile(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("debug/macho cannot parse the binary: %v", err)
	}
	return f
}

func segmentByName(f *macho.File, name string) *macho.Segment {
	for _, l := range f.Loads {
		if s, ok := l.(*macho.Segment); ok && s.Name == name {
			return s
		}
	}
	return nil
}

func sectionKey(s *macho.Section) string { return s.Seg + "," + s.Name }

func TestInjectAddsInfoPlistSection(t *testing.T) {
	plist := []byte(testPlist)
	for _, tg := range allTargets {
		t.Run(tg.String(), func(t *testing.T) {
			orig := darwinBinary(t, tg)
			pristine := bytes.Clone(orig)

			out, err := Inject(orig, plist)
			if err != nil {
				t.Fatalf("Inject: %v", err)
			}
			if !bytes.Equal(orig, pristine) {
				t.Fatal("Inject modified its input slice")
			}
			if len(out) != len(orig) {
				t.Fatalf("output is %d bytes, input %d: the plist must go into existing header padding", len(out), len(orig))
			}

			before, after := parse(t, orig), parse(t, out)

			sect := after.Section("__info_plist")
			if sect == nil {
				t.Fatal("no __info_plist section after Inject")
			}
			if sect.Seg != "__TEXT" {
				t.Fatalf("__info_plist is in segment %q, want __TEXT", sect.Seg)
			}
			if sect.Size != uint64(len(plist)) {
				t.Fatalf("__info_plist size = %d, want %d", sect.Size, len(plist))
			}
			data, err := sect.Data()
			if err != nil {
				t.Fatalf("read __info_plist: %v", err)
			}
			if !bytes.Equal(data, plist) {
				t.Fatalf("__info_plist contents differ from the plist:\n%q", data)
			}
			if got := out[sect.Offset : uint64(sect.Offset)+sect.Size]; !bytes.Equal(got, plist) {
				t.Fatal("plist bytes are not at the section's file offset")
			}

			text := segmentByName(after, "__TEXT")
			textBefore := segmentByName(before, "__TEXT")
			if sect.Addr != text.Addr+uint64(sect.Offset)-text.Offset {
				t.Fatalf("__info_plist addr %#x does not map its file offset %#x inside __TEXT (addr %#x, offset %#x)",
					sect.Addr, sect.Offset, text.Addr, text.Offset)
			}
			headerEnd := uint64(32) + uint64(after.Cmdsz)
			if uint64(sect.Offset) < headerEnd {
				t.Fatalf("plist at offset %d overlaps the load commands ending at %d", sect.Offset, headerEnd)
			}
			if uint64(sect.Offset)+sect.Size > text.Offset+text.Filesz {
				t.Fatal("plist extends past the end of __TEXT")
			}

			if after.Ncmd != before.Ncmd {
				t.Fatalf("ncmds = %d, want %d (a section is not a load command)", after.Ncmd, before.Ncmd)
			}
			if after.Cmdsz != before.Cmdsz+80 {
				t.Fatalf("sizeofcmds = %d, want %d + 80 (one section_64 header)", after.Cmdsz, before.Cmdsz)
			}
			if text.Nsect != textBefore.Nsect+1 {
				t.Fatalf("__TEXT nsects = %d, want %d", text.Nsect, textBefore.Nsect+1)
			}
			if text.Len != textBefore.Len+80 {
				t.Fatalf("__TEXT cmdsize = %d, want %d + 80", text.Len, textBefore.Len)
			}

			assertSectionsUnchanged(t, before, after)
			assertOtherLoadCommandsUnchanged(t, before, after)
			assertSymbolsKeepTheirSections(t, before, after)
		})
	}
}

// assertSectionsUnchanged checks every section the linker wrote keeps its
// address, file offset, size and bytes.
func assertSectionsUnchanged(t *testing.T, before, after *macho.File) {
	t.Helper()
	got := map[string]*macho.Section{}
	for _, s := range after.Sections {
		got[sectionKey(s)] = s
	}
	if len(after.Sections) != len(before.Sections)+1 {
		t.Fatalf("section count = %d, want %d", len(after.Sections), len(before.Sections)+1)
	}
	for _, b := range before.Sections {
		a, ok := got[sectionKey(b)]
		if !ok {
			t.Fatalf("section %s disappeared", sectionKey(b))
		}
		if a.Addr != b.Addr || a.Offset != b.Offset || a.Size != b.Size || a.Flags != b.Flags {
			t.Fatalf("section %s header changed: addr %#x->%#x offset %d->%d size %d->%d",
				sectionKey(b), b.Addr, a.Addr, b.Offset, a.Offset, b.Size, a.Size)
		}
		if b.Offset == 0 {
			continue // zero-fill: no file bytes
		}
		bd, err := b.Data()
		if err != nil {
			t.Fatalf("read %s before: %v", sectionKey(b), err)
		}
		ad, err := a.Data()
		if err != nil {
			t.Fatalf("read %s after: %v", sectionKey(b), err)
		}
		if !bytes.Equal(ad, bd) {
			t.Fatalf("section %s contents changed", sectionKey(b))
		}
	}
}

// assertOtherLoadCommandsUnchanged checks that only __TEXT's segment command
// changed: the others moved down by one section header but kept their bytes
// (file offsets in __LINKEDIT, the code signature, dylib names).
func assertOtherLoadCommandsUnchanged(t *testing.T, before, after *macho.File) {
	t.Helper()
	if len(after.Loads) != len(before.Loads) {
		t.Fatalf("load command count changed: %d -> %d", len(before.Loads), len(after.Loads))
	}
	for i := range before.Loads {
		if s, ok := before.Loads[i].(*macho.Segment); ok && s.Name == "__TEXT" {
			continue
		}
		if !bytes.Equal(before.Loads[i].Raw(), after.Loads[i].Raw()) {
			t.Fatalf("load command %d (cmd %#x) changed", i, binary.LittleEndian.Uint32(before.Loads[i].Raw()))
		}
	}
}

// assertSymbolsKeepTheirSections checks the n_sect renumbering: inserting a
// section into __TEXT shifts the global ordinal of every later section, so a
// symbol in __DATA must still name __DATA afterwards.
func assertSymbolsKeepTheirSections(t *testing.T, before, after *macho.File) {
	t.Helper()
	if before.Symtab == nil {
		return
	}
	if after.Symtab == nil || len(after.Symtab.Syms) != len(before.Symtab.Syms) {
		t.Fatal("symbol table changed size")
	}
	name := func(f *macho.File, sect uint8) string {
		if sect == 0 {
			return "NO_SECT"
		}
		if int(sect) > len(f.Sections) {
			return fmt.Sprintf("out-of-range(%d)", sect)
		}
		return sectionKey(f.Sections[sect-1])
	}
	checked := 0
	for i, b := range before.Symtab.Syms {
		a := after.Symtab.Syms[i]
		if bn, an := name(before, b.Sect), name(after, a.Sect); bn != an {
			t.Fatalf("symbol %q moved from %s to %s", b.Name, bn, an)
		}
		if b.Sect != 0 {
			checked++
		}
	}
	t.Logf("%d section-relative symbols keep their section", checked)
}

func TestInjectRejects(t *testing.T) {
	arm64 := target{arch: "arm64", stripped: true}
	amd64 := target{arch: "amd64", stripped: true}
	plist := []byte(testPlist)

	cases := []struct {
		name    string
		binary  func(t *testing.T) []byte
		plist   []byte
		wantErr string
	}{
		{
			name: "second injection",
			binary: func(t *testing.T) []byte {
				once, err := Inject(darwinBinary(t, arm64), plist)
				if err != nil {
					t.Fatalf("first Inject: %v", err)
				}
				return once
			},
			plist:   plist,
			wantErr: "already has an __info_plist section",
		},
		{
			name:    "plist larger than the header padding",
			binary:  func(t *testing.T) []byte { return darwinBinary(t, arm64) },
			plist:   bigPlist(8192),
			wantErr: "not enough header padding",
		},
		{
			name:    "shell script",
			binary:  func(*testing.T) []byte { return []byte("#!/bin/sh\necho hello\n") },
			plist:   plist,
			wantErr: "not a Mach-O",
		},
		{
			name:    "truncated input",
			binary:  func(*testing.T) []byte { return []byte{0xcf, 0xfa} },
			plist:   plist,
			wantErr: "not a Mach-O",
		},
		{
			name: "universal binary",
			binary: func(t *testing.T) []byte {
				return fatBinary(t, darwinBinary(t, arm64), darwinBinary(t, amd64))
			},
			plist:   plist,
			wantErr: "universal (fat) binary",
		},
		{
			name: "32-bit Mach-O",
			binary: func(*testing.T) []byte {
				b := make([]byte, 64)
				binary.LittleEndian.PutUint32(b, 0xfeedface)
				return b
			},
			plist:   plist,
			wantErr: "32-bit Mach-O",
		},
		{
			name: "unsupported CPU type",
			binary: func(t *testing.T) []byte {
				b := darwinBinary(t, arm64)
				binary.LittleEndian.PutUint32(b[4:], 0x01000012) // CPU_TYPE_POWERPC64
				return b
			},
			plist:   plist,
			wantErr: "unsupported CPU type",
		},
		{
			name: "not an executable",
			binary: func(t *testing.T) []byte {
				b := darwinBinary(t, arm64)
				binary.LittleEndian.PutUint32(b[12:], 6) // MH_DYLIB
				return b
			},
			plist:   plist,
			wantErr: "not an executable",
		},
		{
			name: "non-zero header padding",
			binary: func(t *testing.T) []byte {
				b := darwinBinary(t, arm64)
				f := parse(t, b)
				first := firstSectionOffset(f)
				b[first-1] = 0xff
				return b
			},
			plist:   plist,
			wantErr: "header padding is not zero",
		},
		{
			name:    "plist without an XML declaration",
			binary:  func(t *testing.T) []byte { return darwinBinary(t, arm64) },
			plist:   []byte("<plist version=\"1.0\"><dict/></plist>"),
			wantErr: "does not start with <?xml",
		},
		{
			name:    "XML that is not a plist",
			binary:  func(t *testing.T) []byte { return darwinBinary(t, arm64) },
			plist:   []byte("<?xml version=\"1.0\"?>\n<dict><key>a</key></dict>\n"),
			wantErr: "root element is <dict>, want <plist>",
		},
		{
			name:    "unterminated plist",
			binary:  func(t *testing.T) []byte { return darwinBinary(t, arm64) },
			plist:   []byte("<?xml version=\"1.0\"?>\n<plist version=\"1.0\"><dict><key>a</key>\n"),
			wantErr: "not well-formed XML",
		},
		{
			name:    "empty plist",
			binary:  func(t *testing.T) []byte { return darwinBinary(t, arm64) },
			plist:   nil,
			wantErr: "does not start with <?xml",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := tc.binary(t)
			pristine := bytes.Clone(in)
			out, err := Inject(in, tc.plist)
			if err == nil {
				t.Fatalf("Inject succeeded (%d bytes out), want error containing %q", len(out), tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %q, want it to contain %q", err, tc.wantErr)
			}
			if out != nil {
				t.Fatal("Inject returned output alongside an error")
			}
			if !bytes.Equal(in, pristine) {
				t.Fatal("Inject modified its input on failure")
			}
		})
	}
}

// TestInjectLeavesRoomForCodeSignatureCommand pins the space reservation: the
// Go linker signs darwin/arm64 binaries but not darwin/amd64 ones, so an amd64
// binary has no LC_CODE_SIGNATURE and codesign must append one (16 bytes) in
// the same header padding. Even the largest plist Inject accepts must leave
// that room between the load commands and the plist.
func TestInjectLeavesRoomForCodeSignatureCommand(t *testing.T) {
	for _, tg := range []target{{arch: "amd64", stripped: true}, {arch: "arm64", stripped: true}} {
		t.Run(tg.String(), func(t *testing.T) {
			orig := darwinBinary(t, tg)
			hasSig := hasLoadCommand(parse(t, orig), 0x1d) // LC_CODE_SIGNATURE
			var largest []byte
			for size := 256; size < 8192; size += 8 {
				out, err := Inject(orig, bigPlist(size))
				if err != nil {
					if !strings.Contains(err.Error(), "not enough header padding") {
						t.Fatalf("size %d: unexpected error %v", size, err)
					}
					break
				}
				largest = out
			}
			if largest == nil {
				t.Fatal("no plist size was accepted")
			}
			f := parse(t, largest)
			sect := f.Section("__info_plist")
			gap := int64(sect.Offset) - int64(32+f.Cmdsz)
			want := int64(0)
			if !hasSig {
				want = 16
			}
			t.Logf("LC_CODE_SIGNATURE present=%v, largest plist %d bytes, gap after load commands %d", hasSig, sect.Size, gap)
			if gap < want {
				t.Fatalf("gap between load commands and plist = %d bytes, want >= %d", gap, want)
			}
		})
	}
}

func hasLoadCommand(f *macho.File, cmd uint32) bool {
	for _, l := range f.Loads {
		if binary.LittleEndian.Uint32(l.Raw()) == cmd {
			return true
		}
	}
	return false
}

func firstSectionOffset(f *macho.File) uint32 {
	first := ^uint32(0)
	for _, s := range f.Sections {
		if s.Offset != 0 && s.Offset < first {
			first = s.Offset
		}
	}
	return first
}

// bigPlist returns a well-formed plist of exactly size bytes.
func bigPlist(size int) []byte {
	head := "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<plist version=\"1.0\">\n<dict>\n<key>Pad</key>\n<string>"
	tail := "</string>\n</dict>\n</plist>\n"
	n := size - len(head) - len(tail)
	if n < 0 {
		n = 0
	}
	return []byte(head + strings.Repeat("x", n) + tail)
}

// fatBinary assembles a universal binary the way lipo lays one out: a
// big-endian fat header, one fat_arch per slice, slices aligned to 2^14.
func fatBinary(t *testing.T, slices ...[]byte) []byte {
	t.Helper()
	const align = 14
	var hdr bytes.Buffer
	be := binary.BigEndian
	_ = binary.Write(&hdr, be, uint32(0xcafebabe))
	_ = binary.Write(&hdr, be, uint32(len(slices))) //nolint:gosec // G115: a handful of test slices
	off := uint32(1 << align)
	var body bytes.Buffer
	for _, s := range slices {
		f := parse(t, s)
		_ = binary.Write(&hdr, be, uint32(f.Cpu))
		_ = binary.Write(&hdr, be, f.SubCpu)
		_ = binary.Write(&hdr, be, off)
		_ = binary.Write(&hdr, be, uint32(len(s))) //nolint:gosec // G115: test binaries are a few MiB
		_ = binary.Write(&hdr, be, uint32(align))
		pad := int(off) - (int(1<<align) + body.Len())
		body.Write(make([]byte, pad))
		body.Write(s)
		off += uint32(len(s)) //nolint:gosec // G115: test binaries are a few MiB
		off = (off + (1<<align - 1)) &^ (1<<align - 1)
	}
	out := make([]byte, 1<<align, 1<<align+body.Len())
	copy(out, hdr.Bytes())
	out = append(out, body.Bytes()...)
	if _, err := macho.NewFatFile(bytes.NewReader(out)); err != nil {
		t.Fatalf("test fixture is not a valid fat file: %v", err)
	}
	return out
}

// FuzzInjectHeader mutates the header and load commands of a real binary:
// Inject must never panic, and when debug/macho accepts the input and Inject
// succeeds, debug/macho must accept the output with the plist in place.
func FuzzInjectHeader(f *testing.F) {
	base := darwinBinary(f, target{arch: "arm64", stripped: true})
	const window = 4096
	f.Add(bytes.Clone(base[:window]))
	f.Fuzz(func(t *testing.T, hdr []byte) {
		if len(hdr) > window {
			hdr = hdr[:window]
		}
		in := bytes.Clone(base)
		copy(in, hdr)
		out, err := Inject(in, []byte(testPlist))
		if err != nil {
			return
		}
		if _, perr := macho.NewFile(bytes.NewReader(in)); perr != nil {
			return
		}
		f, perr := macho.NewFile(bytes.NewReader(out))
		if perr != nil {
			t.Fatalf("Inject accepted a parseable binary but produced an unparseable one: %v", perr)
		}
		sect := f.Section("__info_plist")
		if sect == nil {
			t.Fatal("Inject succeeded without an __info_plist section")
		}
		if data, err := sect.Data(); err != nil || !bytes.Equal(data, []byte(testPlist)) {
			t.Fatalf("__info_plist does not hold the plist (err %v)", err)
		}
	})
}
