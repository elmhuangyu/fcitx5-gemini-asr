package main

import (
	"sort"
	"strconv"
	"strings"
)

// This file implements the small subset of Fcitx5's INI serialization that
// gemini-voice.conf needs, in a way that preserves hand edits.
//
// Fcitx5 flattens its configuration into slash-separated paths. A path's last
// segment is the key and everything before it is the section name, so
// "LiveTranscription/Model" is the key Model in [LiveTranscription], list
// elements are integer keys in a section named after the list
// ("[PostProcess/Prefetch]" with 0=, 1=), and a list of structs becomes one
// section per element ("[PostProcess/Actions/0]"). Values containing
// whitespace, quotes, backslashes or control characters are double-quoted with
// \\, \", \n, \r and \t escapes. Options equal to their default are written as
// "# Key=value" comments, so a missing key always means "use the default".
// The format was verified against real Fcitx5 output; see
// testdata/gemini-voice*.conf, which the addon's tests generate.

type iniKind int

const (
	iniBlank iniKind = iota
	iniComment
	iniHeader
	iniKV
	iniOther
)

type iniLine struct {
	raw     string // exact line including its line ending
	kind    iniKind
	section string // header: its name; kv: the enclosing section ("" at top level)
	key     string // kv only
	val     string // kv only, unquoted
	path    string // kv only: section + "/" + key (or just key at top level)
}

func joinINIPath(section, key string) string {
	if section == "" {
		return key
	}
	return section + "/" + key
}

func splitINIPath(path string) (section, key string) {
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		return path[:i], path[i+1:]
	}
	return "", path
}

// unquoteFcitxValue trims v and, when it is double-quoted, decodes the escape
// sequences Fcitx5 emits.
func unquoteFcitxValue(v string) string {
	v = strings.TrimSpace(v)
	if len(v) < 2 || v[0] != '"' || v[len(v)-1] != '"' {
		return v
	}
	inner := v[1 : len(v)-1]
	var b strings.Builder
	b.Grow(len(inner))
	for i := 0; i < len(inner); i++ {
		c := inner[i]
		if c != '\\' || i+1 >= len(inner) {
			b.WriteByte(c)
			continue
		}
		i++
		switch inner[i] {
		case 'n':
			b.WriteByte('\n')
		case 'r':
			b.WriteByte('\r')
		case 't':
			b.WriteByte('\t')
		case '\\', '"':
			b.WriteByte(inner[i])
		default:
			b.WriteByte('\\')
			b.WriteByte(inner[i])
		}
	}
	return b.String()
}

// quoteFcitxValue encodes v the way Fcitx5 does: bare unless it contains
// whitespace, quotes, backslashes or control characters.
func quoteFcitxValue(v string) string {
	if !strings.ContainsAny(v, " \t\r\n\"\\") {
		return v
	}
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(v); i++ {
		switch v[i] {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteByte(v[i])
		}
	}
	b.WriteByte('"')
	return b.String()
}

// splitINILines splits s into lines that keep their own line endings.
func splitINILines(s string) []string {
	var lines []string
	for len(s) > 0 {
		i := strings.IndexByte(s, '\n')
		if i < 0 {
			lines = append(lines, s)
			break
		}
		lines = append(lines, s[:i+1])
		s = s[i+1:]
	}
	return lines
}

func classifyINILines(raws []string) []iniLine {
	out := make([]iniLine, len(raws))
	section := ""
	for i, raw := range raws {
		text := strings.TrimRight(raw, "\r\n")
		trimmed := strings.TrimSpace(text)
		ln := iniLine{raw: raw, section: section}
		switch {
		case trimmed == "":
			ln.kind = iniBlank
		case trimmed[0] == '#' || trimmed[0] == ';':
			ln.kind = iniComment
		case trimmed[0] == '[' && trimmed[len(trimmed)-1] == ']':
			section = strings.TrimSpace(trimmed[1 : len(trimmed)-1])
			ln.kind = iniHeader
			ln.section = section
		default:
			eq := strings.IndexByte(trimmed, '=')
			if eq <= 0 {
				ln.kind = iniOther
				break
			}
			ln.kind = iniKV
			ln.key = strings.TrimSpace(trimmed[:eq])
			ln.val = unquoteFcitxValue(trimmed[eq+1:])
			ln.path = joinINIPath(section, ln.key)
		}
		out[i] = ln
	}
	return out
}

// iniDoc is an editable view of an INI file. All edits operate on the raw
// lines and re-classify them, so unknown content (comments, keys, sections)
// and line endings survive untouched.
type iniDoc struct {
	raws []string
	eol  string
}

