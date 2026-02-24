package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

type S3Client struct {
	client    *s3.Client
	presigner *s3.PresignClient // for presigned URLs (may use public endpoint)
	bucket    string
	expiry    time.Duration
}

func NewS3Client(ctx context.Context, cfg *Config) (*S3Client, error) {
	opts := []func(*awsconfig.LoadOptions) error{
		awsconfig.WithRegion(cfg.S3Region),
	}

	if cfg.S3AccessKey != "" && cfg.S3SecretKey != "" {
		opts = append(opts, awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(cfg.S3AccessKey, cfg.S3SecretKey, ""),
		))
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("load AWS config: %w", err)
	}

	// Internal client (for DeleteObject and other operations)
	s3Opts := func(o *s3.Options) {
		if cfg.S3Endpoint != "" {
			o.BaseEndpoint = aws.String(cfg.S3Endpoint)
		}
		if cfg.S3ForcePathStyle {
			o.UsePathStyle = true
		}
	}
	client := s3.NewFromConfig(awsCfg, s3Opts)

	// Presigner: use public endpoint if set, so presigned URLs are accessible
	// from outside Docker (e.g., the agent running on the host)
	presignEndpoint := cfg.S3Endpoint
	if cfg.S3PublicEndpoint != "" {
		presignEndpoint = cfg.S3PublicEndpoint
	}

	presignOpts := func(o *s3.Options) {
		if presignEndpoint != "" {
			o.BaseEndpoint = aws.String(presignEndpoint)
		}
		if cfg.S3ForcePathStyle {
			o.UsePathStyle = true
		}
	}
	presignClient := s3.NewFromConfig(awsCfg, presignOpts)
	presigner := s3.NewPresignClient(presignClient)

	return &S3Client{
		client:    client,
		presigner: presigner,
		bucket:    cfg.S3Bucket,
		expiry:    cfg.PresignExpiry,
	}, nil
}

// PresignPut generates a presigned PUT URL for uploading an object.
func (c *S3Client) PresignPut(ctx context.Context, key string, contentType string) (string, error) {
	input := &s3.PutObjectInput{
		Bucket:      aws.String(c.bucket),
		Key:         aws.String(key),
		ContentType: aws.String(contentType),
	}

	resp, err := c.presigner.PresignPutObject(ctx, input, s3.WithPresignExpires(c.expiry))
	if err != nil {
		return "", fmt.Errorf("presign PUT %s: %w", key, err)
	}
	return resp.URL, nil
}

// PresignPutWithLength generates a presigned PUT URL with a fixed Content-Length.
// S3 will reject uploads where the actual body size doesn't match.
func (c *S3Client) PresignPutWithLength(ctx context.Context, key, contentType string, contentLength int64) (string, error) {
	input := &s3.PutObjectInput{
		Bucket:        aws.String(c.bucket),
		Key:           aws.String(key),
		ContentType:   aws.String(contentType),
		ContentLength: aws.Int64(contentLength),
	}

	resp, err := c.presigner.PresignPutObject(ctx, input, s3.WithPresignExpires(c.expiry))
	if err != nil {
		return "", fmt.Errorf("presign PUT %s: %w", key, err)
	}
	return resp.URL, nil
}

// PresignGet generates a presigned GET URL for downloading an object.
func (c *S3Client) PresignGet(ctx context.Context, key string) (string, error) {
	input := &s3.GetObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	}

	resp, err := c.presigner.PresignGetObject(ctx, input, s3.WithPresignExpires(c.expiry))
	if err != nil {
		return "", fmt.Errorf("presign GET %s: %w", key, err)
	}
	return resp.URL, nil
}

// DeleteObject removes an object from S3.
func (c *S3Client) DeleteObject(ctx context.Context, key string) error {
	_, err := c.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	})
	return err
}

// DeleteBackupObjects deletes both the backup blob and manifest from S3.
func (c *S3Client) DeleteBackupObjects(ctx context.Context, b *Backup) {
	if err := c.DeleteObject(ctx, b.S3Key); err != nil {
		log.Printf("WARN: failed to delete S3 object %s: %v", b.S3Key, err)
	}
	if err := c.DeleteObject(ctx, b.ManifestS3Key); err != nil {
		log.Printf("WARN: failed to delete S3 object %s: %v", b.ManifestS3Key, err)
	}
}
