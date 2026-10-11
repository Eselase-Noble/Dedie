package secure

import (
	"bytes"
	"crypto/rand"
	"io"
	"testing"
)

func TestSealOpen(t *testing.T) {
	kr, err := LoadKeyring(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ct, err := Seal(kr, []byte("s3cret"), []byte("org1/conn1"))
	if err != nil {
		t.Fatal(err)
	}
	pt, err := Open(kr, ct, []byte("org1/conn1"))
	if err != nil || string(pt) != "s3cret" {
		t.Fatalf("round trip failed: %v %q", err, pt)
	}
	if _, err := Open(kr, ct, []byte("org2/conn1")); err == nil {
		t.Fatal("secret opened under a different owner")
	}
	ct[len(ct)-1] ^= 1
	if _, err := Open(kr, ct, []byte("org1/conn1")); err == nil {
		t.Fatal("tampered secret opened")
	}
}

func TestStream(t *testing.T) {
	kr, _ := LoadKeyring(t.TempDir())
	for _, size := range []int{0, 1, chunkSize, chunkSize + 1, 3*chunkSize + 17} {
		data := make([]byte, size)
		rand.Read(data)
		var enc bytes.Buffer
		w, err := NewStreamWriter(kr, &enc, []byte("b1"))
		if err != nil {
			t.Fatal(err)
		}
		w.Write(data)
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		r, err := NewStreamReader(kr, bytes.NewReader(enc.Bytes()), []byte("b1"))
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(r)
		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("size %d: round trip failed: %v", size, err)
		}
		// Truncating the stream must fail, not silently return less data.
		if size > chunkSize {
			cut := enc.Bytes()[:enc.Len()-chunkSize/2]
			r, _ := NewStreamReader(kr, bytes.NewReader(cut), []byte("b1"))
			if _, err := io.ReadAll(r); err == nil {
				t.Fatalf("size %d: truncated stream accepted", size)
			}
		}
	}
}

func TestPassword(t *testing.T) {
	h, err := HashPassword("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if !VerifyPassword(h, "correct horse battery") || VerifyPassword(h, "wrong") {
		t.Fatal("password verification broken")
	}
}
