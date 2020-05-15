package main

import (
	"archive/zip"
	"crypto/md5"
	"encoding/binary"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path"
	pathlib "path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nilp0inter/MiSTer_WebMenu/fastwalk"
	_ "github.com/nilp0inter/MiSTer_WebMenu/statik"
	"github.com/nilp0inter/MiSTer_WebMenu/system"
	"github.com/nilp0inter/MiSTer_WebMenu/update"

	"github.com/bendahl/uinput"
	"github.com/gorilla/mux"
	"github.com/rakyll/statik/fs"
	"github.com/thetannerryan/ring"
	bolt "go.etcd.io/bbolt"
)

// Version is obtained at compile time
var Version = "<Version>"

var scanMutex = &sync.Mutex{}

type Cores struct {
	RBFs []RBF `json:"rbfs"`
	MRAs []MRA `json:"mras"`
}

type MRA struct {
	Path      string   `json:"path"`
	Filename  string   `json:"filename"`
	Ctime     int64    `json:"ctime"`
	LogicPath []string `json:"lpath"`
	MD5       string   `json:"md5"`
	Name      string   `json:"name" xml:"name"`
	Rbf       string   `xml:"rbf" json:"-"`
	Roms      []struct {
		Zip   string `xml:"zip,attr" json:"zip"`
		Index string `xml:"index,attr" json:"-"`
	} `xml:"rom" json:"roms"`
	RomsFound bool `json:"roms_found"`
}

type RBF struct {
	Path      string   `json:"path"`
	Filename  string   `json:"filename"`
	Codename  string   `json:"codename"`
	Codedate  string   `json:"codedate"`
	Ctime     int64    `json:"ctime"`
	LogicPath []string `json:"lpath"`
	MD5       string   `json:"md5"`
}

func scanMRA(filename string) (MRA, error) {
	var c MRA

	// Path
	c.Path = filename
	fi, err := os.Stat(filename)
	if err != nil {
		return c, err
	}
	c.Ctime = fi.ModTime().Unix()

	// MD5
	x, err := ioutil.ReadFile(filename)
	if err != nil {
		return c, err
	}

	h := md5.New()
	h.Sum(x)
	c.MD5 = fmt.Sprintf("%x", h.Sum(nil))

	// NAME
	baseDir := pathlib.Dir(filename)
	c.Filename = pathlib.Base(filename)

	// LPATH
	for _, d := range strings.Split(strings.TrimPrefix(baseDir, system.SdPath), "/") {
		if strings.HasPrefix(d, "_") {
			c.LogicPath = append(c.LogicPath, strings.TrimLeft(d, "_"))
		}
	}

	err = xml.Unmarshal(x, &c)
	if err != nil {
		return c, err
	}

	c.RomsFound = len(c.Roms) == 0
	rp := 0
	for i := 0; i < len(c.Roms); i++ {
		rom := c.Roms[i]
		if rom.Zip == "" || rom.Index != "0" {
			continue
		}
		c.Roms[rp] = rom
		rp++
		thisFound := false
	romLoop:
		for _, zip := range strings.Split(rom.Zip, "|") {
			parent := filepath.Clean(path.Join(system.SdPath, "..", "..")) //Double .. to include /media/fat
			for p := baseDir; filepath.Clean(p) != parent; p = path.Join(p, "..") {
				_, err := os.Stat(path.Join(p, zip))
				if err == nil {
					thisFound = true
					break romLoop
				}
				_, err = os.Stat(path.Join(p, "mame", zip))
				if err == nil {
					thisFound = true
					break romLoop
				}
				_, err = os.Stat(path.Join(p, "hbmame", zip))
				if err == nil {
					thisFound = true
					break romLoop
				}
			}
		}
		c.RomsFound = c.RomsFound || thisFound
	}
	c.Roms = c.Roms[:rp]

	return c, nil
}

func scanRBF(filename string) (RBF, error) {
	var c RBF

	// Path
	c.Path = filename
	fi, err := os.Stat(filename)
	if err != nil {
		return c, err
	}
	c.Ctime = fi.ModTime().Unix()

	// MD5
	f, err := os.Open(filename)
	if err != nil {
		return c, err
	}
	defer f.Close()

	h := md5.New()
	if _, err := io.Copy(h, f); err != nil {
		return c, err
	}
	c.MD5 = fmt.Sprintf("%x", h.Sum(nil))

	// NAME
	c.Filename = pathlib.Base(filename)

	re := regexp.MustCompile(`^([^_]+)_(\d{8})[^\.]*\.rbf$`)
	matches := re.FindStringSubmatch(c.Filename)
	if matches != nil {
		c.Codename = string(matches[1])
		c.Codedate = string(matches[2])
	}

	// LPATH
	for _, d := range strings.Split(strings.TrimPrefix(pathlib.Dir(filename), system.SdPath), "/") {
		if strings.HasPrefix(d, "_") {
			c.LogicPath = append(c.LogicPath, strings.TrimLeft(d, "_"))
		}
	}
	return c, nil
}

