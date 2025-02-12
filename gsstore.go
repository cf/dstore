package dstore

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"path/filepath"
	"strings"

	"cloud.google.com/go/storage"
	"go.uber.org/zap"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
)

//
// Google Storage Store

type GSStore struct {
	baseURL *url.URL
	client  *storage.Client
	*commonStore
}

func NewGSStore(baseURL *url.URL, extension, compressionType string, overwrite bool) (*GSStore, error) {
	ctx := context.Background()
	client, err := storage.NewClient(ctx)
	if err != nil {
		return nil, err
	}

	return &GSStore{
		baseURL: baseURL,
		client:  client,
		commonStore: &commonStore{
			compressionType: compressionType,
			extension:       extension,
			overwrite:       overwrite,
		},
	}, nil
}
func (s *GSStore) SubStore(subFolder string) (Store, error) {
	url, err := url.Parse(s.baseURL.String())
	if err != nil {
		return nil, fmt.Errorf("gs store parsing base url: %w", err)
	}
	url.Path = path.Join(url.Path, subFolder)
	return NewGSStore(url, s.extension, s.compressionType, s.overwrite)
}

func (s *GSStore) BaseURL() *url.URL {
	return s.baseURL
}

func (s *GSStore) ObjectPath(name string) string {
	return path.Join(strings.TrimLeft(s.baseURL.Path, "/"), s.pathWithExt(name))
}

func (s *GSStore) ObjectURL(name string) string {
	return fmt.Sprintf("%s/%s", strings.TrimRight(s.baseURL.String(), "/"), strings.TrimLeft(s.pathWithExt(name), "/"))
}

func (s *GSStore) toBaseName(filename string) string {
	return strings.TrimPrefix(strings.TrimSuffix(filename, s.pathWithExt("")), strings.TrimLeft(s.baseURL.Path, "/")+"/")
}

func (s *GSStore) WriteObject(ctx context.Context, base string, f io.Reader) (err error) {
	path := s.ObjectPath(base)

	object := s.client.Bucket(s.baseURL.Host).Object(path)

	if !s.overwrite {
		object = object.If(storage.Conditions{DoesNotExist: true})
	}
	w := object.NewWriter(ctx)
	w.ContentType = "application/octet-stream"
	w.CacheControl = "public, max-age=86400"

	if err := s.compressedCopy(f, w); err != nil {
		return err
	}

	if err := w.Close(); err != nil {
		if s.overwrite {
			return err
		}
		return silencePreconditionError(err)
	}

	return nil
}

func silencePreconditionError(err error) error {
	if e, ok := err.(*googleapi.Error); ok {
		if e.Code == http.StatusPreconditionFailed {
			return nil
		}
	}
	return err
}

func (s *GSStore) OpenObject(ctx context.Context, name string) (out io.ReadCloser, err error) {
	path := s.ObjectPath(name)

	if tracer.Enabled() {
		zlog.Debug("opening dstore file", zap.String("path", s.pathWithExt(name)))
	}
	reader, err := s.client.Bucket(s.baseURL.Host).Object(path).NewReader(ctx)
	if err != nil {
		if err == storage.ErrObjectNotExist {
			return nil, ErrNotFound
		}

		return nil, err
	}

	out, err = s.uncompressedReader(reader)
	if tracer.Enabled() {
		out = wrapReadCloser(out, func() {
			zlog.Debug("closing dstore file", zap.String("path", s.pathWithExt(name)))
		})
	}
	return
}

func (s *GSStore) DeleteObject(ctx context.Context, base string) error {
	path := s.ObjectPath(base)
	return s.client.Bucket(s.baseURL.Host).Object(path).Delete(ctx)
}

func (s *GSStore) FileExists(ctx context.Context, base string) (bool, error) {
	path := s.ObjectPath(base)

	_, err := s.client.Bucket(s.baseURL.Host).Object(path).Attrs(ctx)
	if err != nil {
		if err == storage.ErrObjectNotExist {
			return false, nil
		}

		return false, err
	}
	return true, nil
}

func (s *GSStore) PushLocalFile(ctx context.Context, localFile, toBaseName string) error {
	remove, err := pushLocalFile(ctx, s, localFile, toBaseName)
	if err != nil {
		return err
	}
	return remove()
}

func (s *GSStore) ListFiles(ctx context.Context, prefix string, max int) ([]string, error) {
	return listFiles(ctx, s, prefix, max)
}

func (s *GSStore) Walk(ctx context.Context, prefix string, f func(filename string) (err error)) error {
	return s.WalkFrom(ctx, prefix, "", f)
}

func (s *GSStore) WalkFrom(ctx context.Context, prefix, startingPoint string, f func(filename string) (err error)) error {
	q := &storage.Query{}
	q.Prefix = strings.TrimLeft(s.baseURL.Path, "/") + "/"
	if prefix != "" {
		q.Prefix = filepath.Join(q.Prefix, prefix)
		// join cleans the string and will remove the trailing / in the prefix if present.
		// adding it back to prevent false positive matches
		if prefix[len(prefix)-1:] == "/" {
			q.Prefix = q.Prefix + "/"
		}
	}
	if startingPoint != "" {
		q.StartOffset = filepath.Join(q.Prefix, startingPoint)
	}
	it := s.client.Bucket(s.baseURL.Host).Objects(ctx, q)

	for {
		attrs, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return err
		}
		if err := f(s.toBaseName(attrs.Name)); err != nil {
			if err == StopIteration {
				return nil
			}
			return err
		}
	}
	return nil
}
