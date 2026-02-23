module github.com/openclaw/backup-service

go 1.22

require (
	github.com/aws/aws-lambda-go v1.47.0
	github.com/aws/aws-sdk-go-v2 v1.34.0
	github.com/aws/aws-sdk-go-v2/config v1.29.0
	github.com/aws/aws-sdk-go-v2/credentials v1.17.55
	github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue v1.15.22
	github.com/aws/aws-sdk-go-v2/service/dynamodb v1.39.5
	github.com/aws/aws-sdk-go-v2/service/s3 v1.74.0
	github.com/awslabs/aws-lambda-go-api-proxy v0.16.2
	modernc.org/sqlite v1.34.5
)
