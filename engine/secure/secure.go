// Package secure holds the cryptographic primitives used by the engine:
// envelope encryption for stored secrets, streaming encryption for backups,
// password hashing and random token generation.
package secure

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/argon2"
)

const keySize = 32

// Keyring wraps and unwraps data keys. The local implementation keeps the
// master key in memory; a KMS or Vault backed implementation can replace it
// without touching stored data formats.
type Keyring interface {
	WrapKey(dek, aad []byte) ([]byte, error)
	UnwrapKey(wrapped, aad []byte) ([]byte, error)
}

type localKeyring struct{ aead cipher.AEAD }

// LoadKeyring returns the master keyring. The key comes from DEDIE_MASTER_KEY
// (base64, 32 bytes) when set, otherwise from <dataDir>/master.key, which is
// generated with 0600 permissions on first use.
func LoadKeyring(dataDir string) (Keyring, error) {
	var key []byte
	if env := strings.TrimSpace(os.Getenv("DEDIE_MASTER_KEY")); env != "" {
		k, err := base64.StdEncoding.DecodeString(env)
		if err != nil || len(k) != keySize {
			return nil, errors.New("DEDIE_MASTER_KEY must be 32 bytes, base64 encoded")
		}
		key = k
	} else {
		path := filepath.Join(dataDir, "master.key")
		raw, err := os.ReadFile(path)
		switch {
		case err == nil:
			k, derr := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw)))
			if derr != nil || len(k) != keySize {
				return nil, fmt.Errorf("%s is not a valid master key", path)
			}
			if fi, serr := os.Stat(path); serr == nil && fi.Mode().Perm()&0o077 != 0 {
				return nil, fmt.Errorf("%s must not be readable by group or others (chmod 600)", path)
			}
			key = k
		case os.IsNotExist(err):
			key = make([]byte, keySize)
			if _, err := rand.Read(key); err != nil {
				return nil, err
			}
			enc := base64.StdEncoding.EncodeToString(key) + "\n"
			if err := os.WriteFile(path, []byte(enc), 0o600); err != nil {
				return nil, err
			}
		default:
			return nil, err
		}
	}
	aead, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	return &localKeyring{aead: aead}, nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func (k *localKeyring) WrapKey(dek, aad []byte) ([]byte, error) {
	nonce := make([]byte, k.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return k.aead.Seal(nonce, nonce, dek, aad), nil
}

func (k *localKeyring) UnwrapKey(wrapped, aad []byte) ([]byte, error) {
	ns := k.aead.NonceSize()
	if len(wrapped) < ns {
		return nil, errors.New("wrapped key too short")
	}
	return k.aead.Open(nil, wrapped[:ns], wrapped[ns:], aad)
}

// Seal encrypts a small secret with a fresh data key. aad binds the ciphertext
// to its owner (for example org and connection id) so it cannot be moved to
// another record.
//
// Layout: version(1) | len(wrapped)(2) | wrapped DEK | nonce | ciphertext.
func Seal(kr Keyring, plaintext, aad []byte) ([]byte, error) {
	dek := make([]byte, keySize)
	if _, err := rand.Read(dek); err != nil {
		return nil, err
	}
	wrapped, err := kr.WrapKey(dek, aad)
	if err != nil {
		return nil, err
	}
	aead, err := newGCM(dek)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	out := []byte{1, byte(len(wrapped) >> 8), byte(len(wrapped))}
	out = append(out, wrapped...)
	out = append(out, nonce...)
	return aead.Seal(out, nonce, plaintext, aad), nil
}

// Open reverses Seal.
func Open(kr Keyring, sealed, aad []byte) ([]byte, error) {
	if len(sealed) < 3 || sealed[0] != 1 {
		return nil, errors.New("unknown secret format")
	}
	wl := int(sealed[1])<<8 | int(sealed[2])
	rest := sealed[3:]
	if len(rest) < wl {
		return nil, errors.New("secret truncated")
	}
	dek, err := kr.UnwrapKey(rest[:wl], aad)
	if err != nil {
		return nil, errors.New("cannot decrypt secret: wrong master key or tampered data")
	}
	aead, err := newGCM(dek)
	if err != nil {
		return nil, err
	}
	rest = rest[wl:]
	if len(rest) < aead.NonceSize() {
		return nil, errors.New("secret truncated")
	}
	pt, err := aead.Open(nil, rest[:aead.NonceSize()], rest[aead.NonceSize():], aad)
	if err != nil {
		return nil, errors.New("cannot decrypt secret: wrong master key or tampered data")
	}
	return pt, nil
}

// --- streaming encryption (backups) ---

const (
	streamMagic = "DEDIEBK1\n"
	chunkSize   = 64 * 1024
)

// StreamWriter encrypts a stream in independently authenticated chunks. Each
// chunk's nonce carries its index and a final-chunk flag, so truncation,
// reordering and tampering are all detected on read.
type StreamWriter struct {
	w      io.Writer
	aead   cipher.AEAD
	prefix []byte
	buf    []byte
	index  uint64
	closed bool
}

func NewStreamWriter(kr Keyring, w io.Writer, aad []byte) (*StreamWriter, error) {
	dek := make([]byte, keySize)
	if _, err := rand.Read(dek); err != nil {
		return nil, err
	}
	wrapped, err := kr.WrapKey(dek, aad)
	if err != nil {
		return nil, err
	}
	aead, err := newGCM(dek)
	if err != nil {
		return nil, err
	}
	prefix := make([]byte, 4)
	if _, err := rand.Read(prefix); err != nil {
		return nil, err
	}
	hdr := []byte(streamMagic)
	hdr = append(hdr, byte(len(wrapped)>>8), byte(len(wrapped)))
	hdr = append(hdr, wrapped...)
	hdr = append(hdr, prefix...)
	if _, err := w.Write(hdr); err != nil {
		return nil, err
	}
	return &StreamWriter{w: w, aead: aead, prefix: prefix, buf: make([]byte, 0, chunkSize)}, nil
}

func streamNonce(prefix []byte, index uint64, final bool) []byte {
	n := make([]byte, 12)
	copy(n, prefix)
	binary.BigEndian.PutUint64(n[4:], index<<1)
	if final {
		n[11] |= 1
	}
	return n
}

func (s *StreamWriter) flush(final bool) error {
	ct := s.aead.Seal(nil, streamNonce(s.prefix, s.index, final), s.buf, nil)
	var l [4]byte
	binary.BigEndian.PutUint32(l[:], uint32(len(ct)))
	if _, err := s.w.Write(l[:]); err != nil {
		return err
	}
	if _, err := s.w.Write(ct); err != nil {
		return err
	}
	s.index++
	s.buf = s.buf[:0]
	return nil
}

func (s *StreamWriter) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		room := chunkSize - len(s.buf)
		if room == 0 {
			if err := s.flush(false); err != nil {
				return 0, err
			}
			room = chunkSize
		}
		if room > len(p) {
			room = len(p)
		}
		s.buf = append(s.buf, p[:room]...)
		p = p[room:]
	}
	return n, nil
}

