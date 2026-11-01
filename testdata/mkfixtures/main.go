// Command mkfixtures builds the FAT12 and FAT16 test images in ../ with the
// platform's own tools, so the committed fixtures are another implementation's
// idea of a volume, not the driver's.
//
// newfs_msdos (macOS) formats; mtools (mmd, mcopy) populates. Everything that
// varies is fixed (volume ID, file contents from a seeded PRNG), so a re-run
// reproduces the same files; the image bytes themselves carry mtools
// timestamps and are committed, not re-derived by CI.
//
//	go run ./testdata/mkfixtures   (from the module root: cd testdata/mkfixtures && go run . ..)
//
// It writes fat12.img.gz, fat16.img.gz and fixtures.json (the expected
// contents: path, size and SHA-256 of every file, and the root listing).
package main

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
)

type file struct {
	Path   string `json:"path"`
	Size   int    `json:"size"`
	SHA256 string `json:"sha256"`
}

type image struct {
	Image       string   `json:"image"`
	Type        int      `json:"type"`
	RootEntries int      `json:"root_entries"`
	RootFull    bool     `json:"root_full"`
	Files       []file   `json:"files"`
	Dirs        []string `json:"dirs"`
	Root        []string `json:"root"`
}

type spec struct {
	name     string
	fat      string
	sectors  int
	spc      int
	rootEnt  int
	label    string
	volID    string
	fillRoot bool
	big      int
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: mkfixtures <testdata dir>")
		os.Exit(2)
	}
	out := os.Args[1]
	var all []image
	for _, s := range []spec{
		{"fat12", "12", 2048, 1, 32, "FAT12TEST", "0x12121212", true, 205000},
		{"fat16", "16", 16384, 2, 512, "FAT16TEST", "0x16161616", false, 300000},
	} {
		im, err := build(out, s)
		if err != nil {
			fmt.Fprintln(os.Stderr, "mkfixtures:", err)
			os.Exit(1)
		}
		all = append(all, im)
	}
	b, _ := json.MarshalIndent(all, "", "  ")
	if err := os.WriteFile(filepath.Join(out, "fixtures.json"), append(b, '\n'), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "mkfixtures:", err)
		os.Exit(1)
	}
}

func run(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdin = nil
	if o, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s %v: %v\n%s", name, args, err, o)
	}
	return nil
}

func build(out string, s spec) (image, error) {
	work, err := os.MkdirTemp("", "mkfixtures")
	if err != nil {
		return image{}, err
	}
	defer os.RemoveAll(work)
	img := filepath.Join(work, s.name+".img")
	if err := os.WriteFile(img, make([]byte, s.sectors*512), 0o644); err != nil {
		return image{}, err
	}
	if err := run("newfs_msdos", "-F", s.fat, "-S", "512", "-c", strconv.Itoa(s.spc), "-e", strconv.Itoa(s.rootEnt),
		"-I", s.volID, "-v", s.label, "-s", strconv.Itoa(s.sectors), img); err != nil {
		return image{}, err
	}
	typ, _ := strconv.Atoi(s.fat)
	im := image{Image: s.name + ".img.gz", Type: typ, RootEntries: s.rootEnt}
	rng := rand.New(rand.NewSource(int64(typ)))
	put := func(path string, size int) error {
		data := make([]byte, size)
		if size > 4096 {
			// Large files carry the number of every 512-byte block and zeros
			// elsewhere: position-unique (a swapped or repeated cluster is
			// visible) and tiny once compressed.
			for i := 0; i*512 < size; i++ {
				copy(data[i*512:], fmt.Sprintf("BLK%05d", i))
			}
		} else {
			rng.Read(data)
		}
		src := filepath.Join(work, "src.bin")
		if err := os.WriteFile(src, data, 0o644); err != nil {
			return err
		}
		if err := run("mcopy", "-i", img, "-D", "o", src, "::"+path); err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		im.Files = append(im.Files, file{path, size, hex.EncodeToString(sum[:])})
		return nil
	}
	mkdir := func(path string) error {
		if err := run("mmd", "-i", img, "::"+path); err != nil {
			return err
		}
		im.Dirs = append(im.Dirs, path)
		return nil
	}
	rootNames := []string{}
	step := func(err error) error { return err }
	for _, f := range []struct {
		path string
		size int
	}{
		{"/HELLO.TXT", 10},
		{"/A long file name.txt", 3000},
		{"/BIG.BIN", s.big},
	} {
		if err := step(put(f.path, f.size)); err != nil {
			return image{}, err
		}
		rootNames = append(rootNames, f.path[1:])
	}
	for _, d := range []string{"/DIR1", "/DIR1/DIR2", "/MANY"} {
		if err := mkdir(d); err != nil {
			return image{}, err
		}
	}
	rootNames = append(rootNames, "DIR1", "MANY")
	if err := put("/DIR1/DIR2/NESTED.TXT", 700); err != nil {
		return image{}, err
	}
	if err := put("/DIR1/inner long name.txt", 100); err != nil {
		return image{}, err
	}
	// A directory that spans several clusters.
	for i := 0; i < 40; i++ {
		if err := put(fmt.Sprintf("/MANY/G%03d.TXT", i), 5); err != nil {
			return image{}, err
		}
	}
	if s.fillRoot {
		// Fill the fixed root directory to its last slot: a short name takes
		// exactly one, so mcopy only fails once there is none left.
		for i := 0; ; i++ {
			before := len(im.Files)
			if err := put(fmt.Sprintf("/F%03d.TXT", i), 3); err != nil {
				im.Files = im.Files[:before]
				if i < 5 {
					return image{}, err
				}
				im.RootFull = true
				break
			}
			rootNames = append(rootNames, fmt.Sprintf("F%03d.TXT", i))
		}
	}
	im.Root = rootNames
	var buf bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	raw, err := os.ReadFile(img)
	if err != nil {
		return image{}, err
	}
	zw.Write(raw)
	zw.Close()
	return im, os.WriteFile(filepath.Join(out, im.Image), buf.Bytes(), 0o644)
}
