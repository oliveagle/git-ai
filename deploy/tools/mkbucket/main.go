// Command mkbucket creates an object storage bucket if it does not exist.
//
// The release lane needs a bucket of its own: the staging lane must not
// write a single object into the bucket the production lane serves. The
// NAS runs RustFS, an S3 compatible store, and carries no S3 client of its
// own, so this small tool does the job with the AWS SDK the server already
// depends on.
//
// The credentials come from the environment (AWS_ACCESS_KEY_ID,
// AWS_SECRET_ACCESS_KEY), never from a flag: they belong to the service
// account of the store, and the file that holds them is 0600 on the host.
//
//	AWS_ACCESS_KEY_ID=... AWS_SECRET_ACCESS_KEY=... \
//	  go run ./deploy/tools/mkbucket -endpoint http://127.0.0.1:9000 \
//	    -bucket git-ai-objects-staging -region us-east-1
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

func main() {
	endpoint := flag.String("endpoint", "http://127.0.0.1:9000", "S3 endpoint of the store")
	region := flag.String("region", "us-east-1", "region of the bucket")
	bucket := flag.String("bucket", "", "name of the bucket to create")
	flag.Parse()

	if *bucket == "" {
		fmt.Fprintln(os.Stderr, "mkbucket: -bucket is required")
		os.Exit(2)
	}

	ctx := context.Background()
	cfg := aws.Config{
		Region:      *region,
		Credentials: credentials.NewStaticCredentialsProvider(os.Getenv("AWS_ACCESS_KEY_ID"), os.Getenv("AWS_SECRET_ACCESS_KEY"), ""),
	}
	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(*endpoint)
		// RustFS is reached by path, not by a virtual host name.
		o.UsePathStyle = true
	})

	if _, err := client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(*bucket)}); err == nil {
		fmt.Printf("bucket %s exists\n", *bucket)
		return
	}

	if _, err := client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(*bucket)}); err != nil {
		var owned *types.BucketAlreadyExists
		var ownedByYou *types.BucketAlreadyOwnedByYou
		if errors.As(err, &owned) || errors.As(err, &ownedByYou) {
			fmt.Printf("bucket %s exists\n", *bucket)
			return
		}
		fmt.Fprintf(os.Stderr, "mkbucket: create %s: %v\n", *bucket, err)
		os.Exit(1)
	}
	fmt.Printf("bucket %s created\n", *bucket)
}