func launchGame(filename string) error {
	return ioutil.WriteFile(system.MisterFifo, []byte("load_core "+filename), 0644)
}

func createCache() {
	os.MkdirAll(system.CachePath, os.ModePerm)
}

// Get preferred outbound ip of this machine
func GetOutboundIP() (error, net.IP) {
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return err, nil
	}
	defer conn.Close()

	localAddr := conn.LocalAddr().(*net.UDPAddr)

	return nil, localAddr.IP
}

func greetUser() {
	fmt.Printf("MiSTer WebMenu %s\n\n", Version)
	err, ip := GetOutboundIP()
	if err != nil {
		fmt.Println("No connection detected :(")
	} else {
		fmt.Printf("Browse to: http://%s\n", ip)
	}
}

func main() {

	// always do this after the initialization in order to guarantee that the device will be properly closed
	// defer keyboard.Close()

	greetUser()
	createCache()

	statikFS, err := fs.New()
	if err != nil {
		log.Fatal(err)
	}

	// Serve the contents over HTTP.
	r := mux.NewRouter()
	r.HandleFunc("/api/webmenu/reboot", PerformWebMenuReboot).Methods("POST")
	r.HandleFunc("/api/update", PerformUpdate).Methods("POST")
	r.HandleFunc("/api/run", RunCoreWithGame)
	r.HandleFunc("/api/input", BuildSendInput())
	r.HandleFunc("/api/version/current", GetCurrentVersion)
	r.HandleFunc("/api/cores/scan", ScanForCores)
	r.HandleFunc("/api/games/scan", ScanForGames)
	r.PathPrefix("/cached/").Handler(http.StripPrefix("/cached/", http.FileServer(http.Dir(system.CachePath))))
	r.PathPrefix("/").Handler(http.FileServer(statikFS))

	srv := &http.Server{
		Handler:      r,
		Addr:         "0.0.0.0:80",
		WriteTimeout: 90 * time.Second,
		ReadTimeout:  90 * time.Second,
	}
	log.Fatal(srv.ListenAndServe())
}

/////////////////////////////////////////////////////////////////////////
//                                 API                                 //
/////////////////////////////////////////////////////////////////////////

func GetCurrentVersion(w http.ResponseWriter, r *http.Request) {
	w.Write([]byte(Version))
}

func ScanPath(base string, file os.FileInfo, cores *Cores) {
	ext := strings.ToLower(pathlib.Ext(file.Name()))
	isPrefix := strings.HasPrefix(file.Name(), "_")
	filepath := path.Join(base, file.Name())
	if file.IsDir() && isPrefix {
		files, err := ioutil.ReadDir(filepath)
		if err != nil {
			// fmt.Println(err)
			return
		}
		for _, entry := range files {
			ScanPath(filepath, entry, cores)
		}
	} else if file.Mode().IsRegular() && ext == ".rbf" {
		fmt.Printf("RBF: %s\n", filepath)
		c, err := scanRBF(filepath)
		if err != nil {
			// log.Println(filepath, err)
		} else {
			cores.RBFs = append(cores.RBFs, c)
		}
	} else if file.Mode().IsRegular() && ext == ".mra" {
		fmt.Printf("MRA: %s\n", filepath)
		c, err := scanMRA(filepath)
		if err != nil {
			// log.Println(filepath, err)
		} else {
			cores.MRAs = append(cores.MRAs, c)
		}
	}
}

func ScanForCores(w http.ResponseWriter, r *http.Request) {
	scanMutex.Lock()
	defer scanMutex.Unlock()

	force, ok := r.URL.Query()["force"]
	doForce := ok && force[0] == "1"

	if _, err := os.Stat(system.CoresDBPath); doForce || err != nil {
		var cores Cores

		// Scan for RBFs & MRAs
		topLevels, err := ioutil.ReadDir(system.SdPath)
		for _, root := range topLevels {
			if strings.HasPrefix(root.Name(), "_") {
				ScanPath(system.SdPath, root, &cores)
			}
		}

		b, err := json.Marshal(cores)
		if err != nil {
			log.Fatal(err)
		}
		err = ioutil.WriteFile(system.CoresDBPath, b, 0644)
		if err != nil {
			log.Fatal(err)
		}
	}
	w.WriteHeader(http.StatusOK)
}

func RunCoreWithGame(w http.ResponseWriter, r *http.Request) {
	path, ok := r.URL.Query()["path"]
	if !ok {
		return
	}

	err := launchGame(path[0])
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(err.Error()))
		return
	}
}