func newINIDoc(data []byte) *iniDoc {
	d := &iniDoc{raws: splitINILines(string(data)), eol: "\n"}
	if len(d.raws) > 0 && strings.HasSuffix(d.raws[0], "\r\n") {
		d.eol = "\r\n"
	}
	return d
}

func (d *iniDoc) bytes() []byte { return []byte(strings.Join(d.raws, "")) }

func (d *iniDoc) lines() []iniLine { return classifyINILines(d.raws) }

// ensureTrailingEOL terminates the last line when the file lacks a final newline.
func (d *iniDoc) ensureTrailingEOL() {
	if n := len(d.raws); n > 0 && !strings.HasSuffix(d.raws[n-1], "\n") {
		d.raws[n-1] += d.eol
	}
}

func (d *iniDoc) has(path string) bool {
	for _, ln := range d.lines() {
		if ln.kind == iniKV && ln.path == path {
			return true
		}
	}
	return false
}

// iniScalar returns the value at path; like Fcitx5, the last occurrence wins.
func iniScalar(lines []iniLine, path string) (string, bool) {
	val, found := "", false
	for _, ln := range lines {
		if ln.kind == iniKV && ln.path == path {
			val, found = ln.val, true
		}
	}
	return val, found
}

// iniList reads a list option: integer keys in the section named path. An
// explicit empty "Name=" in the parent section means an explicitly empty list
// (distinct from an absent one, which means "use the default").
func iniList(lines []iniLine, path string) ([]string, bool) {
	type entry struct {
		idx int
		val string
	}
	var entries []entry
	for _, ln := range lines {
		if ln.kind != iniKV || ln.section != path {
			continue
		}
		idx, err := strconv.Atoi(ln.key)
		if err != nil {
			continue
		}
		entries = append(entries, entry{idx, ln.val})
	}
	if len(entries) == 0 {
		if _, ok := iniScalar(lines, path); ok {
			return []string{}, true
		}
		return nil, false
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].idx < entries[j].idx })
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.val != "" {
			out = append(out, e.val)
		}
	}
	return out, true
}

// iniStructIndices lists the distinct element indices of a list of structs
// (sections "prefix/<n>"), ascending.
func iniStructIndices(lines []iniLine, prefix string) []int {
	seen := map[int]bool{}
	var idxs []int
	for _, ln := range lines {
		if ln.kind != iniHeader && ln.kind != iniKV {
			continue
		}
		rest := ln.section
		if !strings.HasPrefix(rest, prefix+"/") {
			continue
		}
		rest = strings.TrimPrefix(rest, prefix+"/")
		if strings.Contains(rest, "/") {
			continue
		}
		idx, err := strconv.Atoi(rest)
		if err != nil || seen[idx] {
			continue
		}
		seen[idx] = true
		idxs = append(idxs, idx)
	}
	sort.Ints(idxs)
	return idxs
}

// firstHeader returns the index of the first header named section, or -1.
func firstHeader(lines []iniLine, section string) int {
	for i, ln := range lines {
		if ln.kind == iniHeader && ln.section == section {
			return i
		}
	}
	return -1
}

func (d *iniDoc) splice(at, remove int, insert []string) {
	out := make([]string, 0, len(d.raws)-remove+len(insert))
	out = append(out, d.raws[:at]...)
	out = append(out, insert...)
	out = append(out, d.raws[at+remove:]...)
	d.raws = out
}

func (d *iniDoc) deleteIndices(del map[int]bool) {
	if len(del) == 0 {
		return
	}
	out := make([]string, 0, len(d.raws))
	for i, raw := range d.raws {
		if !del[i] {
			out = append(out, raw)
		}
	}
	d.raws = out
}

// appendBlock appends lines at the end of the file, separated from the
// existing content by a blank line.
func (d *iniDoc) appendBlock(block []string) {
	d.ensureTrailingEOL()
	if n := len(d.raws); n > 0 && strings.TrimSpace(d.raws[n-1]) != "" {
		d.raws = append(d.raws, d.eol)
	}
	d.raws = append(d.raws, block...)
}

func (d *iniDoc) kvLine(key, quotedVal string) string {
	return key + "=" + quotedVal + d.eol
}

