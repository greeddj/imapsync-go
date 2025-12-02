// Package cache persists mailbox metadata between runs to avoid repeated IMAP scans.
package cache

import (
	"bytes"
	"crypto/sha256"
	"encoding/gob"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/emersion/go-imap"
	"github.com/greeddj/imapsync-go/internal/config"
)

// MessageInfo stores lightweight metadata about a synchronized message.
type MessageInfo struct {
	UID       uint32    // IMAP UID of the message.
	MessageID string    // Unique message identifier.
	Subject   string    // Message subject line.
	From      string    // Sender information.
	Date      time.Time // Message date.
	Size      uint32    // Message size in bytes.
}

// MailboxCache keeps cached statistics and message metadata for one folder.
type MailboxCache struct {
	Mailbox      string                  // Mailbox name.
	Messages     map[string]*MessageInfo // Messages indexed by message ID.
	UIDNext      uint32                  // Next expected UID.
	MessageCount uint32                  // Total number of messages.
	TotalSize    uint64                  // Total size of all messages.
	Updated      time.Time               // Last update timestamp.
}

// ServerCache aggregates cached mailboxes for one IMAP account.
type ServerCache struct {
	Server    string                   // IMAP server address.
	User      string                   // IMAP username.
	Mailboxes map[string]*MailboxCache // Cached mailboxes indexed by name.
	Updated   time.Time                // Last update timestamp.
}

// CacheManager coordinates loading, updating, and encrypting cache files.
type CacheManager struct {
	SourceCache *ServerCache // Cache for source server.
	DestCache   *ServerCache // Cache for destination server.
	cacheDir    string       // Directory where cache files are stored.
	cacheFile   string       // Full path to the cache file.
	encryptPass string       // Password for cache encryption.
}

// getCacheDir returns the cache directory path, creating it if necessary.
func getCacheDir() (string, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("get home directory: %w", err)
	}

	cacheDir := filepath.Join(homeDir, ".imapsync", "cache")

	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		return "", fmt.Errorf("create cache directory: %w", err)
	}

	return cacheDir, nil
}

// generateCacheFileName creates a unique cache filename based on server credentials.
func generateCacheFileName(srcServer, srcUser, dstServer, dstUser string) string {
	return fmt.Sprintf("%x.cache", sha256.Sum256([]byte(srcServer+":"+srcUser+":"+dstServer+":"+dstUser)))
}

// NewCacheManager builds a cache manager bound to the provided IMAP credentials.
func NewCacheManager(src, dst config.Credentials) (*CacheManager, error) {
	cacheDir, err := getCacheDir()
	if err != nil {
		return nil, err
	}

	cacheFile := generateCacheFileName(src.Server, src.User, dst.Server, dst.User)
	cacheFilePath := filepath.Join(cacheDir, cacheFile)

	encryptPass := fmt.Sprintf("%s:%s:%s:%s:%s:%s", src.Pass, src.User, src.Server, dst.Pass, dst.User, dst.Server)

	cm := &CacheManager{
		SourceCache: &ServerCache{
			Server:    src.Server,
			User:      src.User,
			Mailboxes: make(map[string]*MailboxCache),
			Updated:   time.Now(),
		},
		DestCache: &ServerCache{
			Server:    dst.Server,
			User:      dst.User,
			Mailboxes: make(map[string]*MailboxCache),
			Updated:   time.Now(),
		},
		cacheDir:    cacheDir,
		cacheFile:   cacheFilePath,
		encryptPass: encryptPass,
	}

	return cm, nil
}

// Load decrypts and deserializes cache contents from disk if present.
func (cm *CacheManager) Load() error {
	ciphertext, err := os.ReadFile(cm.cacheFile)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read cache file: %w", err)
	}

	plaintext, err := decrypt(ciphertext, cm.encryptPass)
	if err != nil {
		return fmt.Errorf("decrypt cache: %w", err)
	}

	buf := bytes.NewReader(plaintext)
	decoder := gob.NewDecoder(buf)

	if err := decoder.Decode(cm.SourceCache); err != nil {
		return fmt.Errorf("decode source cache: %w", err)
	}

	if err := decoder.Decode(cm.DestCache); err != nil {
		return fmt.Errorf("decode destination cache: %w", err)
	}

	return nil
}

