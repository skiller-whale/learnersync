package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"io/ioutil"
	"log"
	"math/rand"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/caarlos0/env/v6"
	"github.com/fsnotify/fsnotify"
)

const MAX_UPLOAD_BYTES = 2000000

const SEND_AFTER_MILLIS = 100
const PING_EVERY_MILLIS = 2000
const PING_WARNING_MILLIS = 5000
const POLL_INTERVAL_MILLIS = 2500
const MAX_RETRY_DELAY = 30000
const MAX_TRIGGER_TIME = 2500

func fatalIfSet(e error) {
	if e != nil {
		log.Fatal(e)
	}
}

type Error string

func (e Error) Error() string { return string(e) }

const TOO_LARGE_ERROR = Error("file too large")

// Read file at `path` into a byte array, as long as it is less than `max` bytes.
//
// Returns the data, or nil if there's an error (including TOO_LARGE_ERROR if file size > `max`)
func readFileMax(path string, max int) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	b := make([]byte, max)
	n, err := file.Read(b)
	if err != nil {
		if err == io.EOF {
			return []byte{}, nil
		}
		return nil, err
	}
	if n == max {
		return nil, TOO_LARGE_ERROR
	}
	return b[:n], nil
}

// Byte-order mark (Windows Notepad), claims to be UTF-8
var BOM = [3]byte{0xef, 0xbb, 0xbf}

func decodeBytes(b []byte) string {
	if len(b) > 3 && *(*[3]byte)(b) == BOM {
		b = b[3:]
	}

	// FIXME: Add decoding and test of other encodings
	// e.g. from Java:
	//             Matcher m = Pattern.compile("^[ \t\f]*#.*?coding[:=][ \t]*([-_.a-zA-Z0-9]+)", Pattern.MULTILINE).matcher(asciiHeader);
	// and then try ISO-8859-1 if UTF-8 doesn't work
	// but Go doesn't have a general decoder like Java so not sure what's best for now

	return string(b) // will soldier on in case of bad encoding
}

type WatchedFiles map[string]time.Time

func (s *Sync) ScanForFiles(dir string, ignoreFilter func(filename string) bool, matchFilter func(filename string) bool) (WatchedFiles, error) {
	w := make(WatchedFiles)

	return w, filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			s.Logf("%v ignored while traversing %s", err, path)
			return nil
		} else if entry.IsDir() {
			if ignoreFilter(path) {
				return filepath.SkipDir
			}
		} else if entry.Type().IsRegular() && !ignoreFilter(path) && matchFilter(path) {
			if info, err := entry.Info(); err == nil {
				w[path] = info.ModTime()
			} else {
				s.Logf("%v ignored while reading info for %s", err, path)
			}
		}
		return nil
	})
}

func (w WatchedFiles) AddedOrChanged(prev WatchedFiles) (o []string) {
	o = make([]string, 0)
	for path, updated := range w {
		if prev[path].IsZero() || updated.After(prev[path]) {
			o = append(o, path)
		}
	}
	return o
}

type Sync struct {
	ServerUrl        string   `env:"SERVER_URL"         envDefault:"https://train.skillerwhale.com"`
	AttendanceIdFile string   `env:"ATTENDANCE_ID_FILE" envDefault:"attendance_id"`
	Base             string   `env:"WATCHER_BASE_PATH"  envDefault:"."`
	TriggerExec      string   `env:"TRIGGER_EXEC"`
	Ignore           []string `env:"IGNORE_MATCH"       envSeparator:" ""`
	WatchedExts      []string `env:"WATCHED_EXTS"       envSeparator:" ""`
	AttendanceId     string   `env:"ATTENDANCE_ID"`
	ForcePoll        bool     `env:"FORCE_POLL"`
	DebugFlags       []string `env:"DEBUG"              envSeparator:" "`

	watcher *fsnotify.Watcher

	noPollSignal chan struct{}
	fileUpdated  chan string
	filePosted   chan string
	stopSignal   struct {
		WaitForFileUpdates chan struct{}
		PollForFileUpdates chan struct{}
		PostFileUpdates    chan struct{}
		RunTriggers        chan struct{}
	}
}

func DebugFlags() []string {
	return []string{"fsevents", "fspoll", "http", "nopings", "quiet"}
}

func (s *Sync) IsServerDisabled() bool {
	return s.ServerUrl == "DISABLED"
}

func (s *Sync) IsDebugOn(flag string) bool {
	for _, f := range s.DebugFlags {
		if f == flag {
			return true
		}
	}
	return false
}

