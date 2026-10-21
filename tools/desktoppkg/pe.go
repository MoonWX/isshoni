package main

import (
	"bytes"
	"debug/pe"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"unicode/utf16"
)

// A PE image read for dll-check: what `dumpbin /exports /dependents /loadconfig` shows and the VERSIONINFO resource,
// read with debug/pe and the data directories it doesn't decode (exports, delay imports, resources, load config). It
// runs anywhere, so a DLL from a CI artifact can be checked on the Mac too.

// Data directory indexes (winnt.h IMAGE_DIRECTORY_ENTRY_*).
const (
	dirExport      = 0
	dirImport      = 1
	dirResource    = 2
	dirLoadConfig  = 10
	dirDelayImport = 13
)

// IMAGE_FILE_* and IMAGE_DLLCHARACTERISTICS_* bits dll-check reads.
const (
	fileDLL           = 0x2000
	dllHighEntropyVA  = 0x0020
	dllDynamicBase    = 0x0040
	dllNXCompat       = 0x0100
	dllGuardCF        = 0x4000
	rtVersion         = 16
	fixedInfoSig      = 0xFEEF04BD
	maxPEStringLength = 4096
)

// peImage is one parsed image.
type peImage struct {
	data      []byte
	file      *pe.File
	machine   uint16
	is64      bool
	isDLL     bool
	dllChars  uint16
	imageBase uint64
	dirs      []pe.DataDirectory
}

// exportInfo is the export directory: the DLL name it records and the exported names.
type exportInfo struct {
	dllName   string
	names     []string
	unnamed   int      // functions exported by ordinal only
	forwarded []string // names whose function is a forwarder string
}

// versionInfo is VERSIONINFO: the binary file and product versions and the first string table.
type versionInfo struct {
	fileVersion    [4]uint16
	productVersion [4]uint16
	strings        map[string]string
	order          []string // string names in file order
}

func parsePE(data []byte) (*peImage, error) {
	f, err := pe.NewFile(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("not a PE image: %w", err)
	}
	img := &peImage{data: data, file: f, machine: f.Machine, isDLL: f.Characteristics&fileDLL != 0}
	switch oh := f.OptionalHeader.(type) {
	case *pe.OptionalHeader64:
		img.is64 = true
		img.dllChars = oh.DllCharacteristics
		img.imageBase = oh.ImageBase
		img.dirs = oh.DataDirectory[:min(int(oh.NumberOfRvaAndSizes), len(oh.DataDirectory))]
	case *pe.OptionalHeader32:
		img.dllChars = oh.DllCharacteristics
		img.imageBase = uint64(oh.ImageBase)
		img.dirs = oh.DataDirectory[:min(int(oh.NumberOfRvaAndSizes), len(oh.DataDirectory))]
	default:
		return nil, errors.New("the PE image has no optional header")
	}
	return img, nil
}

// dir returns data directory i, or a zero one when the image has fewer.
func (p *peImage) dir(i int) pe.DataDirectory {
	if i < len(p.dirs) {
		return p.dirs[i]
	}
	return pe.DataDirectory{}
}

// offset maps an RVA to a file offset with n bytes readable there.
func (p *peImage) offset(rva uint32, n int) (int, error) {
	for _, s := range p.file.Sections {
		size := max(s.VirtualSize, s.Size)
		if rva < s.VirtualAddress || rva-s.VirtualAddress >= size {
			continue
		}
		delta := rva - s.VirtualAddress
		if delta >= s.Size {
			return 0, fmt.Errorf("RVA 0x%x is in %s's uninitialized part", rva, s.Name)
		}
		off := int(s.Offset) + int(delta)
		if n < 0 || off+n > len(p.data) || int64(n) > int64(s.Size-delta) {
			return 0, fmt.Errorf("RVA 0x%x: %d bytes run past %s", rva, n, s.Name)
		}
		return off, nil
	}
	return 0, fmt.Errorf("RVA 0x%x is in no section", rva)
}

func (p *peImage) bytesAt(rva uint32, n int) ([]byte, error) {
	off, err := p.offset(rva, n)
	if err != nil {
		return nil, err
	}
	return p.data[off : off+n], nil
}

func (p *peImage) u16(rva uint32) (uint16, error) {
	b, err := p.bytesAt(rva, 2)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint16(b), nil
}

func (p *peImage) u32(rva uint32) (uint32, error) {
	b, err := p.bytesAt(rva, 4)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint32(b), nil
}

