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

func ScanForFiles(dir string, ignoreFilter func(filename string) bool, matchFilter func(filename string) bool) (WatchedFiles, error) {
	w := make(WatchedFiles)

	return w, filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			log.Printf("%v ignored while traversing %s", err, path)
			return nil
		} else if entry.IsDir() {
			if ignoreFilter(path) {
				return filepath.SkipDir
			}
		} else if entry.Type().IsRegular() && !ignoreFilter(path) && matchFilter(path) {
			if info, err := entry.Info(); err == nil {
				w[path] = info.ModTime()
			} else {
				log.Printf("%v ignored while reading info for %s", err, path)
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
	ServerUrl        string   `env:"SERVER_URL"               envDefault:"https://train.skillerwhale.com"`
	InHostedEnv      bool     `env:"SW_RUNNING_IN_HOSTED_ENV" envDefault:"0"`
	AttendanceIdFile string   `env:"ATTENDANCE_ID_FILE"       envDefault:"attendance_id"`
	Base             string   `env:"WATCHER_BASE_PATH"        envDefault:"."`
	TriggerExec      string   `env:"TRIGGER_EXEC"`
	Ignore           []string `env:"IGNORE_MATCH"             envSeparator:" ""`
	WatchedExts      []string `env:"WATCHED_EXTS"             envSeparator:" ""`
	AttendanceId     string   `env:"ATTENDANCE_ID"`
	ForcePoll        bool     `env:"FORCE_POLL"`

	watcher *fsnotify.Watcher

	noPollSignal chan struct{}

	fileUpdated chan string
	filePosted  chan string
}

func (s *Sync) MatchesExts(path string) bool {
	if ext := filepath.Ext(path); len(ext) > 0 {
		// The path has an extension, try matching this.
		return s.isEqualToWatchedExtension(ext[1:])
	} else {
		// There is no extension, try matching the complete filename instead (e.g. Dockerfile)
		return s.isEqualToWatchedExtension(filepath.Base(path))
	}
}

// isEqualToWatchedExtension returns true if the given string matches any of the
// extensions in the WatchedExts list. The passed string should not include a
// leading period.
func (s *Sync) isEqualToWatchedExtension(str string) bool {
	for _, ext := range s.WatchedExts {
		if len(ext) > 0 && ext[0] == '.' {
			ext = ext[1:] // strip leading period if present
		}
		if str == ext {
			return true // the extension is an exact match
		}
	}
	return false
}

func (s *Sync) Close() {
	s.watcher.Close()
}

func (s *Sync) ignorable(path string) bool {
	for _, pattern := range s.Ignore {
		// Strip trailing slash from pattern to handle "node_modules/" like "node_modules"
		pattern = strings.TrimSuffix(pattern, "/")

		// First, try matching against the full path
		matched, err := filepath.Match(pattern, path)
		fatalIfSet(err)
		if matched {
			return true
		}

		// If pattern doesn't contain wildcards, also check if it matches any path component
		// This allows "node_modules" to match "/app/exercises/node_modules" and files within it
		if !strings.ContainsAny(pattern, "*?[") {
			// Check if the pattern matches the full path or any component
			for _, component := range strings.Split(path, string(filepath.Separator)) {
				if component == pattern {
					return true
				}
			}
			// Also check if the pattern appears as a path component followed by more path
			// e.g., "node_modules" should match "/app/node_modules/foo"
			pathWithSep := string(filepath.Separator) + pattern + string(filepath.Separator)
			if strings.Contains(path, pathWithSep) {
				return true
			}
			// Check if path ends with the pattern (e.g., "/app/exercises/node_modules")
			pathEndsWith := string(filepath.Separator) + pattern
			if strings.HasSuffix(path, pathEndsWith) {
				return true
			}
		}
	}
	return false
}

func (s *Sync) WatchDirectory(pathBase string) error {
	return filepath.WalkDir(pathBase, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			log.Printf("%v ignored while traversing %s", err, path)
			return nil
		} else if d.IsDir() {
			if s.ignorable(path) {
				return filepath.SkipDir
			} else {
				if err := s.watcher.Add(path); err != nil {
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
const THROTTLED_ERROR = Error("Server requested that we try later")

func (s *Sync) postJSON(endpoint, data string) error {
	url := s.ServerUrl + "/attendances/" + s.AttendanceId + "/" + endpoint
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
		body, _ := io.ReadAll(resp.Body)
		log.Printf("We tried to post %d bytes to %s but server gave us status %d and body \"%s\"", len(data), endpoint, resp.StatusCode, body[0:100])
		if resp.StatusCode == 429 {
			return THROTTLED_ERROR
		} else {
			return CLIENT_ERROR
		}
	}
	return SERVER_ERROR
}

func (s *Sync) PostPing() error {
	body, err := json.Marshal(
		struct {
			InHostedEnv bool `json:"sent_from_hosted_environment"`
		}{
			s.InHostedEnv,
		},
	)
	fatalIfSet(err)
	return s.postJSON("pings", string(body))
}

func (s *Sync) PostFile(path string) error {
	contents, err := readFileMax(path, MAX_UPLOAD_BYTES)
	if err != nil {
		return err
	}
	if len(contents) == 0 {
		log.Printf("Ignoring 0-length file %s", path)
		return nil
	}
	body, err := json.Marshal(
		struct {
			RelativePath string `json:"relative_path"`
			Contents     string `json:"contents"`
			InHostedEnv  bool   `json:"sent_from_hosted_environment"`
		}{
			strings.TrimPrefix(path, s.Base),
			decodeBytes(contents),
			s.InHostedEnv,
		},
	)
	fatalIfSet(err)
	return s.postJSON("file_snapshots", string(body))
}

func (s *Sync) WaitForFileUpdates() {
	for {
		select {
		case event, ok := <-s.watcher.Events:
			if !ok {
				panic("watcher.Events channel closed unexpectedly")
			}
			if event.Has(fsnotify.Write) {
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
		cur, err := ScanForFiles(s.Base, s.ignorable, s.MatchesExts)
		if err != nil {
			log.Println("Error scanning for files:", err)
		}
		if prev != nil {
			for _, f := range cur.AddedOrChanged(prev) {
				s.fileUpdated <- f
			}
		} else {
			log.Println("Now watching", s.Base, "for changes to files with extensions", s.WatchedExts, "which contains", len(cur), "matching files on startup")
		}
		prev = cur
		select {
		case <-s.noPollSignal:
			log.Println("Polling disabled because we're getting fsnotify events")
			return
		case <-time.After(POLL_INTERVAL_MILLIS * time.Millisecond):
			// Wait for the POLL_INTERVAL to elapse, and then continue
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
		case updatedFile, ok := <-s.fileUpdated:
			if !ok {
				panic("fileUpdated channel closed unexpectedly")
			}
			filesToPostTimes[updatedFile] = filePost{
				time:    time.Now().Add(time.Duration(SEND_AFTER_MILLIS) * time.Millisecond),
				retries: 0,
			}
		case <-nextfileChannel:
			log.Printf("Posting %s", nextFile)
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
					log.Println("Giving up on", nextFile+":", err)
				default:
					// retry
					v := filesToPostTimes[nextFile]
					if v.retries == 5 {
						log.Printf("Gave up posting %s (%v)", nextFile, err)
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
				log.Printf("trigger %v failed (%v)", f.c.Args, f.err)
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
	var httpEnabled = !s.InHostedEnv
	var err error = nil
	incomingId := make(chan string)
	if httpEnabled {
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
	}

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
			if httpEnabled {
				log.Printf("Write attendance_id to local file %s or GET http://localhost:9494/set?id=xxxxxxx&redirect=https://skillerwhale.com/", s.AttendanceIdFile)
			} else {
				log.Printf("Write attendance_id to local file %s", s.AttendanceIdFile)
			}
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
	runIt(func() {
		for {
			fatalIfSet(s.PostPing())
			time.Sleep(time.Duration(PING_EVERY_MILLIS) * time.Millisecond)
		}
	})
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
	s.fileUpdated = make(chan string)
	s.noPollSignal = make(chan struct{})

	if s.Base, err = filepath.Abs(s.Base); err != nil {
		return s, err
	}

	_, err = httpClient.Get(s.ServerUrl)
	if err != nil {
		return s, err
	}
	// resp.StatusCode

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
		log.Println("Forced polling mode, will not try to listen for filesystem events")
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
	if sync.AttendanceId == "" {
		fatalIfSet(sync.WaitForAttendanceId())
	} else {
		fatalIfSet(sync.PostPing())
	}
	log.Println("Valid attendance_id", sync.AttendanceId)
	sync.Run()
}