func (s *Sync) Log(args ...any) {
	if !s.IsDebugOn("quiet") {
		log.Print(args...)
	}
}

func (s *Sync) Logf(format string, args ...any) {
	if !s.IsDebugOn("quiet") {
		log.Printf(format, args...)
	}
}

func (s *Sync) Debug(flag string, args ...any) {
	if s.IsDebugOn(flag) {
		log.Print(args...)
	}
}

func (s *Sync) Debugf(flag string, format string, args ...any) {
	if s.IsDebugOn(flag) {
		log.Printf(format, args...)
	}
}

func (s *Sync) MatchesExts(path string) bool {
	for _, ext := range s.WatchedExts {
		if strings.HasSuffix(path, "."+ext) {
			return true
		}
	}
	return false
}

func (s *Sync) Close() {
	s.stopSignal.WaitForFileUpdates <- struct{}{}
	s.stopSignal.PollForFileUpdates <- struct{}{}
	s.stopSignal.PostFileUpdates <- struct{}{}
	s.stopSignal.RunTriggers <- struct{}{}
}

func (s *Sync) ignorable(path string) bool {
	for _, pattern := range s.Ignore {
		matched, err := filepath.Match(pattern, path)
		fatalIfSet(err)
		if matched {
			return true
		}
	}
	return false
}

func (s *Sync) WatchDirectory(pathBase string) error {
	return filepath.WalkDir(pathBase, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			s.Logf("%v ignored while traversing %s", err, path)
			return nil
		} else if d.IsDir() {
			if s.ignorable(path) {
				return filepath.SkipDir
			} else {
				s.Debug("fsevents", "watcher add directory:", path)
				if err := s.watcher.Add(path); err != nil {
					s.Log("ERROR watcher add directory", path, err)
					return err
				}
			}
		}
		return nil
	})
}

var httpClient = &http.Client{
	Timeout: time.Second * time.Duration(5),
}

const CLIENT_ERROR = Error("Server rejected our request - probably invalid attendance_id")
const SERVER_ERROR = Error("server not working")

func (s *Sync) postJSON(endpoint, data string) error {
	url := s.ServerUrl + "/attendances/" + s.AttendanceId + "/" + endpoint

	if s.IsServerDisabled() {
		s.Logf("Not posting %s - %d bytes", url, len(data))
		return nil
	}

	s.Debugf("http", "POST %s %d bytes", url, len(data))
	resp, err := httpClient.Post(url, "application/json", bytes.NewReader(([]byte)(data)))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	if resp.StatusCode >= 400 && resp.StatusCode < 500 {
		// log response body
		defer resp.Body.Close()
		body, _ := ioutil.ReadAll(resp.Body)
		s.Logf("We tried to post %d bytes to %s but server gave us status %d and body \"%s\"", len(data), endpoint, resp.StatusCode, body[0:100])
		return CLIENT_ERROR
	}
	return SERVER_ERROR
}

func (s *Sync) PostPing() error {
	return s.postJSON("pings", "")
}

func (s *Sync) PostFile(path string) error {
	contents, err := readFileMax(path, MAX_UPLOAD_BYTES)
	if err != nil {
		return err
	}
	if len(contents) == 0 {
		s.Logf("Ignoring 0-length file %s", path)
		return nil
	}
	body, err := json.Marshal(
		struct {
			RelativePath string `json:"relative_path"`
			Contents     string `json:"contents"`
		}{
			strings.TrimPrefix(path, s.Base),
			decodeBytes(contents),
		},
	)
	fatalIfSet(err)
	return s.postJSON("file_snapshots", string(body))
}

func (s *Sync) WaitForFileUpdates() {
	for {
		select {
		case <-s.stopSignal.WaitForFileUpdates:
			return
		case event, ok := <-s.watcher.Events:
			if !ok {
				panic("watcher.Events channel closed unexpectedly")
			}
			s.Debugf("fsevents", "watcher event: %v", event)
				if s.noPollSignal != nil {
					s.noPollSignal <- struct{}{}
					s.noPollSignal = nil
				}

				fileInfo, err := os.Stat(event.Name)
				if err != nil {
					log.Printf("ignoring change to %s due to %v", event.Name, err)
				} else {
					if fileInfo.IsDir() {
						s.WatchDirectory(event.Name)
					} else if fileInfo.Mode().IsRegular() {
						if s.MatchesExts(event.Name) {
							s.fileUpdated <- event.Name
						}
					}
				}
			}
		case err, ok := <-s.watcher.Errors:
			if !ok {
				panic("watcher.Errors channel closed unexpectedly")
			}
			log.Println("error:", err)
		}
	}
}

