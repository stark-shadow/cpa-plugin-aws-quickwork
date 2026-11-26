package main

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type conversation struct {
	mu           sync.Mutex
	threadID     string
	authID       string
	messages     []chatMessage
	systemPrompt string
	lastUsed     atomic.Int64
}

func (c *conversation) touch() {
	c.lastUsed.Store(time.Now().UnixNano())
}

func (c *conversation) idle() time.Duration {
	n := c.lastUsed.Load()
	if n == 0 {
		return 0
	}
	return time.Since(time.Unix(0, n))
}

func (c *conversation) reset() {
	c.threadID = ""
	c.authID = ""
	c.messages = nil
	c.systemPrompt = ""
}

func (c *conversation) delta(msgs []chatMessage) ([]chatMessage, bool) {
	if c.threadID == "" || len(c.messages) == 0 {
		return nil, false
	}
	if len(msgs) <= len(c.messages) {
		return nil, false
	}
	for i := range c.messages {
		if !reflect.DeepEqual(c.messages[i], msgs[i]) {
			return nil, false
		}
	}
	return msgs[len(c.messages):], true
}

type conversationStore struct {
	mu      sync.Mutex
	entries map[string]*conversation
	ttl     time.Duration
	max     int
}

func newConversationStore(ttl time.Duration, maxEntries int) *conversationStore {
	if ttl <= 0 {
		ttl = 30 * time.Minute
	}
	if maxEntries < 1 {
		maxEntries = 256
	}
	return &conversationStore{
		entries: make(map[string]*conversation),
		ttl:     ttl,
		max:     maxEntries,
	}
}

// acquire locks one conversation for the duration of a request. Store lock is
// released before locking the conversation so one busy chat never blocks others.
func (s *conversationStore) acquire(key string) *conversation {
	s.mu.Lock()
	for k, c := range s.entries {
		if c.idle() > s.ttl {
			delete(s.entries, k)
		}
	}
	if len(s.entries) >= s.max {
		var oldestKey string
		var oldest time.Time
		for k, c := range s.entries {
			used := time.Unix(0, c.lastUsed.Load())
			if oldestKey == "" || used.Before(oldest) {
				oldestKey, oldest = k, used
			}
		}
		if oldestKey != "" {
			delete(s.entries, oldestKey)
		}
	}
	c := s.entries[key]
	if c == nil {
		c = &conversation{}
		s.entries[key] = c
	}
	s.mu.Unlock()

	c.mu.Lock()
	c.touch()
	return c
}

func (s *conversationStore) release(c *conversation) {
	if c == nil {
		return
	}
	c.touch()
	c.mu.Unlock()
}

func conversationKey(authID string, headers http.Header, msgs []chatMessage, fullSystemPrompt string) string {
	for _, header := range []string{"X-Conversation-Id", "X-Thread-Id"} {
		if headers != nil {
			if value := strings.TrimSpace(headers.Get(header)); value != "" {
				return "h:" + authID + "\x00" + value
			}
		}
	}
	h := sha256.New()
	h.Write([]byte(authID))
	h.Write([]byte{0})
	h.Write([]byte(fullSystemPrompt))
	for _, m := range msgs {
		role := strings.ToLower(strings.TrimSpace(m.Role))
		if role == "system" || role == "developer" {
			continue
		}
		h.Write([]byte{0})
		h.Write([]byte(role))
		h.Write([]byte{0})
		h.Write([]byte(contentText(m.Content)))
		return "c:" + hex.EncodeToString(h.Sum(nil))
	}
	for _, m := range msgs {
		h.Write([]byte{0})
		h.Write([]byte(m.Role))
		h.Write([]byte(contentText(m.Content)))
	}
	return "c:" + hex.EncodeToString(h.Sum(nil))
}