// setScalar sets path to val (already unquoted). An existing line keeps its
// position and is only rewritten when its value differs; later duplicates are
// dropped. A missing key is inserted into its section, which is created at the
// end of the file when absent.
func (d *iniDoc) setScalar(path, val string) {
	lines := d.lines()
	var hits []int
	for i, ln := range lines {
		if ln.kind == iniKV && ln.path == path {
			hits = append(hits, i)
		}
	}
	if len(hits) > 0 {
		first := hits[0]
		if len(hits) == 1 && lines[first].val == val {
			return
		}
		ending := lines[first].raw[len(strings.TrimRight(lines[first].raw, "\r\n")):]
		d.raws[first] = lines[first].key + "=" + quoteFcitxValue(val) + ending
		del := map[int]bool{}
		for _, h := range hits[1:] {
			del[h] = true
		}
		d.deleteIndices(del)
		return
	}

	section, key := splitINIPath(path)
	newLine := d.kvLine(key, quoteFcitxValue(val))
	d.ensureTrailingEOL()
	lines = d.lines()

	if section == "" {
		hdr := -1
		for i, ln := range lines {
			if ln.kind == iniHeader {
				hdr = i
				break
			}
		}
		if hdr < 0 {
			d.raws = append(d.raws, newLine)
			return
		}
		at := hdr
		for at > 0 && lines[at-1].kind == iniBlank {
			at--
		}
		ins := []string{newLine}
		if at == hdr { // no blank line between the keys and the first section
			ins = append(ins, d.eol)
		}
		d.splice(at, 0, ins)
		return
	}

	hdr := firstHeader(lines, section)
	if hdr < 0 {
		d.appendBlock([]string{"[" + section + "]" + d.eol, newLine})
		return
	}
	at := hdr + 1
	for i := hdr + 1; i < len(lines) && lines[i].kind != iniHeader; i++ {
		if lines[i].kind == iniKV {
			at = i + 1
		}
	}
	d.splice(at, 0, []string{newLine})
}

// removeScalar deletes every line for path.
func (d *iniDoc) removeScalar(path string) {
	del := map[int]bool{}
	for i, ln := range d.lines() {
		if ln.kind == iniKV && ln.path == path {
			del[i] = true
		}
	}
	d.deleteIndices(del)
}

// sectionExtent describes the lines belonging to the section whose header is
// at index hdr. Comments directly after the header or a key (with no blank
// line in between) belong to the section; a comment block after a blank line
// documents the following section and is excluded.
func sectionExtent(lines []iniLine, hdr int) (last int) {
	last = hdr
	attached := true
	for i := hdr + 1; i < len(lines) && lines[i].kind != iniHeader; i++ {
		switch lines[i].kind {
		case iniBlank:
			attached = false
		case iniComment:
			if attached {
				last = i
			}
		default:
			last = i
			attached = true
		}
	}
	return last
}

// removeSectionAt deletes the section at hdr (header, keys and attached
// comments) plus one adjacent blank separator line. It reports how many lines
// were removed and the index where the section started.
func (d *iniDoc) removeSectionAt(hdr int) {
	lines := d.lines()
	last := sectionExtent(lines, hdr)
	start := hdr
	end := last + 1
	switch {
	case start > 0 && lines[start-1].kind == iniBlank:
		start--
	case end < len(lines) && lines[end].kind == iniBlank:
		end++
	}
	d.splice(start, end-start, nil)
}

// setList writes a list option. Non-empty lists rewrite the integer keys of
// the first matching section in place (keeping its comments); an empty list
// removes the section and, when the default is non-empty, records the
// explicit emptiness as "Name=" like Fcitx5 does.
func (d *iniDoc) setList(path string, values []string, defaultEmpty bool) {
	cur, present := iniList(d.lines(), path)
	if present && stringSlicesEqual(cur, values) {
		return
	}
	if !present && len(values) == 0 && defaultEmpty {
		return
	}
	d.removeScalar(path)

	if len(values) == 0 {
		for {
			lines := d.lines()
			hdr := firstHeader(lines, path)
			if hdr < 0 {
				break
			}
			if !d.dropListSection(hdr) {
				break
			}
		}
		if !defaultEmpty {
			d.setScalar(path, "")
		}
		return
	}

	d.ensureTrailingEOL()
	lines := d.lines()
	hdr := firstHeader(lines, path)
	entries := make([]string, len(values))
	for i, v := range values {
		entries[i] = d.kvLine(strconv.Itoa(i), quoteFcitxValue(v))
	}
	if hdr < 0 {
		d.appendBlock(append([]string{"[" + path + "]" + d.eol}, entries...))
		return
	}
	del := map[int]bool{}
	for i, ln := range lines {
		switch {
		case ln.kind == iniKV && ln.section == path && isIntegerKey(ln.key):
			del[i] = true
		case ln.kind == iniHeader && ln.section == path && i != hdr:
			del[i] = true
		}
	}
	d.deleteIndices(del)
	d.splice(hdr+1, 0, entries)
}