// Save writes the current cache content to disk using authenticated encryption.
func (cm *CacheManager) Save() error {
	cm.SourceCache.Updated = time.Now()
	cm.DestCache.Updated = time.Now()

	var buf bytes.Buffer
	encoder := gob.NewEncoder(&buf)

	if err := encoder.Encode(cm.SourceCache); err != nil {
		return fmt.Errorf("encode source cache: %w", err)
	}

	if err := encoder.Encode(cm.DestCache); err != nil {
		return fmt.Errorf("encode destination cache: %w", err)
	}

	ciphertext, err := encrypt(buf.Bytes(), cm.encryptPass)
	if err != nil {
		return fmt.Errorf("encrypt cache: %w", err)
	}

	tmpFile := cm.cacheFile + ".tmp"

	if err := os.WriteFile(tmpFile, ciphertext, 0600); err != nil {
		return fmt.Errorf("write temporary cache file: %w", err)
	}

	if err := os.Rename(tmpFile, cm.cacheFile); err != nil {
		_ = os.Remove(tmpFile)
		return fmt.Errorf("rename cache file: %w", err)
	}

	return nil
}

// UpdateMailbox refreshes cached metadata for the given folder using fetched messages.
func (sc *ServerCache) UpdateMailbox(mailbox string, messages []*imap.Message) {
	cache := &MailboxCache{
		Mailbox:  mailbox,
		Messages: make(map[string]*MessageInfo),
		Updated:  time.Now(),
	}

	maxUID := uint32(0)
	var totalSize uint64
	var messageCount uint32

	for _, msg := range messages {
		if msg.Envelope == nil {
			continue
		}

		messageCount++
		totalSize += uint64(msg.Size)

		messageID := strings.Trim(msg.Envelope.MessageId, "<>")

		from := ""
		if len(msg.Envelope.From) > 0 && msg.Envelope.From[0] != nil {
			if msg.Envelope.From[0].PersonalName != "" {
				from = msg.Envelope.From[0].PersonalName
			} else if msg.Envelope.From[0].MailboxName != "" && msg.Envelope.From[0].HostName != "" {
				from = msg.Envelope.From[0].MailboxName + "@" + msg.Envelope.From[0].HostName
			}
		}

		info := &MessageInfo{
			UID:       msg.Uid,
			MessageID: messageID,
			Subject:   msg.Envelope.Subject,
			From:      from,
			Date:      msg.Envelope.Date,
			Size:      msg.Size,
		}

		if messageID != "" {
			cache.Messages[messageID] = info
		}

		if msg.Uid > maxUID {
			maxUID = msg.Uid
		}
	}

	cache.UIDNext = maxUID + 1
	cache.MessageCount = messageCount
	cache.TotalSize = totalSize
	sc.Mailboxes[mailbox] = cache
}

// GetMailbox returns the cached entry for the provided folder.
func (sc *ServerCache) GetMailbox(mailbox string) *MailboxCache {
	return sc.Mailboxes[mailbox]
}

// GetMessageIDs returns all cached message IDs for the provided folder.
func (sc *ServerCache) GetMessageIDs(mailbox string) []string {
	cache := sc.GetMailbox(mailbox)
	if cache == nil {
		return nil
	}

	ids := make([]string, 0, len(cache.Messages))
	for id := range cache.Messages {
		ids = append(ids, id)
	}

	return ids
}

// HasMessage reports whether the provided message ID exists in the cache.
func (sc *ServerCache) HasMessage(mailbox, messageID string) bool {
	cache := sc.GetMailbox(mailbox)
	if cache == nil {
		return false
	}

	_, exists := cache.Messages[messageID]
	return exists
}

// Clear removes the cache file and resets in-memory structures.
func (cm *CacheManager) Clear() error {
	if err := os.Remove(cm.cacheFile); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove cache file: %w", err)
	}

	cm.SourceCache.Mailboxes = make(map[string]*MailboxCache)
	cm.DestCache.Mailboxes = make(map[string]*MailboxCache)

	return nil
}

// GetCacheInfo returns a human-readable summary of the cached folders.
func (cm *CacheManager) GetCacheInfo() string {
	info := fmt.Sprintf("Cache file: %s\n", cm.cacheFile)

	if cm.SourceCache.Updated.IsZero() {
		info += "Source: no cached folders\n"
	} else {
		info += fmt.Sprintf("Source: %d folders cached (updated %s)\n",
			len(cm.SourceCache.Mailboxes),
			cm.SourceCache.Updated.Format("2006-01-02 15:04:05"))
	}

	if cm.DestCache.Updated.IsZero() {
		info += "Destination: no cached folders\n"
	} else {
		info += fmt.Sprintf("Destination: %d folders cached (updated %s)\n",
			len(cm.DestCache.Mailboxes),
			cm.DestCache.Updated.Format("2006-01-02 15:04:05"))
	}

	return info
}
