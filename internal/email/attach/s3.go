package attach

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// S3 is a Store on one private S3 bucket. The bucket's own settings (SSE-KMS
// default, Block Public Access, versioning off, lifecycle) come from
// Terraform; each upload also asks for SSE-KMS explicitly.
type S3 struct {
	Client *s3.Client
	Bucket string
	// KMSKeyID, when set, names the key each object is encrypted under.
	KMSKeyID string

	up *manager.Uploader
}

// NewS3 returns an S3 store. Build it with NewS3, not a struct literal: the
// uploader is set there.
func NewS3(c *s3.Client, bucket, kmsKeyID string) *S3 {
	return &S3{Client: c, Bucket: bucket, KMSKeyID: kmsKeyID, up: uploader(c)}
}

// uploader streams in 5 MiB parts, one at a time, so an upload holds at most
// one part in memory. LeavePartsOnError stays false: a failed multipart upload
// is aborted with AbortMultipartUpload before Upload returns.
func uploader(c *s3.Client) *manager.Uploader {
	return manager.NewUploader(c, func(u *manager.Uploader) {
		u.PartSize = manager.MinUploadPartSize
		u.Concurrency = 1
		u.LeavePartsOnError = false
	})
}

// Put streams body to key. If the body fails or the upload does, the multipart
// upload is aborted and a best-effort DeleteObject follows, so no object or
// parts are left.
func (s *S3) Put(ctx context.Context, key string, body io.Reader) error {
	in := &s3.PutObjectInput{
		Bucket:               aws.String(s.Bucket),
		Key:                  aws.String(key),
		Body:                 body,
		ServerSideEncryption: s3types.ServerSideEncryptionAwsKms,
		ContentType:          aws.String("application/octet-stream"),
	}
	if s.KMSKeyID != "" {
		in.SSEKMSKeyId = aws.String(s.KMSKeyID)
	}
	if _, err := s.up.Upload(ctx, in); err != nil {
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer cancel()
		_ = s.Delete(dctx, key)
		return err
	}
	return nil
}

// Delete removes key. A missing key is not an error.
func (s *S3) Delete(ctx context.Context, key string) error {
	_, err := s.Client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(s.Bucket), Key: aws.String(key)})
	return err
}

// PresignGet signs a GET for key. The response is forced to download as an
// opaque file, never rendered by the browser.
func (s *S3) PresignGet(ctx context.Context, key string, ttl time.Duration, downloadName string) (string, error) {
	req, err := s3.NewPresignClient(s.Client).PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket:                     aws.String(s.Bucket),
		Key:                        aws.String(key),
		ResponseContentType:        aws.String("application/octet-stream"),
		ResponseContentDisposition: aws.String(fmt.Sprintf("attachment; filename=%q", downloadName)),
	}, s3.WithPresignExpires(ttl))
	if err != nil {
		return "", err
	}
	return req.URL, nil
}

// List walks every object under prefix.
func (s *S3) List(ctx context.Context, prefix string, fn func(Object) error) error {
	p := s3.NewListObjectsV2Paginator(s.Client, &s3.ListObjectsV2Input{Bucket: aws.String(s.Bucket), Prefix: aws.String(prefix)})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return err
		}
		for _, o := range page.Contents {
			if err := fn(Object{Key: aws.ToString(o.Key), Modified: aws.ToTime(o.LastModified)}); err != nil {
				return err
			}
		}
	}
	return nil
}
