// ptscry-gui - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later
// fonts.go: the monospaced fonts of the system.

package main

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	giofont "gioui.org/font"
	"gioui.org/font/opentype"
	"golang.org/x/image/font/sfnt"
)

// System monospace fonts, for the terminal font chooser. On Linux and macOS
// fc-list answers when fontconfig is there; otherwise the usual font folders
// are scanned and a face counts when its post table says "fixed pitch".

// monoFont is a monospaced family and the file of its regular face (empty
// when unknown: the system lookup by name is then the only way to it).
type monoFont struct {
	Name, Path string
}

var (
	monoMu    sync.Mutex
	monoCache []monoFont
)

// systemMonoFonts is the list found last time (scanning if there is none).
func systemMonoFonts() []monoFont {
	monoMu.Lock()
	c := monoCache
	monoMu.Unlock()
	if c != nil {
		return c
	}
	return rescanMonoFonts()
}

// rescanMonoFonts looks again, so that a font installed meanwhile shows up:
// fontconfig, plus the user's own font folders read directly (fontconfig sees
// a new font there only after its cache is refreshed).
func rescanMonoFonts() []monoFont {
	fs := fcMonoFiles()
	if len(fs) == 0 {
		fs = scanMonoFiles(fontDirs())
	} else {
		fs = append(fs, scanMonoFiles(userFontDirs())...)
	}
	l := uniqFonts(fs)
	if l == nil {
		l = []monoFont{}
	}
	monoMu.Lock()
	monoCache = l
	monoMu.Unlock()
	return l
}

// userFontDirs are where a user installs fonts without privileges.
func userFontDirs() []string {
	home, _ := os.UserHomeDir()
	switch runtime.GOOS {
	case "windows":
		return []string{filepath.Join(os.Getenv("LOCALAPPDATA"), "Microsoft", "Windows", "Fonts")}
	case "darwin":
		return []string{filepath.Join(home, "Library", "Fonts")}
	}
	d := []string{filepath.Join(home, ".local/share/fonts"), filepath.Join(home, ".fonts")}
	if x := os.Getenv("XDG_DATA_HOME"); filepath.IsAbs(x) {
		d = append(d, filepath.Join(x, "fonts"))
	}
	return d
}

// uniqFonts keeps one entry per family (the regular face when there is one),
// sorted by name.
func uniqFonts(in []fontFace) []monoFont {
	best := map[string]fontFace{}
	var order []string
	for _, f := range in {
		n := strings.TrimSpace(f.family)
		if n == "" || strings.ContainsAny(n, "\r\n,") {
			continue
		}
		k := strings.ToLower(n)
		old, ok := best[k]
		if !ok {
			order = append(order, k)
		}
		if !ok || regularStyle(f.style) && !regularStyle(old.style) {
			f.family = n
			best[k] = f
		}
	}
	out := make([]monoFont, 0, len(order))
	for _, k := range order {
		out = append(out, monoFont{Name: best[k].family, Path: best[k].path})
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	return out
}

type fontFace struct{ family, style, path string }

func regularStyle(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "regular", "book", "roman", "normal", "medium", "":
		return true
	}
	return false
}

// fcMonoFiles asks fontconfig: family, style and file of every monospaced face.
func fcMonoFiles() []fontFace {
	path, err := exec.LookPath("fc-list")
	if err != nil {
		return nil
	}
	cmd := exec.Command(path, "-f", "%{family[0]}\t%{style[0]}\t%{file}\n", ":spacing=mono")
	done := make(chan []byte, 1)
	go func() { b, _ := cmd.Output(); done <- b }()
	var out []byte
	select {
	case out = <-done:
	case <-time.After(15 * time.Second):
		_ = cmd.Process.Kill()
		return nil
	}
	var fs []fontFace
	for _, l := range strings.Split(string(out), "\n") {
		p := strings.Split(l, "\t")
		if len(p) == 3 && p[0] != "" && usableFont(p[0], p[2]) {
			fs = append(fs, fontFace{family: p[0], style: p[1], path: p[2]})
		}
	}
	return fs
}

