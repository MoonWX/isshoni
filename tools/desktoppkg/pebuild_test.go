package main

import (
	"bytes"
	"debug/pe"
	"encoding/binary"
	"sort"
	"unicode/utf16"
)

// fakeDLL describes a synthetic PE image for dll-check's tests: the headers and the data directories dll-check
// reads (exports, imports, delay imports, VERSIONINFO, load config), laid out as link.exe and rc.exe write them,
// in one section. No code: the checks never run anything.
type fakeDLL struct {
	machine     uint16 // 0 = AMD64
	pe32        bool   // a 32-bit optional header (image base 0x10000000)
	exe         bool   // no IMAGE_FILE_DLL
	dllChars    uint16 // 0 = DYNAMIC_BASE | HIGH_ENTROPY_VA | NX_COMPAT | GUARD_CF
	exportName  string // "" = isshoni-media.dll
	exports     []string
	forwarded   []string // exports whose function is a forwarder string
	unnamed     int      // extra functions exported by ordinal only
	imports     []string
	delay       []string
	delayVA     bool // delay descriptors with 32-bit addresses (Visual C++ 6) instead of RVAs
	version     *fakeVersion
	loadConfig  bool
	dependFlags uint16
}

type fakeVersion struct {
	file, product [4]uint16
	strings       [][2]string // in order
	table         string      // "" = 040904b0
	byteLengths   bool        // write a String's wValueLength in bytes, as some tools do
}

const (
	fakeImageBase   = 0x180000000
	fakeImageBase32 = 0x10000000
	fakeSecRVA      = 0x1000
	fakeSecOffset   = 0x400
)

// u32 and u16 narrow the fixtures' sizes, counts and offsets, which are a few KiB at most.
func u32(n int) uint32 { return uint32(n) } //nolint:gosec // G115: fixture sizes are a few KiB
func u16(n int) uint16 { return uint16(n) } //nolint:gosec // G115: as u32

// fakeSection is the image's one section while it is built: RVAs are fakeSecRVA + offset. put appends and may move
// the buffer, so a write into it takes the values it needs from put first.
type fakeSection struct{ b []byte }

func (s *fakeSection) rva() uint32 { return fakeSecRVA + u32(len(s.b)) }

func (s *fakeSection) align(n int) {
	for len(s.b)%n != 0 {
		s.b = append(s.b, 0)
	}
}

func (s *fakeSection) put(data []byte) uint32 {
	s.align(4)
	r := s.rva()
	s.b = append(s.b, data...)
	return r
}

func (s *fakeSection) cstring(str string) uint32 { return s.put(append([]byte(str), 0)) }

func (s *fakeSection) at(rva uint32) []byte { return s.b[rva-fakeSecRVA:] }

func le32(vs ...uint32) []byte {
	b := make([]byte, 4*len(vs))
	for i, v := range vs {
		binary.LittleEndian.PutUint32(b[4*i:], v)
	}
	return b
}

