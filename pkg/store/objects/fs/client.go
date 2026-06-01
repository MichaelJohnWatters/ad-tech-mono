// Package fs implements objects.Store on the local filesystem.
//
// Used by unit tests and as a no-credential fallback for one-shot
// scripts. Buckets become subdirectories under Root.
package fs

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Store is a filesystem-backed objects.Store rooted at Root.
type Store struct {
	Root string
}

// New creates a filesystem store. Root must exist or be createable.
func New(root string) (*Store, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	return &Store{Root: root}, nil
}

func (s *Store) path(bucket, key string) string {
	return filepath.Join(s.Root, bucket, key)
}

func (s *Store) Put(_ context.Context, bucket, key string, body io.Reader, _ int64, _ string) error {
	p := s.path(bucket, key)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	f, err := os.Create(p)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, body)
	return err
}

func (s *Store) Get(_ context.Context, bucket, key string) (io.ReadCloser, error) {
	return os.Open(s.path(bucket, key))
}

func (s *Store) Delete(_ context.Context, bucket, key string) error {
	err := os.Remove(s.path(bucket, key))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (s *Store) List(_ context.Context, bucket, prefix string) ([]string, error) {
	root := filepath.Join(s.Root, bucket)
	var keys []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if strings.HasPrefix(rel, prefix) {
			keys = append(keys, rel)
		}
		return nil
	})
	return keys, err
}

func (s *Store) Exists(_ context.Context, bucket, key string) (bool, error) {
	_, err := os.Stat(s.path(bucket, key))
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
}

func (s *Store) PresignedGetURL(_ context.Context, bucket, key string, _ time.Duration) (string, error) {
	return "file://" + s.path(bucket, key), nil
}

func (s *Store) EnsureBucket(_ context.Context, bucket string) error {
	return os.MkdirAll(filepath.Join(s.Root, bucket), 0o755)
}
