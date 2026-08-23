package repository

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// s3ClientParams describes the common S3-compatible client configuration.
type s3ClientParams struct {
	Endpoint        string
	Region          string
	AccessKeyID     string
	SecretAccessKey string
	ForcePathStyle  bool
}

// newS3Client constructs an AWS S3 / R2 / OSS / MinIO compatible client.
func newS3Client(ctx context.Context, p s3ClientParams) (*s3.Client, error) {
	region := p.Region
	if region == "" {
		region = "auto"
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion(region),
		awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(p.AccessKeyID, p.SecretAccessKey, ""),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("load aws config: %w", err)
	}

	return s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if p.Endpoint != "" {
			o.BaseEndpoint = &p.Endpoint
		}
		if p.ForcePathStyle {
			o.UsePathStyle = true
		}
		o.APIOptions = append(o.APIOptions, v4.SwapComputePayloadSHA256ForUnsignedPayloadMiddleware)
		o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
	}), nil
}