func (d fakeDLL) build() []byte {
	var s fakeSection
	s.put(make([]byte, 16)) // a little "code" at the section start, which exports point at
	var dirs [16]pe.DataDirectory

	// Exports: IMAGE_EXPORT_DIRECTORY, then the function, name and ordinal arrays and the strings.
	if len(d.exports)+len(d.forwarded)+d.unnamed > 0 {
		names := append(append([]string(nil), d.exports...), d.forwarded...)
		sort.Strings(names)
		forwarded := map[string]bool{}
		for _, f := range d.forwarded {
			forwarded[f] = true
		}
		nFuncs := len(names) + d.unnamed
		dirRVA := s.put(make([]byte, 40))
		funcs := s.put(make([]byte, 4*nFuncs))
		nameArr := s.put(make([]byte, 4*len(names)))
		ords := s.put(make([]byte, 2*len(names)))
		exportName := d.exportName
		if exportName == "" {
			exportName = "isshoni-media.dll"
		}
		nameRVA := s.cstring(exportName)
		for i, n := range names {
			fn := uint32(fakeSecRVA)
			if forwarded[n] {
				fn = s.cstring("OTHER." + n) // inside the export directory's range
			}
			str := s.cstring(n)
			binary.LittleEndian.PutUint32(s.at(nameArr+4*u32(i)), str)
			binary.LittleEndian.PutUint16(s.at(ords+2*u32(i)), u16(i))
			binary.LittleEndian.PutUint32(s.at(funcs+4*u32(i)), fn)
		}
		for i := range d.unnamed {
			binary.LittleEndian.PutUint32(s.at(funcs+4*u32(len(names)+i)), fakeSecRVA+4)
		}
		copy(s.at(dirRVA), le32(0, 0, 0, nameRVA, 1, u32(nFuncs), u32(len(names)), funcs, nameArr, ords))
		dirs[dirExport] = pe.DataDirectory{VirtualAddress: dirRVA, Size: s.rva() - dirRVA}
	}

	// Imports: IMAGE_IMPORT_DESCRIPTOR per DLL and a zero one, each with an empty thunk array.
	if len(d.imports) > 0 {
		descs := s.put(make([]byte, 20*(len(d.imports)+1)))
		for i, name := range d.imports {
			thunks := s.put(make([]byte, 8))
			nameRVA := s.cstring(name)
			copy(s.at(descs+20*u32(i)), le32(thunks, 0, 0, nameRVA, thunks))
		}
		dirs[dirImport] = pe.DataDirectory{VirtualAddress: descs, Size: 20 * u32(len(d.imports)+1)}
	}

	// Delay imports: ImgDelayDescr per DLL (dlattrRva, or addresses) and a zero one.
	if len(d.delay) > 0 {
		descs := s.put(make([]byte, 32*(len(d.delay)+1)))
		for i, name := range d.delay {
			hmod := s.put(make([]byte, 8))
			iat := s.put(make([]byte, 8))
			intRVA := s.put(make([]byte, 8))
			attrs, nameRef := uint32(1), s.cstring(name)
			if d.delayVA {
				attrs, nameRef = 0, fakeImageBase32+nameRef
			}
			copy(s.at(descs+32*u32(i)), le32(attrs, nameRef, hmod, iat, intRVA, 0, 0, 0))
		}
		dirs[dirDelayImport] = pe.DataDirectory{VirtualAddress: descs, Size: 32 * u32(len(d.delay)+1)}
	}

	// Load config: IMAGE_LOAD_CONFIG_DIRECTORY64 with Size and DependentLoadFlags.
	if d.loadConfig {
		lc := make([]byte, 0x140)
		binary.LittleEndian.PutUint32(lc, u32(len(lc)))
		binary.LittleEndian.PutUint16(lc[78:], d.dependFlags)
		r := s.put(lc)
		dirs[dirLoadConfig] = pe.DataDirectory{VirtualAddress: r, Size: u32(len(lc))}
	}

	// Resources: type RT_VERSION → name 1 → language 0x409 → the data entry → VS_VERSIONINFO.
	if d.version != nil {
		s.align(4)
		root := s.rva()
		dirEntry := func(id, target uint32) []byte {
			return append(le32(0, 0, 0, 1<<16), le32(id, target)...) // header (one ID entry), then the entry
		}
		blob := d.version.bytes()
		// Offsets relative to root: three directories of 24 bytes, then the 16-byte data entry, then the data.
		s.put(dirEntry(rtVersion, 0x80000000|24))
		s.put(dirEntry(1, 0x80000000|48))
		s.put(dirEntry(0x409, 72))
		dataEntry := s.put(make([]byte, 16))
		dataRVA := s.put(blob)
		copy(s.at(dataEntry), le32(dataRVA, u32(len(blob)), 0, 0))
		dirs[dirResource] = pe.DataDirectory{VirtualAddress: root, Size: s.rva() - root}
	}
	s.align(0x200)

	// Headers: DOS stub with e_lfanew, "PE\0\0", the COFF header, the optional header and one section header.
	var out bytes.Buffer
	dos := make([]byte, 0x40)
	copy(dos, "MZ")
	binary.LittleEndian.PutUint32(dos[0x3c:], 0x40)
	out.Write(dos)
	out.WriteString("PE\x00\x00")
	machine := d.machine
	if machine == 0 {
		machine = pe.IMAGE_FILE_MACHINE_AMD64
	}
	chars := uint16(pe.IMAGE_FILE_EXECUTABLE_IMAGE | pe.IMAGE_FILE_LARGE_ADDRESS_AWARE)
	if !d.exe {
		chars |= fileDLL
	}
	dllChars := d.dllChars
	if dllChars == 0 {
		dllChars = dllDynamicBase | dllHighEntropyVA | dllNXCompat | dllGuardCF
	}
	optSize := uint16(240)
	if d.pe32 {
		optSize = 224
	}
	fh := pe.FileHeader{Machine: machine, NumberOfSections: 1, SizeOfOptionalHeader: optSize, Characteristics: chars}
	_ = binary.Write(&out, binary.LittleEndian, fh)
	sizeOfImage := fakeSecRVA + u32((len(s.b)+0xfff)&^0xfff)
	if d.pe32 {
		oh := pe.OptionalHeader32{Magic: 0x10b, ImageBase: fakeImageBase32, SectionAlignment: 0x1000,
			FileAlignment: 0x200, MajorSubsystemVersion: 6, SizeOfImage: sizeOfImage, SizeOfHeaders: fakeSecOffset,
			Subsystem: 2, DllCharacteristics: dllChars, NumberOfRvaAndSizes: 16, DataDirectory: dirs}
		_ = binary.Write(&out, binary.LittleEndian, oh)
	} else {
		oh := pe.OptionalHeader64{Magic: 0x20b, ImageBase: fakeImageBase, SectionAlignment: 0x1000,
			FileAlignment: 0x200, MajorSubsystemVersion: 6, SizeOfImage: sizeOfImage, SizeOfHeaders: fakeSecOffset,
			Subsystem: 2, DllCharacteristics: dllChars, NumberOfRvaAndSizes: 16, DataDirectory: dirs}
		_ = binary.Write(&out, binary.LittleEndian, oh)
	}
	sh := pe.SectionHeader32{VirtualSize: u32(len(s.b)), VirtualAddress: fakeSecRVA, SizeOfRawData: u32(len(s.b)),
		PointerToRawData: fakeSecOffset, Characteristics: 0x40000040}
	copy(sh.Name[:], ".rdata")
	_ = binary.Write(&out, binary.LittleEndian, sh)
	for out.Len() < fakeSecOffset {
		out.WriteByte(0)
	}
	out.Write(s.b)
	return out.Bytes()
}

