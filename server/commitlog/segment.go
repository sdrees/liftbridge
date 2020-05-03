package commitlog

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/pkg/errors"
)

const (
	fileFormat      = "%020d%s"
	logSuffix       = ".log"
	cleanedSuffix   = ".cleaned"
	truncatedSuffix = ".truncated"
	indexSuffix     = ".index"
)

var (
	// ErrEntryNotFound is returned when a segment search cannot find a
	// specific entry.
	ErrEntryNotFound = errors.New("entry not found")

	// ErrSegmentClosed is returned on reads/writes to a closed segment.
	ErrSegmentClosed = errors.New("segment has been closed")

	// ErrSegmentExists is returned when attempting to create a segment that
	// already exists.
	ErrSegmentExists = errors.New("segment already exists")

	// ErrSegmentReplaced is returned when attempting to read from a segment
	// that has been replaced due to log compaction. When this error is
	// encountered, operations should be retried in order to run against the
	// new segment.
	ErrSegmentReplaced = errors.New("segment was replaced")

	// ErrCommitLogDeleted is returned when attempting to read from a commit
	// log that has been deleted.
	ErrCommitLogDeleted = errors.New("commit log was deleted")

	// ErrCommitLogClosed is returned when attempting to read from a commit
	// log that has been closed.
	ErrCommitLogClosed = errors.New("commit log was closed")

	// timestamp returns the current time in Unix nanoseconds. This function
	// exists for mocking purposes.
	timestamp = func() int64 { return time.Now().UnixNano() }
)

type segment struct {
	writer         io.Writer
	reader         io.Reader
	log            *os.File
	Index          *index
	BaseOffset     int64
	firstOffset    int64
	lastOffset     int64
	firstWriteTime int64
	lastWriteTime  int64
	position       int64
	maxBytes       int64
	path           string
	suffix         string
	waiters        map[interface{}]chan struct{}
	sealed         bool
	closed         bool
	replaced       bool

	sync.RWMutex
}

func newSegment(path string, baseOffset, maxBytes int64, isNew bool, suffix string) (*segment, error) {
	s := &segment{
		maxBytes:    maxBytes,
		BaseOffset:  baseOffset,
		firstOffset: -1,
		lastOffset:  -1,
		path:        path,
		suffix:      suffix,
		waiters:     make(map[interface{}]chan struct{}),
	}
	// If this is a new segment, ensure the file doesn't already exist.
	if isNew && exists(s.logPath()) {
		return nil, ErrSegmentExists
	}
	log, err := os.OpenFile(s.logPath(), os.O_RDWR|os.O_CREATE|os.O_APPEND, 0666)
	if err != nil {
		return nil, errors.Wrap(err, "open file failed")
	}
	info, err := log.Stat()
	if err != nil {
		return nil, errors.Wrap(err, "stat file failed")
	}
	s.log = log
	s.position = info.Size()
	s.writer = log
	s.reader = log
	err = s.setupIndex()
	return s, err
}

// setupIndex creates and initializes an index.
// Initialization is:
// - Initialize index position
// - Initialize firstOffset/lastOffset
// - Initialize firstWriteTime/lastWriteTime
func (s *segment) setupIndex() (err error) {
	s.Index, err = newIndex(options{
		path:       s.indexPath(),
		baseOffset: s.BaseOffset,
	})
	if err != nil {
		return err
	}
	lastEntry, err := s.Index.InitializePosition()
	if err != nil {
		return err
	}
	// If lastEntry is nil, the index is empty.
	if lastEntry != nil {
		s.lastOffset = lastEntry.Offset
		s.lastWriteTime = lastEntry.Timestamp
		// Read the first entry to get firstOffset and firstWriteTime.
		var firstEntry entry
		if err := s.Index.ReadEntryAtFileOffset(&firstEntry, 0); err != nil {
			return err
		}
		s.firstOffset = firstEntry.Offset
		s.firstWriteTime = firstEntry.Timestamp
	}
	return nil
}

// CheckSplit determines if a new log segment should be rolled out either
// because this segment is full or LogRollTime has passed since the first
// message was written to the segment.
func (s *segment) CheckSplit(logRollTime time.Duration) bool {
	s.RLock()
	defer s.RUnlock()
	if s.position >= s.maxBytes {
		return true
	}
	if logRollTime == 0 || s.firstWriteTime == 0 {
		// Don't roll a new segment if there have been no writes to the segment
		// or LogRollTime is disabled.
		return false
	}
	// Check if LogRollTime has passed since first write.
	return timestamp()-s.firstWriteTime >= int64(logRollTime)
}

