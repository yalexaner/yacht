// Package r2 is the Cloudflare R2 implementation of storage.Storage. R2 is
// S3-compatible, so the backend is built on aws-sdk-go-v2/service/s3 with
// static credentials, region="auto", and path-style addressing — the three
// knobs R2 documents as required. See:
// https://developers.cloudflare.com/r2/api/s3/api/
//
// The backend holds a single *s3.Client. The SDK pools HTTP connections
// internally and the Storage interface has no Close method, so neither the
// factory nor the binary startup path needs to dispose of a Backend.
//
// Missing-object contract: Get and Delete translate S3 NoSuchKey / NotFound
// errors into storage.ErrNotFound (wrapped with context) so the share service
// can rely on errors.Is(err, storage.ErrNotFound) uniformly across backends.
// See package storage for the cross-backend contract.
package r2

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/yalexaner/yacht/internal/storage"
)

// Backend is the R2/S3 implementation of storage.Storage.
type Backend struct {
	client *s3.Client
	bucket string
}

// compile-time interface assertion — if the Storage interface grows a new
// method, this line fails to build and forces us to update the backend
// (rather than silently diverging from the local backend).
var _ storage.Storage = (*Backend)(nil)

// New constructs a Backend against the given R2 bucket. accountID is accepted
// for logging symmetry with the rest of the startup pipeline and possible
// future use (e.g. deriving the default endpoint host); the SDK itself does
// not need it because BaseEndpoint already points at the full R2 URL.
//
// All string arguments except accountID are required — R2 has no anonymous
// access and no default bucket/endpoint, so an empty value is almost
// certainly a config-loader bug and deserves to fail fast rather than
// surface as a confusing SDK error on the first Put.
func New(ctx context.Context, accountID, accessKeyID, secretAccessKey, bucket, endpoint string) (*Backend, error) {
	var missing []error
	if accessKeyID == "" {
		missing = append(missing, errors.New("access key id is empty"))
	}
	if secretAccessKey == "" {
		missing = append(missing, errors.New("secret access key is empty"))
	}
	if bucket == "" {
		missing = append(missing, errors.New("bucket is empty"))
	}
	if endpoint == "" {
		missing = append(missing, errors.New("endpoint is empty"))
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("r2 storage: %w", errors.Join(missing...))
	}

	// region="auto" is the value R2 documents; the SDK still requires *some*
	// region for signing, and any real AWS region would be rejected by R2.
	sdkCfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(accessKeyID, secretAccessKey, "")),
		awsconfig.WithRegion("auto"),
	)
	if err != nil {
		return nil, fmt.Errorf("r2 storage: load aws config: %w", err)
	}

	// UsePathStyle=true because R2 does not support virtual-hosted style
	// addressing (https://bucket.endpoint/key); the SDK default is
	// virtual-hosted and would break. BaseEndpoint overrides the SDK's
	// region-derived endpoint with the R2 URL from config.
	client := s3.NewFromConfig(sdkCfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(endpoint)
		o.UsePathStyle = true
	})

	_ = accountID // retained in the signature for symmetry with config.Shared and future logging use.
	return &Backend{client: client, bucket: bucket}, nil
}

// Put streams r into the bucket under key. The reader is passed through to
// the SDK so large uploads stream end-to-end without buffering the whole
// payload in memory. ContentLength is provided (required by the S3 API for
// non-chunked PUTs) and ContentType is recorded so Get can return it.
func (b *Backend) Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error {
	_, err := b.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(b.bucket),
		Key:           aws.String(key),
		Body:          r,
		ContentLength: aws.Int64(size),
		ContentType:   aws.String(contentType),
	})
	if err != nil {
		return fmt.Errorf("put %q: %w", key, err)
	}
	return nil
}

// Get fetches the object and returns its body as an io.ReadCloser alongside
// ObjectInfo. A NoSuchKey response is mapped to storage.ErrNotFound so
// callers can use errors.Is uniformly across backends.
func (b *Backend) Get(ctx context.Context, key string) (io.ReadCloser, *storage.ObjectInfo, error) {
	out, err := b.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(b.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		var noSuchKey *types.NoSuchKey
		if errors.As(err, &noSuchKey) {
			return nil, nil, fmt.Errorf("get %q: %w", key, storage.ErrNotFound)
		}
		return nil, nil, fmt.Errorf("get %q: %w", key, err)
	}

	info := &storage.ObjectInfo{
		Size:        aws.ToInt64(out.ContentLength),
		ContentType: aws.ToString(out.ContentType),
	}
	return out.Body, info, nil
}

// Delete removes the object. S3's DeleteObject is idempotent — it succeeds
// whether or not the key exists — which would make it impossible to
// distinguish "already deleted" from "just deleted". To keep the contract
// symmetric with the local backend, we HeadObject first: on NotFound we
// return ErrNotFound; otherwise we proceed to DeleteObject.
func (b *Backend) Delete(ctx context.Context, key string) error {
	_, err := b.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(b.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		var notFound *types.NotFound
		if errors.As(err, &notFound) {
			return fmt.Errorf("delete %q: %w", key, storage.ErrNotFound)
		}
		return fmt.Errorf("delete %q: head: %w", key, err)
	}

	if _, err := b.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(b.bucket),
		Key:    aws.String(key),
	}); err != nil {
		return fmt.Errorf("delete %q: %w", key, err)
	}
	return nil
}
