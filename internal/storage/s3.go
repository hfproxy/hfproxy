package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

type S3 struct {
	client               *s3.Client
	bucket               string
	prefix               string
	serverSideEncryption bool
}

func IsNotFound(err error) bool {
	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	return apiErr.ErrorCode() == "NotFound" || apiErr.ErrorCode() == "NoSuchKey"
}

func New(ctx context.Context, endpoint, region, bucket, prefix, accessKey, secretKey string, pathStyle, serverSideEncryption bool) (*S3, error) {
	options := []func(*awsconfig.LoadOptions) error{awsconfig.WithRegion(region)}
	if accessKey != "" {
		options = append(options, awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(accessKey, secretKey, "")))
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, options...)
	if err != nil {
		return nil, fmt.Errorf("load S3 configuration: %w", err)
	}
	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.UsePathStyle = pathStyle
		if endpoint != "" {
			o.BaseEndpoint = aws.String(endpoint)
		}
	})
	return &S3{client: client, bucket: bucket, prefix: strings.Trim(prefix, "/"), serverSideEncryption: serverSideEncryption}, nil
}

func (s *S3) Key(sha256 string) string {
	key := "blobs/sha256/" + sha256[:2] + "/" + sha256
	if s.prefix != "" {
		return s.prefix + "/" + key
	}
	return key
}

func (s *S3) PartialKey(id string, part int) string {
	key := fmt.Sprintf("partials/%s/%08d", id, part)
	if s.prefix != "" {
		return s.prefix + "/" + key
	}
	return key
}

func (s *S3) UploadFile(ctx context.Context, file *os.File, key, contentType string, size int64) error {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if size <= 64<<20 {
		input := &s3.PutObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key), Body: file, ContentLength: aws.Int64(size), ContentType: aws.String(contentType)}
		if s.serverSideEncryption {
			input.ServerSideEncryption = "AES256"
		}
		_, err := s.client.PutObject(ctx, input)
		if err != nil {
			return fmt.Errorf("upload object to S3: %w", err)
		}
		return nil
	}
	create := &s3.CreateMultipartUploadInput{Bucket: aws.String(s.bucket), Key: aws.String(key), ContentType: aws.String(contentType)}
	if s.serverSideEncryption {
		create.ServerSideEncryption = "AES256"
	}
	upload, err := s.client.CreateMultipartUpload(ctx, create)
	if err != nil {
		return fmt.Errorf("start multipart upload: %w", err)
	}
	abort := func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		_, _ = s.client.AbortMultipartUpload(cleanup, &s3.AbortMultipartUploadInput{Bucket: aws.String(s.bucket), Key: aws.String(key), UploadId: upload.UploadId})
	}
	const partSize int64 = 64 << 20
	parts := make([]types.CompletedPart, 0, (size+partSize-1)/partSize)
	for offset, partNumber := int64(0), int32(1); offset < size; offset, partNumber = offset+partSize, partNumber+1 {
		length := partSize
		if remaining := size - offset; remaining < length {
			length = remaining
		}
		part, partErr := s.client.UploadPart(ctx, &s3.UploadPartInput{Bucket: aws.String(s.bucket), Key: aws.String(key), UploadId: upload.UploadId, PartNumber: aws.Int32(partNumber), Body: io.NewSectionReader(file, offset, length), ContentLength: aws.Int64(length)})
		if partErr != nil {
			abort()
			return fmt.Errorf("upload multipart part: %w", partErr)
		}
		parts = append(parts, types.CompletedPart{ETag: part.ETag, PartNumber: aws.Int32(partNumber)})
	}
	if _, err := s.client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{Bucket: aws.String(s.bucket), Key: aws.String(key), UploadId: upload.UploadId, MultipartUpload: &types.CompletedMultipartUpload{Parts: parts}}); err != nil {
		abort()
		return fmt.Errorf("complete multipart upload: %w", err)
	}
	return nil
}

func (s *S3) Get(ctx context.Context, key, byteRange string) (*s3.GetObjectOutput, error) {
	input := &s3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)}
	if byteRange != "" {
		input.Range = aws.String(byteRange)
	}
	return s.client.GetObject(ctx, input)
}

func (s *S3) Head(ctx context.Context, key string) (*s3.HeadObjectOutput, error) {
	return s.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)})
}

func (s *S3) Delete(ctx context.Context, key string) error {
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)})
	return err
}

func (s *S3) Ready(ctx context.Context) error {
	_, err := s.client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(s.bucket)})
	return err
}

func (s *S3) Bucket() string { return s.bucket }