// Seal a segment from being written to. This is called on the former active
// segment after a new segment is rolled. This is a no-op if the segment is
// already sealed.
func (s *segment) Seal() {
	s.Lock()
	defer s.Unlock()
	if s.sealed {
		return
	}
	s.sealed = true
	// Notify any readers waiting for data.
	s.notifyWaiters()
	s.Index.Shrink() // nolint: errcheck
}

func (s *segment) NextOffset() int64 {
	s.RLock()
	defer s.RUnlock()
	// If the segment hasn't been written to, the next offset should be the
	// base offset.
	if s.lastOffset == -1 {
		return s.BaseOffset
	}
	return s.lastOffset + 1
}

func (s *segment) FirstOffset() int64 {
	s.RLock()
	defer s.RUnlock()
	return s.firstOffset
}

func (s *segment) LastOffset() int64 {
	s.RLock()
	defer s.RUnlock()
	return s.lastOffset
}

func (s *segment) Position() int64 {
	s.RLock()
	defer s.RUnlock()
	return s.position
}

func (s *segment) IsEmpty() bool {
	s.RLock()
	defer s.RUnlock()
	return s.firstOffset == -1
}

func (s *segment) MessageCount() int64 {
	s.RLock()
	defer s.RUnlock()
	return s.Index.CountEntries()
}

func (s *segment) WriteMessageSet(ms []byte, entries []*entry) error {
	s.Lock()
	defer s.Unlock()
	if _, err := s.write(ms, entries); err != nil {
		return err
	}
	return s.Index.writeEntries(entries)
}

// write a byte slice to the log at the current position. This increments the
// offset as well as sets the position to the new tail.
func (s *segment) write(p []byte, entries []*entry) (n int, err error) {
	if s.closed {
		return 0, ErrSegmentClosed
	}
	n, err = s.writer.Write(p)
	if err != nil {
		return n, errors.Wrap(err, "log write failed")
	}
	s.position += int64(n)
	if s.firstWriteTime == 0 {
		first := entries[0]
		s.firstOffset = first.Offset
		s.firstWriteTime = first.Timestamp
	}
	last := entries[len(entries)-1]
	s.lastOffset = last.Offset
	s.lastWriteTime = last.Timestamp
	s.notifyWaiters()
	return n, nil
}

func (s *segment) ReadAt(p []byte, off int64) (n int, err error) {
	s.RLock()
	defer s.RUnlock()
	if s.closed {
		if s.replaced {
			return 0, ErrSegmentReplaced
		}
		return 0, ErrSegmentClosed
	}
	return s.log.ReadAt(p, off)
}

func (s *segment) notifyWaiters() {
	for r, ch := range s.waiters {
		close(ch)
		delete(s.waiters, r)
	}
}

func (s *segment) WaitForLEO(waiter interface{}, leo int64) <-chan struct{} {
	s.Lock()
	defer s.Unlock()
	if s.lastOffset != leo {
		ch := make(chan struct{})
		close(ch)
		return ch
	}
	return s.waitForData(waiter, s.position)
}
func (s *segment) WaitForData(waiter interface{}, pos int64) <-chan struct{} {
	s.Lock()
	ch := s.waitForData(waiter, pos)
	s.Unlock()
	return ch
}

func (s *segment) waitForData(waiter interface{}, pos int64) <-chan struct{} {
	// Check if we're already registered.
	wait, ok := s.waiters[waiter]
	if ok {
		return wait
	}
	wait = make(chan struct{})
	// Check if data has been written and/or the segment was filled.
	if s.position > pos || s.position >= s.maxBytes {
		close(wait)
	} else {
		s.waiters[waiter] = wait
	}
	return wait
}

func (s *segment) removeWaiter(waiter interface{}) {
	s.Lock()
	delete(s.waiters, waiter)
	s.Unlock()
}

// Close a segment such that it can no longer be read from or written to. This
// operation is idempotent.
func (s *segment) Close() error {
	s.Lock()
	defer s.Unlock()
	return s.close()
}

func (s *segment) close() error {
	if s.closed {
		return nil
	}
	if err := s.log.Close(); err != nil {
		return err
	}
	if err := s.Index.Close(); err != nil {
		return err
	}
	s.closed = true
	return nil
}

