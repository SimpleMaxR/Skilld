package skill

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// FileEntry represents a normalized file inside a Skill bundle.
type FileEntry struct {
	Encoding string `json:"encoding"`
	Content  string `json:"content"`
	Mode     string `json:"mode"`
}

// UnmarshalJSON accepts either a plain UTF-8 string or an object {encoding, content, mode}.
func (f *FileEntry) UnmarshalJSON(data []byte) error {
	var asStr string
	if err := json.Unmarshal(data, &asStr); err == nil {
		f.Encoding = "utf8"
		f.Content = asStr
		f.Mode = "0644"
		return nil
	}

	var raw struct {
		Encoding string `json:"encoding"`
		Content  string `json:"content"`
		Mode     any    `json:"mode"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("invalid file entry: %w", err)
	}
	enc := "utf8"
	if raw.Encoding == "base64" {
		enc = "base64"
	}
	f.Encoding = enc
	f.Content = raw.Content
	f.Mode = NormalizeMode(raw.Mode)
	return nil
}

// NormalizeMode maps numeric or string POSIX modes into canonical "0755" or "0644".
func NormalizeMode(mode any) string {
	switch v := mode.(type) {
	case float64:
		if (int(v) & 0o111) != 0 {
			return "0755"
		}
		return "0644"
	case int:
		if (v & 0o111) != 0 {
			return "0755"
		}
		return "0644"
	case string:
		s := strings.TrimSpace(v)
		if s == "0755" || s == "755" || s == "100755" {
			return "0755"
		}
		return "0644"
	default:
		return "0644"
	}
}

// IsValidRelativePath validates a safe POSIX relative path inside a skill bundle directory.
func IsValidRelativePath(relPath string) bool {
	if relPath == "" {
		return false
	}
	if strings.ContainsRune(relPath, '\x00') || strings.ContainsRune(relPath, '\\') {
		return false
	}
	if strings.HasPrefix(relPath, "/") || strings.HasSuffix(relPath, "/") {
		return false
	}
	parts := strings.Split(relPath, "/")
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}

// NormalizeFileMap validates paths and canonicalizes file entries sorted by relative path.
func NormalizeFileMap(files map[string]FileEntry) (map[string]FileEntry, error) {
	if files == nil {
		return nil, errors.New("skill files map must not be nil")
	}
	out := make(map[string]FileEntry, len(files))
	for relPath, entry := range files {
		if !IsValidRelativePath(relPath) {
			return nil, fmt.Errorf("invalid relative skill file path: %q", relPath)
		}
		enc := "utf8"
		if entry.Encoding == "base64" {
			enc = "base64"
		}
		out[relPath] = FileEntry{
			Encoding: enc,
			Content:  entry.Content,
			Mode:     NormalizeMode(entry.Mode),
		}
	}
	return out, nil
}

// ComputeFilesHash computes the deterministic SHA-256 fingerprint compatible with the Node Agent CLI.
func ComputeFilesHash(files map[string]FileEntry) (string, error) {
	canonical, err := NormalizeFileMap(files)
	if err != nil {
		return "", err
	}
	keys := make([]string, 0, len(canonical))
	for k := range canonical {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	h := sha256.New()
	for _, relPath := range keys {
		entry := canonical[relPath]
		h.Write([]byte(relPath))
		h.Write([]byte{0})
		h.Write([]byte(entry.Encoding))
		h.Write([]byte{0})
		h.Write([]byte(entry.Mode))
		h.Write([]byte{0})
		h.Write([]byte(entry.Content))
		h.Write([]byte{'\n'})
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

var (
	reNonSlugChars = regexp.MustCompile(`[^a-z0-9_-]+`)
	reMultiDash    = regexp.MustCompile(`-{2,}`)
)

// SlugifySkillID converts an arbitrary skill name into a URL/filesystem-safe slug ID.
func SlugifySkillID(input string) string {
	cleaned := strings.ToLower(strings.TrimSpace(input))
	cleaned = reNonSlugChars.ReplaceAllString(cleaned, "-")
	cleaned = reMultiDash.ReplaceAllString(cleaned, "-")
	cleaned = strings.Trim(cleaned, "-_")
	if cleaned == "" {
		return "skill"
	}
	return cleaned
}

// ShortHash strips the sha256: prefix and returns the leading length characters.
func ShortHash(hash string, length int) string {
	if hash == "" {
		return "—"
	}
	raw := strings.TrimPrefix(hash, "sha256:")
	if len(raw) <= length {
		return raw
	}
	return raw[:length]
}

// ISONow returns a millisecond-precision UTC ISO-8601 timestamp matching JS Date.toISOString().
func ISONow() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
}

// GenerateID produces a collision-resistant UUIDv4 identifier with optional prefix.
func GenerateID(prefix string) string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		now := time.Now().UnixNano()
		hexStr := fmt.Sprintf("%032x", now)
		if prefix != "" {
			return prefix + "_" + hexStr
		}
		return hexStr
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	uuidStr := fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
	if prefix != "" {
		return prefix + "_" + uuidStr
	}
	return uuidStr
}
