// Copyright 2015 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package cookiejar

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"regexp"
	"sort"
	"time"

	"github.com/juju/mutex"
	"github.com/juju/utils/clock"
	"gopkg.in/errgo.v1"
)

// Save saves the cookies to the persistent cookie file.
// Before the file is written, it reads any cookies that
// have been stored from it and merges them into j.
func (j *Jar) Save() error {
	return j.save(time.Now())
}

// save is like Save but takes the current time as a parameter.
func (j *Jar) save(now time.Time) error {
	releaser, err := lockFile(j.filename)
	if err != nil {
		return errgo.Mask(err)
	}
	defer releaser.Release()
	f, err := os.OpenFile(j.filename, os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return errgo.Mask(err)
	}
	defer f.Close()
	// TODO optimization: if the file hasn't changed since we
	// loaded it, don't bother with the merge step.

	j.mu.Lock()
	defer j.mu.Unlock()
	if err := j.mergeFrom(f); err != nil {
		// The cookie file is probably corrupt.
		log.Printf("cannot read cookie file to merge it; ignoring it: %v", err)
	}
	j.deleteExpired(now)
	if err := f.Truncate(0); err != nil {
		return errgo.Notef(err, "cannot truncate file")
	}
	if _, err := f.Seek(0, 0); err != nil {
		return errgo.Mask(err)
	}
	return j.writeTo(f)
}

// load loads the cookies from j.filename. If the file does not exist,
// no error will be returned and no cookies will be loaded.
func (j *Jar) load() error {
	releaser, err := lockFile(j.filename)
	if err != nil {
		return errgo.Mask(err)
	}
	defer releaser.Release()
	f, err := os.Open(j.filename)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()
	if err := j.mergeFrom(f); err != nil {
		return errgo.Mask(err)
	}
	return nil
}

// mergeFrom reads all the cookies from r and stores them in the Jar.
func (j *Jar) mergeFrom(r io.Reader) error {
	decoder := json.NewDecoder(r)
	// Cope with old cookiejar format by just discarding
	// cookies, but still return an error if it's invalid JSON.
	var data json.RawMessage
	if err := decoder.Decode(&data); err != nil {
		if err == io.EOF {
			// Empty file.
			return nil
		}
		return err
	}
	var entries []entry
	if err := json.Unmarshal(data, &entries); err != nil {
		log.Printf("warning: discarding cookies in invalid format (error: %v)", err)
		return nil
	}
	j.merge(entries)
	return nil
}

// writeTo writes all the cookies in the jar to w
// as a JSON array.
func (j *Jar) writeTo(w io.Writer) error {
	encoder := json.NewEncoder(w)
	entries := j.allPersistentEntries()
	if err := encoder.Encode(entries); err != nil {
		return err
	}
	return nil
}

// allPersistentEntries returns all the entries in the jar, sorted by primarly by canonical host
// name and secondarily by path length.
func (j *Jar) allPersistentEntries() []entry {
	var entries []entry
	for _, submap := range j.entries {
		for _, e := range submap {
			if e.Persistent {
				entries = append(entries, e)
			}
		}
	}
	sort.Sort(byCanonicalHost{entries})
	return entries
}

const maxRetryDuration = 2 * time.Second

var re = regexp.MustCompile(`[^a-z0-9A-Z]*`)

func lockNameFromPath(path string) (string, error) {
	if path == "" {
		return "", errors.New("path cannot be empty")
	}
	pathBytes := []byte(path)
	hash := sha256.New()
	hash.Write(pathBytes)
	sha := base64.URLEncoding.EncodeToString(hash.Sum(nil))

	if len(path) > 10 {
		sliceIndex := len(path) - 10
		path = path[sliceIndex:]
	}
	// We start with an alphabetical character, and remove non alphanumeric
	// characters so that we can't have an invalid name for the lock.
	final := re.ReplaceAllLiteralString(fmt.Sprintf("L%v%v", sha[:29], path), ``)
	return final, nil
}

func lockFile(path string) (mutex.Releaser, error) {
	retry := 100 * time.Microsecond
	startTime := time.Now()
	name, err := lockNameFromPath(path)
	if err != nil {
		return nil, err
	}
	spec := mutex.Spec{
		Name:    name,
		Clock:   clock.WallClock,
		Delay:   retry,
		Timeout: maxRetryDuration,
	}
	for {
		releaser, err := mutex.Acquire(spec)
		if err == nil {
			return releaser, nil
		}
		total := time.Since(startTime)
		if total > maxRetryDuration {
			return nil, errgo.Notef(err, "file locked for too long; giving up")
		}
		// Always have at least one try at the end of the interval.
		if remain := maxRetryDuration - total; retry > remain {
			retry = remain
		}
		time.Sleep(retry)
	}
}