// cstring reads a NUL-terminated ASCII string at rva.
func (p *peImage) cstring(rva uint32) (string, error) {
	off, err := p.offset(rva, 1)
	if err != nil {
		return "", err
	}
	end := bytes.IndexByte(p.data[off:min(len(p.data), off+maxPEStringLength)], 0)
	if end < 0 {
		return "", fmt.Errorf("RVA 0x%x: no string end within %d bytes", rva, maxPEStringLength)
	}
	return string(p.data[off : off+end]), nil
}

// exports reads the export directory. An image without one has no exports.
func (p *peImage) exports() (exportInfo, error) {
	var info exportInfo
	d := p.dir(dirExport)
	if d.VirtualAddress == 0 || d.Size == 0 {
		return info, nil
	}
	hdr, err := p.bytesAt(d.VirtualAddress, 40)
	if err != nil {
		return info, fmt.Errorf("export directory: %w", err)
	}
	le := binary.LittleEndian
	nameRVA := le.Uint32(hdr[12:])
	nFuncs := le.Uint32(hdr[20:])
	nNames := le.Uint32(hdr[24:])
	funcs, names, ords := le.Uint32(hdr[28:]), le.Uint32(hdr[32:]), le.Uint32(hdr[36:])
	if nFuncs > 65536 || nNames > nFuncs {
		return info, fmt.Errorf("export directory: %d functions and %d names", nFuncs, nNames)
	}
	if info.dllName, err = p.cstring(nameRVA); err != nil {
		return info, fmt.Errorf("export directory name: %w", err)
	}
	named := make(map[uint16]bool, nNames)
	for i := range nNames {
		rva, err := p.u32(names + 4*i)
		if err != nil {
			return info, fmt.Errorf("export name %d: %w", i, err)
		}
		name, err := p.cstring(rva)
		if err != nil {
			return info, fmt.Errorf("export name %d: %w", i, err)
		}
		ord, err := p.u16(ords + 2*i)
		if err != nil {
			return info, fmt.Errorf("export ordinal %d: %w", i, err)
		}
		if uint32(ord) >= nFuncs {
			return info, fmt.Errorf("export %s: ordinal index %d is past the %d functions", name, ord, nFuncs)
		}
		named[ord] = true
		info.names = append(info.names, name)
		fn, err := p.u32(funcs + 4*uint32(ord))
		if err != nil {
			return info, fmt.Errorf("export %s: %w", name, err)
		}
		if fn >= d.VirtualAddress && fn < d.VirtualAddress+d.Size {
			info.forwarded = append(info.forwarded, name)
		}
	}
	for i := range nFuncs {
		fn, err := p.u32(funcs + 4*i)
		if err != nil {
			return info, fmt.Errorf("export function %d: %w", i, err)
		}
		if fn != 0 && !named[uint16(i)] {
			info.unnamed++
		}
	}
	return info, nil
}

// imports reads the names of the DLLs in the import directory (static imports, as dumpbin /dependents lists them).
func (p *peImage) imports() ([]string, error) {
	d := p.dir(dirImport)
	if d.VirtualAddress == 0 || d.Size == 0 {
		return nil, nil
	}
	var out []string
	for i := uint32(0); ; i++ {
		desc, err := p.bytesAt(d.VirtualAddress+20*i, 20)
		if err != nil {
			return nil, fmt.Errorf("import descriptor %d: %w", i, err)
		}
		if bytes.Equal(desc, make([]byte, 20)) {
			return out, nil
		}
		name, err := p.cstring(binary.LittleEndian.Uint32(desc[12:]))
		if err != nil {
			return nil, fmt.Errorf("import descriptor %d: %w", i, err)
		}
		out = append(out, name)
		if i > 4096 {
			return nil, errors.New("import directory: no end")
		}
	}
}

// delayImports reads the names of the delay-loaded DLLs (ImgDelayDescr, delayimp.h): RVAs when grAttrs has
// dlattrRva, else (Visual C++ 6) addresses.
func (p *peImage) delayImports() ([]string, error) {
	d := p.dir(dirDelayImport)
	if d.VirtualAddress == 0 || d.Size == 0 {
		return nil, nil
	}
	var out []string
	for i := uint32(0); ; i++ {
		desc, err := p.bytesAt(d.VirtualAddress+32*i, 32)
		if err != nil {
			return nil, fmt.Errorf("delay import descriptor %d: %w", i, err)
		}
		attrs, nameRef := binary.LittleEndian.Uint32(desc[0:]), binary.LittleEndian.Uint32(desc[4:])
		if nameRef == 0 {
			return out, nil
		}
		rva := nameRef
		if attrs&1 == 0 {
			// A 32-bit address: only 32-bit images (whose base fits) ever had these.
			base, ok := uint32ImageBase(p.imageBase)
			if p.is64 || !ok || nameRef < base {
				return nil, fmt.Errorf("delay import descriptor %d: an address-based descriptor in this image", i)
			}
			rva = nameRef - base
		}
		name, err := p.cstring(rva)
		if err != nil {
			return nil, fmt.Errorf("delay import descriptor %d: %w", i, err)
		}
		out = append(out, name)
		if i > 4096 {
			return nil, errors.New("delay import directory: no end")
		}
	}
}

