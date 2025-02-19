import * as pulumi from "@pulumi/pulumi";
import * as aws from "@pulumi/aws";
import * as awsx from "@pulumi/awsx";

// Create an AWS resource (S3 Bucket).
const bucket = new aws.s3.Bucket("my-bucket");

// Create a bucket object.
new aws.s3.BucketObject("my-bucket-object", {
    bucket: bucket,
    key: "my-bucket-object",
    content: "bucket object!",
});

// Export the name of the bucket.
export const bucketName = bucket.id;
