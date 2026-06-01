package fs

import (
	"bytes"
	"context"
	"io"
	"testing"
)

func TestFS_PutGetExistsDelete(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := s.EnsureBucket(ctx, "b"); err != nil {
		t.Fatal(err)
	}
	body := []byte("hello")
	if err := s.Put(ctx, "b", "k.txt", bytes.NewReader(body), int64(len(body)), "text/plain"); err != nil {
		t.Fatal(err)
	}
	ok, err := s.Exists(ctx, "b", "k.txt")
	if err != nil || !ok {
		t.Fatalf("Exists = %v, %v", ok, err)
	}
	rc, err := s.Get(ctx, "b", "k.txt")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, body) {
		t.Errorf("Get = %q, want %q", got, body)
	}
	if err := s.Delete(ctx, "b", "k.txt"); err != nil {
		t.Fatal(err)
	}
	ok, _ = s.Exists(ctx, "b", "k.txt")
	if ok {
		t.Error("Exists after Delete should be false")
	}
}

func TestFS_ListWithPrefix(t *testing.T) {
	s, _ := New(t.TempDir())
	ctx := context.Background()
	_ = s.EnsureBucket(ctx, "b")
	for _, k := range []string{"a/1", "a/2", "b/3"} {
		_ = s.Put(ctx, "b", k, bytes.NewReader([]byte("x")), 1, "")
	}
	got, err := s.List(ctx, "b", "a/")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("List prefix a/ = %v, want 2 keys", got)
	}
}
