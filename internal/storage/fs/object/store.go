package object

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"strings"
	"sync"
	"time"

	s3v2 "github.com/aws/aws-sdk-go-v2/service/s3"
	"go.flipt.io/flipt/internal/containers"
	"go.flipt.io/flipt/internal/storage"
	storagefs "go.flipt.io/flipt/internal/storage/fs"
	"go.uber.org/zap"
	gcaws "gocloud.dev/aws"
	gcblob "gocloud.dev/blob"
	"gocloud.dev/blob/s3blob"
	"gocloud.dev/gcerrors"
)

var _ storagefs.SnapshotStore = (*SnapshotStore)(nil)

// S3Schema is a custom scheme for gocloud blob which works with
// how we interact with s3 (supports interfacing with minio)
const S3Schema = "s3i"

func init() {
	gcblob.DefaultURLMux().RegisterBucket(S3Schema, new(urlSessionOpener))
}

type urlSessionOpener struct{}

func (o *urlSessionOpener) OpenBucketURL(ctx context.Context, u *url.URL) (*gcblob.Bucket, error) {
	cfg, err := gcaws.V2ConfigFromURLParams(ctx, u.Query())
	if err != nil {
		return nil, fmt.Errorf("open bucket %v: %w", u, err)
	}
	clientV2 := s3v2.NewFromConfig(cfg, func(o *s3v2.Options) {
		o.UsePathStyle = true
	})
	return s3blob.OpenBucketV2(ctx, clientV2, u.Host, &s3blob.Options{})
}

type SnapshotStore struct {
	*storagefs.Poller

	logger   *zap.Logger
	scheme   string
	bucket   *gcblob.Bucket
	prefix   string
	pollOpts []containers.Option[storagefs.Poller]

	mu   sync.RWMutex
	snap storage.ReadOnlyStore
}

func NewSnapshotStore(ctx context.Context, logger *zap.Logger, scheme string, bucket *gcblob.Bucket, opts ...containers.Option[SnapshotStore]) (*SnapshotStore, error) {
	s := &SnapshotStore{
		logger: logger,
		scheme: scheme,
		bucket: bucket,
		pollOpts: []containers.Option[storagefs.Poller]{
			storagefs.WithInterval(60 * time.Second),
		},
	}

	containers.ApplyAll(s, opts...)

	// fetch snapshot at-least once before returning store
	// to ensure we have some state to serve
	if _, err := s.update(ctx); err != nil {
		return nil, err
	}

	s.Poller = storagefs.NewPoller(s.logger, ctx, s.update, s.pollOpts...)

	go s.Poll()

	return s, nil
}

// WithPrefix configures the prefix for object store
func WithPrefix(prefix string) containers.Option[SnapshotStore] {
	return func(s *SnapshotStore) {
		s.prefix = prefix
	}
}

// WithPollOptions configures the poller options used when periodically updating snapshot state
func WithPollOptions(opts ...containers.Option[storagefs.Poller]) containers.Option[SnapshotStore] {
	return func(s *SnapshotStore) {
		s.pollOpts = append(s.pollOpts, opts...)
	}
}

// View accepts a function which takes a *StoreSnapshot.
// The SnapshotStore will supply a snapshot which is valid
// for the lifetime of the provided function call.
func (s *SnapshotStore) View(fn func(storage.ReadOnlyStore) error) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return fn(s.snap)
}

func (s *SnapshotStore) String() string {
	return s.scheme
}

// Update fetches a new snapshot and swaps it out for the current one.
func (s *SnapshotStore) update(ctx context.Context) (bool, error) {
	snap, err := s.build(ctx)
	if err != nil {
		return false, err
	}

	s.mu.Lock()
	s.snap = snap
	s.mu.Unlock()

	return true, nil
}

func (s *SnapshotStore) build(ctx context.Context) (*storagefs.Snapshot, error) {
	idx, err := s.getIndex(ctx)
	if err != nil {
		return nil, err
	}

	iterator := s.bucket.List(&gcblob.ListOptions{
		Prefix: s.prefix,
	})

	var files []fs.File
	for {
		item, err := iterator.Next(ctx)
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, err
		}

		key := strings.TrimPrefix(item.Key, s.prefix)
		if !idx.Match(key) {
			continue
		}

		rd, err := s.bucket.NewReader(ctx, s.prefix+key, &gcblob.ReaderOptions{})
		if err != nil {
			return nil, err
		}

		files = append(files, NewFile(
			key,
			item.Size,
			rd,
			item.ModTime,
		))
	}

	return storagefs.SnapshotFromFiles(s.logger, files...)
}

func (s *SnapshotStore) getIndex(ctx context.Context) (*storagefs.FliptIndex, error) {
	rd, err := s.bucket.NewReader(ctx, s.prefix+storagefs.IndexFileName, &gcblob.ReaderOptions{})
	if err == nil {
		idx, err := storagefs.ParseFliptIndex(rd)
		if err != nil {
			return nil, err
		}

		return idx, nil
	}

	if err != nil {
		if gcerrors.Code(err) != gcerrors.NotFound {
			return nil, err
		}

		s.logger.Debug("using default index",
			zap.String("file", storagefs.IndexFileName),
			zap.Error(fs.ErrNotExist))
	}

	return storagefs.DefaultFliptIndex()
}