func scanMonoFiles(dirs []string) []fontFace {
	var out []fontFace
	deadline := time.Now().Add(20 * time.Second)
	for _, dir := range dirs {
		_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil || time.Now().After(deadline) {
				return fs.SkipAll
			}
			if d.IsDir() {
				return nil
			}
			switch strings.ToLower(filepath.Ext(p)) {
			case ".ttf", ".otf", ".ttc", ".otc":
				out = append(out, monoFaces(p)...)
			}
			return nil
		})
	}
	return out
}

// usableFont: an OpenType/TrueType file (the only kind the renderer reads:
// Type 1 and bitmap fonts are left out) that is not an emoji font.
func usableFont(family, path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".ttf", ".otf", ".ttc", ".otc":
	default:
		return false
	}
	return !strings.Contains(strings.ToLower(family), "emoji")
}

// monoFaces lists the monospaced faces of a font file.
func monoFaces(path string) []fontFace {
	var out []fontFace
	for _, nf := range monoFacesNames(path) {
		if usableFont(nf[0], path) {
			out = append(out, fontFace{family: nf[0], style: nf[1], path: path})
		}
	}
	return out
}

func fontDirs() []string {
	home, _ := os.UserHomeDir()
	switch runtime.GOOS {
	case "windows":
		d := []string{filepath.Join(os.Getenv("WINDIR"), "Fonts")}
		if l := os.Getenv("LOCALAPPDATA"); l != "" {
			d = append(d, filepath.Join(l, "Microsoft", "Windows", "Fonts"))
		}
		return d
	case "darwin":
		return []string{"/System/Library/Fonts", "/Library/Fonts", filepath.Join(home, "Library", "Fonts")}
	}
	return []string{"/usr/share/fonts", "/usr/local/share/fonts", filepath.Join(home, ".local/share/fonts"), filepath.Join(home, ".fonts")}
}

func monoFamilies(path string) []string {
	var out []string
	for _, nf := range monoFacesNames(path) {
		out = append(out, nf[0])
	}
	return out
}

// monoFacesNames: family and style of every monospaced face of the file.
func monoFacesNames(path string) [][2]string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.Size() > 64<<20 {
		return nil
	}
	coll, err := sfnt.ParseCollectionReaderAt(f)
	if err != nil {
		return nil
	}
	var out [][2]string
	var buf sfnt.Buffer
	for i := 0; i < coll.NumFonts(); i++ {
		ft, err := coll.Font(i)
		if err != nil {
			continue
		}
		if p := ft.PostTable(); p == nil || !p.IsFixedPitch {
			continue
		}
		if n, err := ft.Name(&buf, sfnt.NameIDFamily); err == nil && !bytes.ContainsAny([]byte(n), "\x00") {
			st, _ := ft.Name(&buf, sfnt.NameIDSubfamily)
			out = append(out, [2]string{n, st})
		}
	}
	return out
}

func filterFonts(all []monoFont, q string) []monoFont {
	q = strings.ToLower(strings.TrimSpace(q))
	if q == "" {
		return all
	}
	var out []monoFont
	for _, f := range all {
		if strings.Contains(strings.ToLower(f.Name), q) {
			out = append(out, f)
		}
	}
	return out
}

var (
	faceMu    sync.Mutex
	faceCache = map[string][]giofont.FontFace{}
)

// loadFontFile reads the faces of a font file (cached).
func loadFontFile(path string) ([]giofont.FontFace, error) {
	faceMu.Lock()
	defer faceMu.Unlock()
	if f, ok := faceCache[path]; ok {
		return f, nil
	}
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if st.Size() > 64<<20 {
		return nil, errors.New("file del carattere troppo grande")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	faces, err := opentype.ParseCollection(raw)
	if err != nil {
		return nil, err
	}
	faceCache[path] = faces
	return faces, nil
}