// uint32ImageBase is a 32-bit image's base address.
func uint32ImageBase(base uint64) (uint32, bool) {
	if base > 0xFFFFFFFF {
		return 0, false
	}
	return uint32(base), true
}

// dependentLoadFlags reads IMAGE_LOAD_CONFIG_DIRECTORY64.DependentLoadFlags (/DEPENDENTLOADFLAG); ok is false when the
// image has no load config, or one too short to hold the field, or is not 64-bit.
func (p *peImage) dependentLoadFlags() (flags uint16, ok bool, err error) {
	d := p.dir(dirLoadConfig)
	if d.VirtualAddress == 0 || d.Size == 0 || !p.is64 {
		return 0, false, nil
	}
	size, err := p.u32(d.VirtualAddress)
	if err != nil {
		return 0, false, fmt.Errorf("load config: %w", err)
	}
	const fieldOffset = 78 // after Size … CSDVersion in IMAGE_LOAD_CONFIG_DIRECTORY64
	if size < fieldOffset+2 {
		return 0, false, nil
	}
	v, err := p.u16(d.VirtualAddress + fieldOffset)
	if err != nil {
		return 0, false, fmt.Errorf("load config: %w", err)
	}
	return v, true, nil
}

// versionResource finds the first RT_VERSION resource and parses it; nil without one.
func (p *peImage) versionResource() (*versionInfo, error) {
	d := p.dir(dirResource)
	if d.VirtualAddress == 0 || d.Size == 0 {
		return nil, nil
	}
	root := d.VirtualAddress
	// entry finds the entry with id in the directory at offset off (relative to root); any=true takes the first.
	entry := func(off uint32, id uint32, any bool) (uint32, bool, error) {
		hdr, err := p.bytesAt(root+off, 16)
		if err != nil {
			return 0, false, err
		}
		n := uint32(binary.LittleEndian.Uint16(hdr[12:])) + uint32(binary.LittleEndian.Uint16(hdr[14:]))
		for i := range n {
			e, err := p.bytesAt(root+off+16+8*i, 8)
			if err != nil {
				return 0, false, err
			}
			name, target := binary.LittleEndian.Uint32(e[0:]), binary.LittleEndian.Uint32(e[4:])
			if any || (name&0x80000000 == 0 && name == id) {
				return target, true, nil
			}
		}
		return 0, false, nil
	}
	target, found, err := entry(0, rtVersion, false)
	if err != nil || !found {
		return nil, wrapResource(err)
	}
	for level := 0; target&0x80000000 != 0; level++ { // name, then language: take the first of each
		if level > 2 {
			return nil, errors.New("resources: the version resource is nested too deeply")
		}
		if target, found, err = entry(target&0x7FFFFFFF, 0, true); err != nil || !found {
			return nil, wrapResource(err)
		}
	}
	leaf, err := p.bytesAt(root+target, 16)
	if err != nil {
		return nil, wrapResource(err)
	}
	dataRVA, size := binary.LittleEndian.Uint32(leaf[0:]), binary.LittleEndian.Uint32(leaf[4:])
	if size > 1<<20 {
		return nil, fmt.Errorf("resources: a %d-byte version resource", size)
	}
	blob, err := p.bytesAt(dataRVA, int(size))
	if err != nil {
		return nil, wrapResource(err)
	}
	return parseVersionInfo(blob)
}

func wrapResource(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("resources: %w", err)
}

// verBlock is one block of a VERSIONINFO resource (VS_VERSIONINFO, StringFileInfo, StringTable, String, VarFileInfo,
// Var): wLength, wValueLength, wType, a UTF-16 key, a value and child blocks, each aligned to 4 bytes.
type verBlock struct {
	key      string
	text     bool   // wType 1
	value    []byte // the value's bytes (a String's text runs to the block's end; see stringValue)
	tail     []byte // from the value to the block's end (children, or a String's text)
	children []verBlock
}

