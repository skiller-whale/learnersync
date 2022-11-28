package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"math/rand"
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

const MAX_UPLOAD_BYTES = 10000000
const SEND_AFTER_MILLIS = 100
const PING_EVERY_MILLIS = 2000
const PING_WARNING_MILLIS = 5000
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

type Sync struct {
	ServerUrl        string   `env:"SERVER_URL"         envDefault:"https://train.skillerwhale.com"`
	AttendanceIdFile string   `env:"ATTENDANCE_ID_FILE" envDefault:"attendance_id"`
	Base             string   `env:"WATCHER_BASE_PATH"  envDefault:"."`
	TriggerExec      string   `env:"TRIGGER_EXEC"`
	Ignore           []string `env:"IGNORE_MATCH"       envSeparator:" ""`
	WatchedExts      []string `env:"WATCHED_EXTS"       envSeparator:" ""`
	AttendanceId     string   `env:"ATTENDANCE_ID"`

	watcher     *fsnotify.Watcher
	fileUpdated chan string
	filePosted  chan string
}

func (s *Sync) Close() {
	s.watcher.Close()
}

func (s *Sync) ignorable(path string) bool {
	for _, pattern := range s.Ignore {
		matched, err := filepath.Match(pattern, path)
		if matched || err != nil {
			if err != nil {
				panic(err) // shouldn't happen, we checked before storing it!
			}
			return true
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
		log.Printf("Ignoring 0-length file %s", path)
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
	if err != nil {
		panic(err)
	}
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
				fileInfo, err := os.Stat(event.Name)
				if err != nil {
					log.Printf("ignoring change to %s due to %v", event.Name, err)
				} else {
					if fileInfo.IsDir() {
						s.WatchDirectory(event.Name)
					} else if fileInfo.Mode().IsRegular() {
						for _, ext := range s.WatchedExts {
							if strings.HasSuffix(event.Name, "."+ext) {
								s.fileUpdated <- event.Name
								break
							}
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
			if err := s.PostFile(nextFile); err == nil {
				log.Printf("Posted %s", nextFile)
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

func (s *Sync) readAttendanceIdFile() error {
	contents, err := readFileMax(s.AttendanceIdFile, 100)
	if err != nil {
		return err
	}
	s.AttendanceId = regexp.MustCompile(`\s+`).ReplaceAllLiteralString(string(contents), "")
	if err := s.PostPing(); err != nil {
		s.AttendanceId = ""
		return err
	}
	return nil
}

func (s *Sync) WaitForAttendanceId() error {
	if err := s.readAttendanceIdFile(); err == nil {
		return nil
	}
	if err := s.watcher.Add(filepath.Dir(s.AttendanceIdFile)); err != nil {
		return err
	}
	defer s.watcher.Remove(filepath.Dir(s.AttendanceIdFile))
	for event := range s.watcher.Events {
		if event.Has(fsnotify.Write) && event.Name == s.AttendanceIdFile {
			if err := s.readAttendanceIdFile(); err != nil {
				log.Printf("attendance_id rejected %s (%v)", s.AttendanceId, err)
			} else {
				return nil
			}
		}
	}
	panic("events channel closed")
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
	runIt(s.WaitForFileUpdates)
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
	if s.Base, err = filepath.Abs(s.Base); err != nil {
		return s, err
	}

	if err := s.WatchDirectory(s.Base); err != nil {
		return s, err
	}

	return s, err
}

func main() {
	rand.NewSource(time.Now().UnixNano())
	sync, err := InitFromEnv()
	fatalIfSet(err)
	defer sync.Close()
	log.Printf("Watching %s for valid attendance_id", sync.AttendanceIdFile)
	if sync.AttendanceId == "" {
		fatalIfSet(sync.WaitForAttendanceId())
	} else {
		fatalIfSet(sync.PostPing())
	}
	log.Printf("Starting with attendance_id=%s", sync.AttendanceId)
	sync.Run()
}