func (s *Sync) PollForFileUpdates() {
	var prev WatchedFiles
	for {
		cur, err := s.ScanForFiles(s.Base, s.ignorable, s.MatchesExts)
		if err != nil {
			s.Log("Error scanning for files:", err)
		}
		if prev != nil {
			for _, f := range cur.AddedOrChanged(prev) {
				s.fileUpdated <- f
			}
		} else {
			s.Log("Now watching", s.Base, "for changes to files with extensions", s.WatchedExts, "which contains", len(cur), "matching files on startup")
		}
		prev = cur
		select {
		case <-s.stopSignal.PollForFileUpdates:
			return
		case <-s.noPollSignal:
			s.Log("Polling disabled because we're getting fsnotify events")
			return
		default:
		}
	}
}

var MAX_TIME = time.Unix(1<<63-62135596801, 999999999)

func (s *Sync) PostFileUpdates() {
	type filePost struct {
		time    time.Time
		retries int
	}
	filesToPostTimes := make(map[string]filePost)

	for {
		var nextFile string
		var nextTime time.Time = MAX_TIME
		var nextfileChannel <-chan time.Time

		for f, t := range filesToPostTimes {
			if nextTime.After(t.time) {
				nextFile = f
				nextTime = t.time
			}
		}
		if nextFile != "" {
			nextfileChannel = time.NewTimer(time.Until(nextTime)).C
		}

		select {
		case <-s.stopSignal.PostFileUpdates:
			return
		case updatedFile, ok := <-s.fileUpdated:
			if !ok {
				panic("fileUpdated channel closed unexpectedly")
			}
			filesToPostTimes[updatedFile] = filePost{
				time:    time.Now().Add(time.Duration(SEND_AFTER_MILLIS) * time.Millisecond),
				retries: 0,
			}
		case <-nextfileChannel:
			s.Logf("Posting %s", nextFile)
			if err := s.PostFile(nextFile); err == nil {
				delete(filesToPostTimes, nextFile)
				if s.filePosted != nil {
					s.filePosted <- nextFile
				}
			} else {
				switch err {
				case CLIENT_ERROR:
					log.Fatal("Server has invalidated our attendance_id")
				case TOO_LARGE_ERROR:
					delete(filesToPostTimes, nextFile)
					s.Log("Giving up on", nextFile+":", err)
				default:
					// retry
					v := filesToPostTimes[nextFile]
					if v.retries == 5 {
						s.Logf("Gave up posting %s (%v)", nextFile, err)
						delete(filesToPostTimes, nextFile)
						return
					}
					v.retries += 1
					v.time = v.time.Add(time.Duration(v.retries*(1000+(rand.Int()%1500))) * time.Millisecond)
					filesToPostTimes[nextFile] = v
				}
			}
		}
	}
}

func (s *Sync) RunTriggers() {
	type finisher struct {
		c   *exec.Cmd
		out []byte
		err error
	}
	finishers := make(chan finisher)
	for {
		select {
		case <-s.stopSignal.RunTriggers:
			return
		case file, ok := <-s.filePosted:
			if !ok {
				panic("filePosted channel closed unexpectedly")
			}
			ctx, _ := context.WithTimeout(context.Background(), MAX_TRIGGER_TIME*time.Millisecond)
			go func() {
				cmd := exec.CommandContext(ctx, s.TriggerExec, file)
				out, err := cmd.CombinedOutput()
				finishers <- finisher{cmd, out, err}
			}()
		case f := <-finishers:
			if f.err != nil {
				s.Logf("trigger %v failed (%v)", f.c.Args, f.err)
			}
		}
	}
}

func (s *Sync) readAttendanceIdFile() (string, error) {
	contents, err := readFileMax(s.AttendanceIdFile, 100)
	if err != nil {
		return "", err
	}
	return regexp.MustCompile(`\s+`).ReplaceAllLiteralString(string(contents), ""), nil
}

