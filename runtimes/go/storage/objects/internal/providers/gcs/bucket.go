package gcs

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"net/http"
	"strconv"

	"cloud.google.com/go/storage"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"encore.dev/appruntime/exported/config"
	"encore.dev/storage/objects/internal/types"
)

type Manager struct {
	ctx     context.Context
	runtime *config.Runtime
	clients map[*config.BucketProvider]*storage.Client
}

func NewManager(ctx context.Context, runtime *config.Runtime) *Manager {
	return &Manager{ctx: ctx, runtime: runtime, clients: make(map[*config.BucketProvider]*storage.Client)}
}

type bucket struct {
	client *storage.Client
	cfg    *config.Bucket
	handle *storage.BucketHandle
}

func (mgr *Manager) ProviderName() string { return "gcs" }

func (mgr *Manager) Matches(cfg *config.BucketProvider) bool {
	return cfg.GCS != nil
}

func (mgr *Manager) NewBucket(provider *config.BucketProvider, runtimeCfg *config.Bucket) types.BucketImpl {
	client := mgr.clientForProvider(provider)
	handle := client.Bucket(runtimeCfg.CloudName)
	return &bucket{client, runtimeCfg, handle}
}

func (b *bucket) Download(data types.DownloadData) (types.Downloader, error) {
	obj := b.handle.Object(data.Object.String())
	if data.Version != "" {
		if gen, err := strconv.ParseInt(data.Version, 10, 64); err == nil {
			obj = obj.Generation(gen)
		}
	}
	r, err := obj.NewReader(data.Ctx)
	return r, mapErr(err)
}

func (b *bucket) Upload(data types.UploadData) (types.Uploader, error) {
	ctx, cancel := context.WithCancelCause(data.Ctx)
	obj := b.handle.Object(data.Object.String())

	if data.Pre.NotExists {
		obj = obj.If(storage.Conditions{
			DoesNotExist: true,
		})
	}

	w := obj.NewWriter(ctx)
	w.ContentType = data.Attrs.ContentType

	u := &uploader{
		cancel: cancel,
		w:      w,
	}
	return u, nil
}

type uploader struct {
	cancel context.CancelCauseFunc
	w      *storage.Writer
}

func (u *uploader) Write(p []byte) (int, error) {
	n, err := u.w.Write(p)
	return n, mapErr(err)
}

func (u *uploader) Complete() (*types.ObjectAttrs, error) {
	if err := u.w.Close(); err != nil {
		return nil, mapErr(err)
	}

	attrs := u.w.Attrs()
	return mapAttrs(attrs), nil
}

func (u *uploader) Abort(err error) {
	u.cancel(err)
}

func mapAttrs(attrs *storage.ObjectAttrs) *types.ObjectAttrs {
	if attrs == nil {
		return nil
	}
	return &types.ObjectAttrs{
		Object:      types.CloudObject(attrs.Name),
		Version:     strconv.FormatInt(attrs.Generation, 10),
		ContentType: attrs.ContentType,
		Size:        attrs.Size,
		ETag:        attrs.Etag,
	}
}

func mapListEntry(attrs *storage.ObjectAttrs) *types.ListEntry {
	return &types.ListEntry{
		Object: types.CloudObject(attrs.Name),
		Size:   attrs.Size,
		ETag:   attrs.Etag,
	}
}

func (b *bucket) List(data types.ListData) iter.Seq2[*types.ListEntry, error] {
	iter := b.handle.Objects(data.Ctx, &storage.Query{
		Prefix: data.Prefix,
	})
	var n int64
	return func(yield func(*types.ListEntry, error) bool) {
		for {
			res, err := iter.Next()
			if err == iterator.Done {
				return
			}

			// Are we over the limit?
			if data.Limit != nil && n >= *data.Limit {
				return
			}
			n++

			var entry *types.ListEntry
			if res != nil {
				entry = mapListEntry(res)
			}

			if !yield(entry, mapErr(err)) {
				return
			}
		}
	}
}

func (b *bucket) Remove(data types.RemoveData) error {
	obj := b.handle.Object(data.Object.String())

	if data.Version != "" {
		if gen, err := strconv.ParseInt(data.Version, 10, 64); err == nil {
			obj = obj.Generation(gen)
		}
	}

	err := obj.Delete(data.Ctx)
	return mapErr(err)
}

func (b *bucket) Attrs(data types.AttrsData) (*types.ObjectAttrs, error) {
	obj := b.handle.Object(data.Object.String())

	if data.Version != "" {
		if gen, err := strconv.ParseInt(data.Version, 10, 64); err == nil {
			obj = obj.Generation(gen)
		}
	}

	resp, err := obj.Attrs(data.Ctx)
	return mapAttrs(resp), mapErr(err)
}

func (mgr *Manager) clientForProvider(prov *config.BucketProvider) *storage.Client {
	if client, ok := mgr.clients[prov]; ok {
		return client
	}

	var opts []option.ClientOption
	if prov.GCS.Anonymous {
		opts = append(opts, option.WithoutAuthentication())
	}
	if prov.GCS.Endpoint != "" {
		opts = append(opts, option.WithEndpoint(prov.GCS.Endpoint))
	}

	client, err := storage.NewClient(mgr.ctx, opts...)
	if err != nil {
		panic(fmt.Sprintf("failed to create object storage client: %s", err))
	}

	mgr.clients[prov] = client
	return client
}

func mapErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, storage.ErrObjectNotExist):
		return types.ErrObjectNotExist
	default:
		// Handle precondition failures
		{
			var e *googleapi.Error
			if ok := errors.As(err, &e); ok && e.Code == http.StatusPreconditionFailed {
				return types.ErrPreconditionFailed
			}
		}

		{
			if s, ok := status.FromError(err); ok && s.Code() == codes.AlreadyExists || s.Code() == codes.FailedPrecondition {
				return types.ErrPreconditionFailed
			}
		}

		return err
	}
}
