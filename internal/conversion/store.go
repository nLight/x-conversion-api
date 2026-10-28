package conversion

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"
)

var ErrConflict = errors.New("event_id already exists with different content")
var ErrNotFound = errors.New("event not found")

type Record struct {
	AppKey      string      `json:"app_key"`
	EventID     string      `json:"event_id"`
	Fingerprint string      `json:"fingerprint,omitempty"`
	Payload     *Conversion `json:"payload,omitempty"`
	State       string      `json:"state"`
	Attempts    int         `json:"attempts"`
	NextAttempt time.Time   `json:"next_attempt,omitempty"`
	UpdatedAt   time.Time   `json:"updated_at"`
	HTTPStatus  int         `json:"x_http_status,omitempty"`
	DebugID     string      `json:"debug_id,omitempty"`
	ErrorCode   string      `json:"error_code,omitempty"`
}

// Store is a durable file outbox for a single service instance. Each record is
// fsynced and atomically replaced. A process lock prevents competing dispatchers.
// Sent tombstones retain the fingerprint but drop all matching identifiers.
type Store struct {
	mu      sync.Mutex
	dir     string
	lock    *os.File
	records map[string]Record
}

func OpenStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(filepath.Join(dir, ".lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, fmt.Errorf("outbox is already in use: %w", err)
	}
	s := &Store{dir: dir, lock: lock, records: map[string]Record{}}
	entries, err := os.ReadDir(dir)
	if err != nil {
		s.Close()
		return nil, err
	}
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		body, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		var record Record
		if err == nil {
			err = json.Unmarshal(body, &record)
		}
		if err != nil || entry.Name() != recordKey(record.AppKey, record.EventID)+".json" || !validRecord(record) {
			s.Close()
			return nil, fmt.Errorf("invalid outbox record %s", entry.Name())
		}
		if record.State == "sending" {
			// A crash after the HTTP request may have happened after X accepted it.
			// The supplied X API has no documented idempotency key, so do not replay.
			record.State, record.ErrorCode = "uncertain", "interrupted_delivery"
			record.UpdatedAt = time.Now().UTC()
			if err := s.save(record); err != nil {
				s.Close()
				return nil, err
			}
		}
		s.records[recordKey(record.AppKey, record.EventID)] = record
	}
	return s, nil
}

func validRecord(r Record) bool {
	validState := r.State == "pending" || r.State == "sending" || r.State == "sent" || r.State == "failed" || r.State == "uncertain"
	return r.AppKey != "" && r.EventID != "" && len(r.Fingerprint) == 64 && validState && (r.State == "sent" || r.Payload != nil)
}

func (s *Store) Close() error {
	syscall.Flock(int(s.lock.Fd()), syscall.LOCK_UN)
	return s.lock.Close()
}

func recordKey(app, id string) string {
	sum := sha256.Sum256([]byte(app + "\x00" + id))
	return hex.EncodeToString(sum[:])
}

func (s *Store) save(record Record) error {
	f, err := os.CreateTemp(s.dir, ".pending-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = json.NewEncoder(f).Encode(record); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Rename(f.Name(), filepath.Join(s.dir, recordKey(record.AppKey, record.EventID)+".json")); err != nil {
		return err
	}
	dir, err := os.Open(s.dir)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func (s *Store) Enqueue(app, id string, payload Conversion, now time.Time) (Record, error) {
	return s.enqueue(app, id, payload, now, false)
}

// EnqueueMobile keeps the first observation: an HTTP retry may arrive from a new
// IP address. IDs are derived by the collector, never arbitrary client event IDs.
func (s *Store) EnqueueMobile(app, id string, payload Conversion, now time.Time) (Record, error) {
	return s.enqueue(app, id, payload, now, true)
}

func (s *Store) enqueue(app, id string, payload Conversion, now time.Time, first bool) (Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	body, err := json.Marshal(payload)
	if err != nil {
		return Record{}, err
	}
	digest := sha256.Sum256(body)
	fingerprint := hex.EncodeToString(digest[:])
	key := recordKey(app, id)
	if existing, ok := s.records[key]; ok {
		if !first && existing.Fingerprint != fingerprint {
			return Record{}, ErrConflict
		}
		return publicRecord(existing), nil
	}
	if first && len(s.records) >= 100000 {
		return Record{}, fmt.Errorf("mobile outbox capacity reached")
	}
	r := Record{AppKey: app, EventID: id, Fingerprint: fingerprint, Payload: &payload,
		State: "pending", NextAttempt: now, UpdatedAt: now}
	if err := s.save(r); err != nil {
		return Record{}, err
	}
	s.records[key] = r
	return publicRecord(r), nil
}

func publicRecord(r Record) Record {
	r.Payload, r.Fingerprint = nil, ""
	return r
}

func (s *Store) Get(app, id string) (Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.records[recordKey(app, id)]
	if !ok {
		return Record{}, ErrNotFound
	}
	return publicRecord(r), nil
}

func (s *Store) Claim(app string, now time.Time) ([]Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var eligible []Record
	for _, r := range s.records {
		if r.AppKey == app && r.State == "pending" && !r.NextAttempt.After(now) {
			eligible = append(eligible, r)
		}
	}
	sort.Slice(eligible, func(i, j int) bool {
		if eligible[i].NextAttempt.Equal(eligible[j].NextAttempt) {
			return eligible[i].EventID < eligible[j].EventID
		}
		return eligible[i].NextAttempt.Before(eligible[j].NextAttempt)
	})
	if len(eligible) > MaxBatch {
		eligible = eligible[:MaxBatch]
	}
	for index := range eligible {
		r := &eligible[index]
		r.State, r.UpdatedAt = "sending", now
		r.Attempts++
		if err := s.save(*r); err != nil {
			return nil, err
		}
		s.records[recordKey(app, r.EventID)] = *r
	}
	return eligible, nil
}

func (s *Store) Finish(records []Record, result Delivery, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, previous := range records {
		r := s.records[recordKey(previous.AppKey, previous.EventID)]
		r.State, r.ErrorCode, r.HTTPStatus, r.DebugID = result.State, result.Code, result.HTTPStatus, result.DebugID
		r.UpdatedAt, r.NextAttempt = now, time.Time{}
		if r.State == "pending" {
			r.NextAttempt = now.Add(result.RetryAfter)
		}
		if r.State == "sent" {
			r.Payload = nil
		}
		if err := s.save(r); err != nil {
			return err
		}
		s.records[recordKey(r.AppKey, r.EventID)] = r
	}
	return nil
}

func (s *Store) Retry(app, id string, now time.Time) (Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := recordKey(app, id)
	r, ok := s.records[key]
	if !ok {
		return Record{}, ErrNotFound
	}
	if r.State != "failed" && r.State != "uncertain" {
		return Record{}, fmt.Errorf("only failed or uncertain events can be retried")
	}
	r.State, r.ErrorCode, r.NextAttempt, r.UpdatedAt = "pending", "", now, now
	if err := s.save(r); err != nil {
		return Record{}, err
	}
	s.records[key] = r
	return publicRecord(r), nil
}