func PerformUpdate(w http.ResponseWriter, r *http.Request) {
	version, ok := r.URL.Query()["version"]
	if !ok {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("Version is mandatory"))
		return
	}
	err := update.UpdateSystem(version[0])
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(err.Error()))
		return
	}
	return
}

func PerformWebMenuReboot(w http.ResponseWriter, r *http.Request) {
	cmd := exec.Command(system.WebMenuSHPath)
	go func() {
		time.Sleep(3 * time.Second)
		cmd.Run()
	}()
}

func BuildSendInput() func(http.ResponseWriter, *http.Request) {

	// initialize keyboard and check for possible errors
	keyboard, err := uinput.CreateKeyboard("/dev/uinput", []byte("WebMenu Virtual Keyboard"))
	if err != nil {
		return nil
	}

	return func(w http.ResponseWriter, r *http.Request) {
		scode, ok := r.URL.Query()["code"]
		if !ok {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte("Version is mandatory"))
			return
		}
		code, err := strconv.ParseInt(scode[0], 10, 8)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(err.Error()))
			return
		}
		err = keyboard.KeyDown(int(code))
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(err.Error()))
			return
		}
		time.Sleep(100 * time.Millisecond)
		err = keyboard.KeyUp(int(code))
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(err.Error()))
			return
		}
	}
}

func ScanForGames(w http.ResponseWriter, r *http.Request) {
	games := make(chan [3]string)
	scanPathParam, ok := r.URL.Query()["path"]
	if !ok {
		return
	}

	scanPath := path.Clean(scanPathParam[0])
	outputDir := pathlib.Join(system.GamesDBPath, pathlib.Dir(scanPath))
	os.MkdirAll(outputDir, 0600)

	f, err := os.Create(pathlib.Join(outputDir, pathlib.Base(scanPath)+".jsonl"))
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(err.Error()))
		return
	}

	enc := json.NewEncoder(f)

	go func() {
		err := ScanGames(scanPath, games)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(err.Error()))
			return
		}
	}()

	for game := range games {
		if err := enc.Encode(&game); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(err.Error()))
			return
		}
	}

}

func IsKnownExt(ext string) bool {
	switch ext {
	case
		// TODO: Altair 8800
		// Apogee
		"rka", "rkr", "gam",
		// Apple-II
		"nib", "dsk", "do", "po",
		// TODO: Apple-I
		// Aquarius
		"bin", "caq",
		// TODO: Archie
		// Atari800
		"atr", "xex", "xfd", "atx",
		"car", "rom", // "bin",
		// TODO: AtariST
		// BBCMicro
		"vhd",
		// BK0011M
		// "bin",
		// "dsk",
		// "vhd",
		// C16
		"prg",
		// "bin",
		"d64",
		"tap",
		// C64
		// "d64",
		"t64",
		// "prg",
		"crt",
		// "tap",
		// TODO: Galaksija
		// Jupiter
		"ace",
		// MSX
		// "vhd",
		// TODO: MacPlus
		// TODO: Minimig
		// TODO: MultiComp
		// TODO: ORAO
		// Oric
		// "dsk",
		// TODO: PDP1
		// PET201
		// "tap",
		// "prg",
		// QL
		"mvd",
		// SAMCoupe
		// "dsk",
		"mgt", "img",
		// TODO: SharpMZ
		// Specialist
		"rks", "od1",
		// TSConf
		// "vhd",
		// TODO: Ti994a
		// VIC20
		// "prg",
		// "crt",
		"ct?", // ???
		// "d64",
		// "tap",
		// Vector06
		// "rom",
		"com", "c00", "edd",
		"fdd",
		// TODO: X68000
		// ZX-Spectrum
		"trd",
		// "img",
		// "dsk",
		// "mgt",
		// "tap",
		"csw", "tzx",
		"z80",
		// ZX81
		"o", "p",
		// ao486
		// "img",
		// "vhd",
		// ht1080z
		"cas",
		// Astrocade
		// "bin",
		// Atari2600
		"a26", // Not from the core
		// Atari5200
		// "car",
		"a52",
		// "bin",
		// "rom",
		// Colecovision
		"col",
		// "bin",
		// "rom",
		"sg",
		// GBA
		"gba",
		// Gameboy
		"gbc", "gb",
		// Genesis
		// "bin",
		"gen", "md",
		// MegaCD
		"cue",
		// "bin",
		// "gen",
		// "md",
		// NES
		"nes", "fds", "nsf",
		// "bin",
		// TODO: NeoGeo
		// Odyssey2
		// "bin",
		// SMS
		"sms",
		// "sg",
		"gg",
		// SNES
		"sfc", "smc",
		// "bin",
		// TurboGrafx16
		"pce",
		// "bin",
		"sgx",
		// Vectrex
		// "bin",
		// "rom",
		"vec":
		return true
	}
	return false
}