// bytes writes VS_VERSIONINFO as rc.exe does: VS_FIXEDFILEINFO, a StringFileInfo with one StringTable, and a
// VarFileInfo with the Translation.
func (v *fakeVersion) bytes() []byte {
	dword := func(hi, lo uint16) uint32 { return uint32(hi)<<16 | uint32(lo) }
	fixed := le32(fixedInfoSig, 0x00010000, dword(v.file[0], v.file[1]), dword(v.file[2], v.file[3]),
		dword(v.product[0], v.product[1]), dword(v.product[2], v.product[3]), 0x3f, 0, 0x40004, 2, 0, 0, 0)
	var strs [][]byte
	for _, kv := range v.strings {
		text := utf16z(kv[1])
		n := u16(len(text) / 2)
		if v.byteLengths {
			n = u16(len(text))
		}
		strs = append(strs, verBlockBytes(kv[0], 1, text, n, nil))
	}
	table := v.table
	if table == "" {
		table = "040904b0"
	}
	sfi := verBlockBytes("StringFileInfo", 1, nil, 0, [][]byte{verBlockBytes(table, 1, nil, 0, strs)})
	translation := []byte{0x09, 0x04, 0xb0, 0x04}
	vfi := verBlockBytes("VarFileInfo", 1, nil, 0, [][]byte{verBlockBytes("Translation", 0, translation, 4, nil)})
	return verBlockBytes("VS_VERSION_INFO", 0, fixed, u16(len(fixed)), [][]byte{sfi, vfi})
}

func utf16z(s string) []byte {
	u := append(utf16.Encode([]rune(s)), 0)
	b := make([]byte, 2*len(u))
	for i, c := range u {
		binary.LittleEndian.PutUint16(b[2*i:], c)
	}
	return b
}

// verBlockBytes writes one block: wLength, wValueLength, wType, the key, padding, the value, padding, the children
// (each starting 4-aligned). wLength covers everything but the padding after the last child.
func verBlockBytes(key string, typ uint16, value []byte, valueLen uint16, children [][]byte) []byte {
	b := make([]byte, 6)
	b = append(b, utf16z(key)...)
	for len(b)%4 != 0 {
		b = append(b, 0)
	}
	b = append(b, value...)
	for _, c := range children {
		for len(b)%4 != 0 {
			b = append(b, 0)
		}
		b = append(b, c...)
	}
	binary.LittleEndian.PutUint16(b[0:], u16(len(b)))
	binary.LittleEndian.PutUint16(b[2:], valueLen)
	binary.LittleEndian.PutUint16(b[4:], typ)
	return b
}