// Close writes the final chunk. It does not close the underlying writer.
func (s *StreamWriter) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	return s.flush(true)
}

// StreamReader decrypts a stream produced by StreamWriter.
type StreamReader struct {
	r      io.Reader
	aead   cipher.AEAD
	prefix []byte
	buf    []byte
	index  uint64
	done   bool
}

func NewStreamReader(kr Keyring, r io.Reader, aad []byte) (*StreamReader, error) {
	hdr := make([]byte, len(streamMagic)+2)
	if _, err := io.ReadFull(r, hdr); err != nil {
		return nil, errors.New("not a dedie backup")
	}
	if string(hdr[:len(streamMagic)]) != streamMagic {
		return nil, errors.New("not a dedie backup")
	}
	wl := int(hdr[len(streamMagic)])<<8 | int(hdr[len(streamMagic)+1])
	rest := make([]byte, wl+4)
	if _, err := io.ReadFull(r, rest); err != nil {
		return nil, errors.New("backup header truncated")
	}
	dek, err := kr.UnwrapKey(rest[:wl], aad)
	if err != nil {
		return nil, errors.New("cannot decrypt backup: wrong master key or tampered header")
	}
	aead, err := newGCM(dek)
	if err != nil {
		return nil, err
	}
	return &StreamReader{r: r, aead: aead, prefix: rest[wl:]}, nil
}

func (s *StreamReader) Read(p []byte) (int, error) {
	for len(s.buf) == 0 {
		if s.done {
			return 0, io.EOF
		}
		var l [4]byte
		if _, err := io.ReadFull(s.r, l[:]); err != nil {
			return 0, errors.New("backup truncated")
		}
		n := binary.BigEndian.Uint32(l[:])
		if n > chunkSize+64 {
			return 0, errors.New("backup corrupted")
		}
		ct := make([]byte, n)
		if _, err := io.ReadFull(s.r, ct); err != nil {
			return 0, errors.New("backup truncated")
		}
		// A chunk authenticates as either a middle chunk or the final one.
		pt, err := s.aead.Open(nil, streamNonce(s.prefix, s.index, false), ct, nil)
		if err != nil {
			pt, err = s.aead.Open(nil, streamNonce(s.prefix, s.index, true), ct, nil)
			if err != nil {
				return 0, errors.New("backup corrupted or tampered")
			}
			s.done = true
		}
		s.index++
		s.buf = pt
	}
	n := copy(p, s.buf)
	s.buf = s.buf[n:]
	return n, nil
}

// --- passwords and tokens ---

const (
	argonTime    = 3
	argonMemory  = 64 * 1024
	argonThreads = 2
)

// HashPassword returns an argon2id hash in the standard encoded form.
func HashPassword(password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	h := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, keySize)
	return fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s", argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(h)), nil
}

// VerifyPassword checks a password against a hash from HashPassword.
func VerifyPassword(encoded, password string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}
	var m, t uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil {
		return false
	}
	salt, err1 := base64.RawStdEncoding.DecodeString(parts[4])
	want, err2 := base64.RawStdEncoding.DecodeString(parts[5])
	if err1 != nil || err2 != nil {
		return false
	}
	got := argon2.IDKey([]byte(password), salt, t, m, p, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// NewToken returns n random bytes, hex encoded.
func NewToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// NewID returns a short random identifier with a type prefix.
func NewID(prefix string) string { return prefix + "_" + NewToken(8) }

// HashToken returns the hex SHA-256 of a token, for storing session lookups.
func HashToken(tok string) string {
	s := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(s[:])
}