func align4(n int) int { return (n + 3) &^ 3 }

// hasChildren says which blocks hold blocks: VS_VERSION_INFO, StringFileInfo and VarFileInfo, and a StringTable
// (the child of StringFileInfo); a String and a Var hold only their value.
func hasChildren(parent string, depth int) bool {
	switch depth {
	case 0, 1:
		return true
	case 2:
		return parent == "StringFileInfo"
	}
	return false
}

// parseVerBlock parses the block at buf[off:] (off 4-aligned) and returns it with the offset after it.
func parseVerBlock(buf []byte, off int, parent string, depth int) (verBlock, int, error) {
	var b verBlock
	if depth > 3 {
		return b, 0, errors.New("version resource: nested too deeply")
	}
	if off+6 > len(buf) {
		return b, 0, errors.New("version resource: a block runs past the resource")
	}
	le := binary.LittleEndian
	length := int(le.Uint16(buf[off:]))
	valueLen := int(le.Uint16(buf[off+2:]))
	b.text = le.Uint16(buf[off+4:]) == 1
	end := off + length
	if length < 6 || end > len(buf) {
		return b, 0, fmt.Errorf("version resource: a block of %d bytes at %d runs past the resource", length, off)
	}
	var key []uint16
	p := off + 6
	for ; p+2 <= end; p += 2 {
		c := le.Uint16(buf[p:])
		if c == 0 {
			p += 2
			break
		}
		key = append(key, c)
	}
	b.key = string(utf16.Decode(key))
	p = align4(p)
	if p > end {
		p = end
	}
	n := valueLen
	if b.text {
		n *= 2
	}
	b.tail = buf[p:end]
	b.value = buf[p:min(end, p+n)]
	p = align4(p + n)
	if hasChildren(parent, depth) {
		for p+6 <= end {
			child, next, err := parseVerBlock(buf, p, b.key, depth+1)
			if err != nil {
				return b, 0, err
			}
			b.children = append(b.children, child)
			p = align4(next)
		}
	}
	return b, end, nil
}

// stringValue is a String block's text: UTF-16 from the value's start to its NUL (rc.exe counts wValueLength in
// characters, other tools in bytes, so the length field isn't trusted).
func (b verBlock) stringValue() string {
	var s []uint16
	for i := 0; i+2 <= len(b.tail); i += 2 {
		c := binary.LittleEndian.Uint16(b.tail[i:])
		if c == 0 {
			break
		}
		s = append(s, c)
	}
	return string(utf16.Decode(s))
}

func parseVersionInfo(blob []byte) (*versionInfo, error) {
	root, _, err := parseVerBlock(blob, 0, "", 0)
	if err != nil {
		return nil, err
	}
	if root.key != "VS_VERSION_INFO" {
		return nil, fmt.Errorf("version resource: the root block is %q, not VS_VERSION_INFO", root.key)
	}
	if len(root.value) < 52 || binary.LittleEndian.Uint32(root.value) != fixedInfoSig {
		return nil, errors.New("version resource: no VS_FIXEDFILEINFO")
	}
	le := binary.LittleEndian
	v := &versionInfo{strings: map[string]string{}}
	// dwFileVersionMS, dwFileVersionLS, dwProductVersionMS, dwProductVersionLS: each DWORD holds two numbers, the
	// first in its high word (little-endian: bytes 2..3).
	words := func(off int) [4]uint16 {
		return [4]uint16{le.Uint16(root.value[off+2:]), le.Uint16(root.value[off:]), le.Uint16(root.value[off+6:]),
			le.Uint16(root.value[off+4:])}
	}
	v.fileVersion = words(8)
	v.productVersion = words(16)
	for _, sfi := range root.children {
		if sfi.key != "StringFileInfo" || len(sfi.children) == 0 {
			continue
		}
		table := sfi.children[0]
		for _, t := range sfi.children {
			if strings.EqualFold(t.key, "040904b0") {
				table = t
			}
		}
		for _, s := range table.children {
			if _, dup := v.strings[s.key]; !dup {
				v.order = append(v.order, s.key)
			}
			v.strings[s.key] = s.stringValue()
		}
		break
	}
	return v, nil
}

// formatVersion writes four 16-bit numbers as a.b.c.d.
func formatVersion(v [4]uint16) string { return fmt.Sprintf("%d.%d.%d.%d", v[0], v[1], v[2], v[3]) }