func (s *Sync) WaitForAttendanceId() error {
	incomingId := make(chan string)
	listener, err := net.Listen("tcp", ":9494")
	if err != nil {
		return err
	}
	go (&http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			q := r.URL.Query()
			if r.URL.Path != "/set" {
				http.NotFound(w, r)
			} else if q["id"] == nil || len(q["id"]) == 0 || q["redirect"] == nil || len(q["redirect"]) == 0 {
				http.Error(w, "Must supply id and redirect parameters", 400)
			} else {
				incomingId <- q["id"][0]
				http.Redirect(w, r, q["redirect"][0], 302)
			}
		}),
	}).Serve(listener)
	defer listener.Close()

	var prompted bool
	for newId := ""; ; {
		select {
		case newId = <-incomingId:
		case <-time.NewTimer(time.Second / 2).C:
			newId, err = s.readAttendanceIdFile()
			if !errors.Is(err, fs.ErrNotExist) {
				fatalIfSet(err)
			}
		}
		if newId != s.AttendanceId {
			s.AttendanceId = newId
			switch s.PostPing() {
			case CLIENT_ERROR:
				continue
			case nil:
				return nil
			default:
				return err
			}
		}
		if !prompted {
			s.Logf("Write attendance_id to local file %s or GET http://localhost:9494/set?id=xxxxxxx&redirect=https://skillerwhale.com/", s.AttendanceIdFile)
			prompted = true
		}
	}
}

func (s *Sync) Run() {
	wg := sync.WaitGroup{}
	runIt := func(doIt func()) {
		wg.Add(1)
		go func() { defer wg.Done(); doIt() }()
	}
	if !s.IsDebugOn("nopings") {
	runIt(func() {
		for {
			fatalIfSet(s.PostPing())
			time.Sleep(time.Duration(PING_EVERY_MILLIS) * time.Millisecond)
		}
	})
	}
	runIt(s.PollForFileUpdates)
	if !s.ForcePoll {
		runIt(s.WaitForFileUpdates)
	}
	runIt(s.PostFileUpdates)
	if s.TriggerExec != "" {
		runIt(s.RunTriggers)
	}
	wg.Wait()
}

func InitFromEnv() (s Sync, err error) {
	if err = env.Parse(&s); err != nil {
		return s, err
	}

nextFlag:
	for _, f := range s.DebugFlags {
		for _, g := range DebugFlags() {
			if f == g {
				continue nextFlag
			}
		}
		return s, fmt.Errorf("unknown debug flag %s", f)
	}

	s.fileUpdated = make(chan string)
	s.noPollSignal = make(chan struct{})
	s.stopSignal.WaitForFileUpdates = make(chan struct{}, 1)
	s.stopSignal.PollForFileUpdates = make(chan struct{}, 1)
	s.stopSignal.PostFileUpdates = make(chan struct{}, 1)
	s.stopSignal.RunTriggers = make(chan struct{}, 1)

	if s.Base, err = filepath.Abs(s.Base); err != nil {
		return s, err
	}

	if !s.IsServerDisabled() {
	_, err = httpClient.Get(s.ServerUrl)
	if err != nil {
		return s, err
	}
	// resp.StatusCode
	} else {
		s.AttendanceId = "attendance_id"
	}

	if len(s.WatchedExts) == 0 {
		return s, fmt.Errorf("WATCHED_EXTS not set")
	}
	if s.watcher, err = fsnotify.NewWatcher(); err != nil {
		return s, err
	}
	for _, pattern := range s.Ignore {
		if _, err := filepath.Match(pattern, "/"); err != nil {
			return s, fmt.Errorf("bad pattern in IGNORE_MATCH: %s", pattern)
		}
	}

	if s.AttendanceIdFile != "" {
		if s.AttendanceIdFile, err = filepath.Abs(s.AttendanceIdFile); err != nil {
			return s, err
		}
	}
	if s.TriggerExec != "" {
		s.filePosted = make(chan string)
		if s.TriggerExec, err = filepath.Abs(s.TriggerExec); err != nil {
			return s, err
		}
	}

	if !s.ForcePoll {
		if err := s.WatchDirectory(s.Base); err != nil {
			return s, err
		}
	} else {
		s.Log("Forced polling mode, will not try to listen for filesystem events")
	}

	return s, err
}

//go:generate ./gen_version.sh
//go:embed version.txt
var Version string

func main() {
	log.Printf("SkillerWhaleSync %s", Version)
	rand.NewSource(time.Now().UnixNano())
	sync, err := InitFromEnv()
	fatalIfSet(err)
	defer sync.Close()
	if sync.IsServerDisabled() {
	if sync.AttendanceId == "" {
		fatalIfSet(sync.WaitForAttendanceId())
	} else {
		fatalIfSet(sync.PostPing())
	}
		sync.Log("Valid attendance_id", sync.AttendanceId)
	}
	sync.Run()
}