// Cleaned creates a cleaned segment for this segment.
func (s *segment) Cleaned() (*segment, error) {
	return newSegment(s.path, s.BaseOffset, s.maxBytes, false, cleanedSuffix)
}

// Truncated creates a truncated segment for this segment.
func (s *segment) Truncated() (*segment, error) {
	return newSegment(s.path, s.BaseOffset, s.maxBytes, false, truncatedSuffix)
}

// Replace replaces the given segment with the callee.
func (s *segment) Replace(old *segment) error {
	s.Lock()
	defer s.Unlock()
	old.Lock()
	defer old.Unlock()
	if err := old.close(); err != nil {
		return err
	}
	if err := s.close(); err != nil {
		return err
	}
	if err := os.Rename(s.logPath(), old.logPath()); err != nil {
		return err
	}
	if err := os.Rename(s.indexPath(), old.indexPath()); err != nil {
		return err
	}
	s.suffix = ""
	log, err := os.OpenFile(s.logPath(), os.O_RDWR|os.O_CREATE|os.O_APPEND, 0666)
	if err != nil {
		return errors.Wrap(err, "open file failed")
	}
	s.log = log
	s.writer = log
	s.reader = log
	s.closed = false
	old.replaced = true
	return s.setupIndex()
}

// findEntry returns the first entry whose offset is greater than or equal to
// the given offset.
func (s *segment) findEntry(offset int64) (e *entry, err error) {
	s.RLock()
	defer s.RUnlock()
	e = &entry{}
	n := int(s.Index.Position() / entryWidth)
	idx := sort.Search(n, func(i int) bool {
		if err := s.Index.ReadEntryAtFileOffset(e, int64(i*entryWidth)); err != nil {
			panic(err)
		}
		return e.Offset >= offset
	})
	if idx == n {
		return nil, ErrEntryNotFound
	}
	err = s.Index.ReadEntryAtFileOffset(e, int64(idx*entryWidth))
	return e, err
}

// findEntryByTimestamp returns the first entry whose timestamp is greater than
// or equal to the given offset.
func (s *segment) findEntryByTimestamp(timestamp int64) (e *entry, err error) {
	s.RLock()
	defer s.RUnlock()
	e = &entry{}
	n := int(s.Index.Position() / entryWidth)
	idx := sort.Search(n, func(i int) bool {
		if err := s.Index.ReadEntryAtFileOffset(e, int64(i*entryWidth)); err != nil {
			panic(err)
		}
		return e.Timestamp >= timestamp
	})
	if idx == n {
		return nil, ErrEntryNotFound
	}
	err = s.Index.ReadEntryAtFileOffset(e, int64(idx*entryWidth))
	return e, err
}

// Delete closes the segment and then deletes its log and index files.
func (s *segment) Delete() error {
	if err := s.Close(); err != nil {
		return err
	}
	s.Lock()
	defer s.Unlock()
	if exists(s.log.Name()) {
		if err := os.Remove(s.log.Name()); err != nil {
			return err
		}
	}
	if exists(s.Index.Name()) {
		if err := os.Remove(s.Index.Name()); err != nil {
			return err
		}
	}
	return nil
}

type segmentScanner struct {
	s  *segment
	is *indexScanner
}

func newSegmentScanner(segment *segment) *segmentScanner {
	return &segmentScanner{s: segment, is: newIndexScanner(segment.Index)}
}

// Scan should be called repeatedly to iterate over the messages in the
// segment, it will return io.EOF when there are no more messages.
func (s *segmentScanner) Scan() (messageSet, *entry, error) {
	entry, err := s.is.Scan()
	if err != nil {
		return nil, nil, err
	}
	header := make(messageSet, msgSetHeaderLen)
	_, err = s.s.ReadAt(header, entry.Position)
	if err != nil {
		return nil, nil, err
	}
	payload := make([]byte, header.Size())
	_, err = s.s.ReadAt(payload, entry.Position+msgSetHeaderLen)
	if err != nil {
		return nil, nil, err
	}
	msgSet := append(header, payload...)
	return msgSet, entry, nil
}

func (s *segment) logPath() string {
	return filepath.Join(s.path, fmt.Sprintf(fileFormat, s.BaseOffset, logSuffix+s.suffix))
}

func (s *segment) indexPath() string {
	return filepath.Join(s.path, fmt.Sprintf(fileFormat, s.BaseOffset, indexSuffix+s.suffix))
}
