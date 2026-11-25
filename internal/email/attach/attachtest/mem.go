// Package attachtest is an in-memory attach.Store for tests. It records every
// byte written, so a test can assert what did and did not reach the store.
package attachtest

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/change-fabric/moose/internal/email/attach"
)

// Mem is an in-memory attach.Store.
type Mem struct {
	mu      sync.Mutex
	objs    map[string][]byte
	mod     map[string]time.Time
	Now     func() time.Time
	deleted []string
	puts    int

	// PutFailAfter, when positive, makes Put land that many bytes and then
	// fail, the way an interrupted multipart upload leaves parts behind.
	PutFailAfter int
	// PresignErr makes PresignGet fail.
	PresignErr error
	// DeleteErr, when set, is returned by Delete for the keys it names.
	DeleteErr map[string]error
	// LastDownloadName is the name the last PresignGet was asked for.
	LastDownloadName string
}

// NewMem returns an empty store whose objects are stamped with now().
func NewMem(now func() time.Time) *Mem {
	return &Mem{objs: map[string][]byte{}, mod: map[string]time.Time{}, Now: now, DeleteErr: map[string]error{}}
}

var _ attach.Store = (*Mem)(nil)

// Seed adds an object with an explicit modification time.
func (m *Mem) Seed(key string, data []byte, modified time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objs[key] = append([]byte(nil), data...)
	m.mod[key] = modified
}

func (m *Mem) Put(_ context.Context, key string, body io.Reader) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.puts++
	var buf bytes.Buffer
	var err error
	if m.PutFailAfter > 0 {
		_, _ = io.CopyN(&buf, body, int64(m.PutFailAfter))
		m.objs[key] = buf.Bytes() // the partial upload landed
		m.mod[key] = m.Now()
		return errors.New("attachtest: upload interrupted")
	}
	if _, err = io.Copy(&buf, body); err != nil {
		m.objs[key] = buf.Bytes()
		m.mod[key] = m.Now()
		return err
	}
	m.objs[key] = buf.Bytes()
	m.mod[key] = m.Now()
	return nil
}

func (m *Mem) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.DeleteErr[key]; err != nil {
		return err
	}
	delete(m.objs, key)
	delete(m.mod, key)
	m.deleted = append(m.deleted, key)
	return nil
}

func (m *Mem) PresignGet(_ context.Context, key string, ttl time.Duration, name string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.PresignErr != nil {
		return "", m.PresignErr
	}
	m.LastDownloadName = name
	return "https://bucket.example/" + key + "?X-Amz-Expires=" + ttl.String(), nil
}

func (m *Mem) List(_ context.Context, prefix string, fn func(attach.Object) error) error {
	m.mu.Lock()
	keys := make([]string, 0, len(m.objs))
	for k := range m.objs {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	objs := make([]attach.Object, 0, len(keys))
	for _, k := range keys {
		objs = append(objs, attach.Object{Key: k, Modified: m.mod[k]})
	}
	m.mu.Unlock()
	for _, o := range objs {
		if err := fn(o); err != nil {
			return err
		}
	}
	return nil
}

// Keys returns the keys currently stored, sorted.
func (m *Mem) Keys() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	keys := make([]string, 0, len(m.objs))
	for k := range m.objs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Object returns a copy of the bytes at key.
func (m *Mem) Object(key string) ([]byte, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.objs[key]
	return append([]byte(nil), b...), ok
}

// Deleted returns the keys Delete removed, in order.
func (m *Mem) Deleted() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.deleted...)
}

// Puts returns how many times Put was called.
func (m *Mem) Puts() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.puts
}

// Contains reports whether any stored object holds sub.
func (m *Mem) Contains(sub string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, b := range m.objs {
		if bytes.Contains(b, []byte(sub)) {
			return true
		}
	}
	return false
}
