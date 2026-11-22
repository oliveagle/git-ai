// Command mkbucket creates an object storage bucket if it does not exist, and
// deletes one on request.
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
//
// Deleting a bucket is what retires a lane for good, so it takes -force as
// well: without it a bucket that still holds objects is left alone.
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
	bucket := flag.String("bucket", "", "name of the bucket to create or delete")
	remove := flag.Bool("delete", false, "delete the bucket instead of creating it")
	force := flag.Bool("force", false, "with -delete, delete the bucket's objects too")
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

	if *remove {
		os.Exit(deleteBucket(ctx, client, *bucket, *force))
	}

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

// deleteBucket empties the bucket when force is set and then removes it. It
// refuses to empty a bucket otherwise: a lane's objects are its only copy of
// the packs the storage core refers to.
func deleteBucket(ctx context.Context, client *s3.Client, bucket string, force bool) int {
	keys, err := listKeys(ctx, client, bucket)
	if err != nil {
		if isNoSuchBucket(err) {
			fmt.Printf("bucket %s does not exist\n", bucket)
			return 0
		}
		fmt.Fprintf(os.Stderr, "mkbucket: list %s: %v\n", bucket, err)
		return 1
	}
	if len(keys) > 0 {
		if !force {
			fmt.Fprintf(os.Stderr, "mkbucket: %s still holds %d objects; -force deletes them\n", bucket, len(keys))
			return 1
		}
		for len(keys) > 0 {
			batch := keys
			if len(batch) > 1000 {
				batch = batch[:1000]
			}
			objects := make([]types.ObjectIdentifier, 0, len(batch))
			for _, key := range batch {
				objects = append(objects, types.ObjectIdentifier{Key: aws.String(key)})
			}
			if _, err := client.DeleteObjects(ctx, &s3.DeleteObjectsInput{
				Bucket: aws.String(bucket),
				Delete: &types.Delete{Objects: objects, Quiet: aws.Bool(true)},
			}); err != nil {
				fmt.Fprintf(os.Stderr, "mkbucket: delete objects of %s: %v\n", bucket, err)
				return 1
			}
			keys = keys[len(batch):]
		}
	}
	if _, err := client.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: aws.String(bucket)}); err != nil {
		fmt.Fprintf(os.Stderr, "mkbucket: delete %s: %v\n", bucket, err)
		return 1
	}
	fmt.Printf("bucket %s deleted\n", bucket)
	return 0
}

func listKeys(ctx context.Context, client *s3.Client, bucket string) ([]string, error) {
	var keys []string
	pager := s3.NewListObjectsV2Paginator(client, &s3.ListObjectsV2Input{Bucket: aws.String(bucket)})
	for pager.HasMorePages() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, obj := range page.Contents {
			keys = append(keys, aws.ToString(obj.Key))
		}
	}
	return keys, nil
}

// isNoSuchBucket tells a bucket that is already gone from a real failure, so
// deleting twice is not an error.
func isNoSuchBucket(err error) bool {
	var missing *types.NoSuchBucket
	return errors.As(err, &missing)
}