// dropListSection removes a list section whose only keys are list elements.
// A section that also holds unknown keys keeps its header and those keys; the
// element lines are removed. It reports whether the section header is gone.
func (d *iniDoc) dropListSection(hdr int) bool {
	lines := d.lines()
	last := sectionExtent(lines, hdr)
	foreign := false
	for i := hdr + 1; i <= last; i++ {
		if lines[i].kind == iniKV && !isIntegerKey(lines[i].key) || lines[i].kind == iniOther {
			foreign = true
		}
	}
	if !foreign {
		d.removeSectionAt(hdr)
		return true
	}
	del := map[int]bool{}
	for i := hdr + 1; i <= last; i++ {
		if lines[i].kind == iniKV && isIntegerKey(lines[i].key) {
			del[i] = true
		}
	}
	d.deleteIndices(del)
	// Stop the caller's loop from revisiting the (now element-free) section.
	return false
}

func isIntegerKey(s string) bool {
	_, err := strconv.Atoi(s)
	return err == nil
}

type structField struct{ key, val string }

// setStructList replaces a whole list of structs ("prefix/<n>" sections).
// Fields with empty values are not written. When the existing elements already
// match, the file is left untouched.
func (d *iniDoc) setStructList(prefix string, items [][]structField) {
	if equalStructLists(readStructList(d.lines(), prefix, structFieldKeys(items)), items) {
		return
	}
	d.removeScalar(prefix)

	anchor := -1
	for {
		lines := d.lines()
		hdr := -1
		for i, ln := range lines {
			if ln.kind == iniHeader && isStructSection(ln.section, prefix) {
				hdr = i
				break
			}
		}
		if hdr < 0 {
			break
		}
		if anchor < 0 {
			anchor = hdr
			if hdr > 0 && lines[hdr-1].kind == iniBlank {
				anchor = hdr - 1
			}
		}
		last := sectionExtent(lines, hdr)
		start, end := hdr, last+1
		if start > 0 && lines[start-1].kind == iniBlank {
			start--
		} else if end < len(lines) && lines[end].kind == iniBlank {
			end++
		}
		d.splice(start, end-start, nil)
		if start < anchor {
			anchor -= end - start
		}
	}
	if len(items) == 0 {
		return
	}

	var block []string
	for i, fields := range items {
		if i > 0 {
			block = append(block, d.eol)
		}
		block = append(block, "["+prefix+"/"+strconv.Itoa(i)+"]"+d.eol)
		for _, f := range fields {
			block = append(block, d.kvLine(f.key, quoteFcitxValue(f.val)))
		}
	}
	d.ensureTrailingEOL()
	if anchor < 0 || anchor > len(d.raws) {
		d.appendBlock(block)
		return
	}
	// Re-insert where the first removed element used to be, blank-separated.
	withBlank := make([]string, 0, len(block)+2)
	lines := d.lines()
	if anchor > 0 && lines[anchor-1].kind != iniBlank {
		withBlank = append(withBlank, d.eol)
	}
	withBlank = append(withBlank, block...)
	if anchor < len(lines) && lines[anchor].kind != iniBlank {
		withBlank = append(withBlank, d.eol)
	}
	d.splice(anchor, 0, withBlank)
}

func isStructSection(section, prefix string) bool {
	if !strings.HasPrefix(section, prefix+"/") {
		return false
	}
	return isIntegerKey(strings.TrimPrefix(section, prefix+"/"))
}

func structFieldKeys(items [][]structField) []string {
	return []string{"Name", "Prompt", "Model"}
}

func readStructList(lines []iniLine, prefix string, keys []string) [][]structField {
	var out [][]structField
	for _, idx := range iniStructIndices(lines, prefix) {
		section := prefix + "/" + strconv.Itoa(idx)
		var fields []structField
		for _, k := range keys {
			if v, ok := iniScalar(lines, section+"/"+k); ok && v != "" {
				fields = append(fields, structField{k, v})
			}
		}
		out = append(out, fields)
	}
	return out
}

func equalStructLists(a, b [][]structField) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if len(a[i]) != len(b[i]) {
			return false
		}
		for j := range a[i] {
			if a[i][j] != b[i][j] {
				return false
			}
		}
	}
	return true
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