func ScanZipForGames(basePath string, filename string, file *zip.ReadCloser, db *bolt.DB, crc_ring, size_ring *ring.Ring, games chan<- [3]string) error {
	buf_size := make([]byte, 8)
	for _, zf := range file.File {
		composePath := filepath.Join(filename, zf.FileHeader.Name)
		ext := strings.TrimLeft(strings.ToLower(filepath.Ext(zf.FileHeader.Name)), ".")
		if IsKnownExt(ext) {
			// Check SIZE against bloom
			binary.LittleEndian.PutUint64(buf_size[:], zf.FileHeader.UncompressedSize64)
			if !size_ring.Test(buf_size) {
				// Not a single known file matched size
				// fmt.Println("Skip (size)", composePath)
				games <- [3]string{composePath[len(basePath):], "", ""}
				return nil
			}

			// Check CRC32 against bloom
			buf_crc := make([]byte, 4)
			binary.LittleEndian.PutUint32(buf_crc[:], zf.FileHeader.CRC32)
			if !crc_ring.Test(buf_crc) {
				// Not a single known file matched size
				// fmt.Println("Skip (crc32)", composePath)
				games <- [3]string{composePath[len(basePath):], "", ""}
				return nil
			}

			f, err := zf.Open()
			if err != nil {
				return err
			}
			defer f.Close()

			h := md5.New()
			if _, err := io.Copy(h, f); err != nil {
				return err
			}

			// Check MD5 against bolt
			var md5Bucket = "MD5"
			err = db.View(func(tx *bolt.Tx) error {
				b := tx.Bucket([]byte(md5Bucket))
				v := b.Get([]byte(fmt.Sprintf("%x", h.Sum(nil))))
				if v != nil {
					// fmt.Println("Found", composePath, string(v))
					values := strings.SplitN(string(v), ";", 2)
					games <- [3]string{composePath[len(basePath):], values[1], values[0]}
				} else {
					games <- [3]string{composePath[len(basePath):], "", ""}
				}
				return nil
			})
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func ScanGames(basePath string, games chan<- [3]string) error {

	defer close(games)

	var bloomBucket = "BLOOM"
	var md5Bucket = "MD5"
	var crcKey = "crc"
	var sizeKey = "size"

	db, err := bolt.Open(pathlib.Join(system.CachePath, "databank.db"), 0600, &bolt.Options{ReadOnly: true})
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	// Load bloom filters
	crc_ring := new(ring.Ring)
	size_ring := new(ring.Ring)

	err = db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bloomBucket))
		v := b.Get([]byte(crcKey))
		if v == nil {
			return errors.New("CRC bloom filter is missing")
		}
		crc_ring.UnmarshalBinary(v)

		v = b.Get([]byte(sizeKey))
		if v == nil {
			return errors.New("Size bloom filter is missing")
		}
		size_ring.UnmarshalBinary(v)

		return nil
	})
	if err != nil {
		return err
	}

	// Scan path
	err = fastwalk.Walk(basePath, func(path string, typ os.FileMode) error {
		if typ.IsDir() {
			return nil
		} else if ext := strings.TrimLeft(strings.ToLower(filepath.Ext(path)), "."); ext == "zip" {
			r, err := zip.OpenReader(path)
			if err != nil {
				return err
			}
			defer r.Close()
			return ScanZipForGames(basePath, path, r, db, crc_ring, size_ring, games)
		} else if IsKnownExt(ext) {
			info, err := os.Lstat(path)
			if err != nil {
				return err
			}
			buf_size := make([]byte, 8)
			binary.LittleEndian.PutUint64(buf_size[:], uint64(info.Size()))
			if !size_ring.Test(buf_size) {
				// Not a single known file matched size
				// fmt.Println("Skip (size)", path)
				games <- [3]string{path[len(basePath):], "", ""}
				return nil
			}

			f, err := os.Open(path)
			if err != nil {
				return err
			}
			defer f.Close()

			h := md5.New()
			if _, err := io.Copy(h, f); err != nil {
				return err
			}

			err = db.View(func(tx *bolt.Tx) error {
				b := tx.Bucket([]byte(md5Bucket))
				v := b.Get([]byte(fmt.Sprintf("%x", h.Sum(nil))))
				if v != nil {
					// fmt.Println("Found", path, string(v))
					values := strings.SplitN(string(v), ";", 2)
					games <- [3]string{path[len(basePath):], values[1], values[0]}
				}
				return nil
			})
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}

	return nil
}
